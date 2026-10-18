package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/attach"
	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Handing a machine its own token (deploy/machine-token.sh made it) is one exchange between two
// agents, with a person approving the one step that changes a credential:
//
//  1. `token hand HOST`, on the machine that deploys, checks that the nodes take the token as
//     HOST's own, and sends HOST a task.
//  2. A session there runs `token install ID`: it takes the task, asks its person to approve (a
//     terminal, the approver key or the phone), and answers ready.
//  3. hand puts the token in a one-time, end-to-end-encrypted file (held by a node by default, so
//     nothing leaves the mesh's machines) and sends only the link, in the thread, to that session.
//  4. install fetches it, asks the nodes whether it is a machine token bound to this machine, and
//     only then replaces the token file, putting the old one back if anything after that fails. It
//     restarts the worker and answers with the new token.
//  5. hand asks the nodes which token sent that answer: success is the nodes' word, not the peer's.
//
// The token is in no message, no argument and no log; the link's key is shown to no agent (a
// message renders it as a summary).

const (
	tokenHandTag      = "[token hand v1]"
	tokenReadyTag     = "[token ready]"
	tokenLinkTag      = "[token link]"
	tokenInstalledTag = "[token installed]"
	tokenFailedTag    = "[token install failed]"
)

const tokenUsage = `usage: aivoicebus token hand HOST [--to SESSION] [--via node|pipe] [--wait 30m] [--json]
       aivoicebus token install ID|LINK [--wait 30m] [--json]

Hand a machine its own token (made by deploy/machine-token.sh add HOST) without anyone copying it:
'hand' runs where the token was made and sends HOST a task; a session there runs 'install' with
the task's id, a person approves, and the token goes over as a one-time end-to-end-encrypted file.
'install' checks with the nodes that it is this machine's own before it replaces the token file,
and 'hand' checks with the nodes that the answer came with it.`

var reTokenHost = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func tokenCmd(args []string, s Streams) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(s.Err, tokenUsage)
		if len(args) > 0 {
			return exitError{0}
		}
		return exitError{2}
	}
	switch args[0] {
	case "hand":
		return tokenHand(args[1:], s)
	case "install":
		return tokenInstall(args[1:], s)
	}
	fmt.Fprintln(s.Err, tokenUsage)
	return exitError{2}
}

// tagged reports whether body's first line starts with tag, and returns the rest of that line.
func tagged(body, tag string) (string, bool) {
	line, _, _ := strings.Cut(body, "\n")
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), tag)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// machineTokensDir is where deploy/machine-token.sh keeps the tokens it made.
func machineTokensDir() string {
	if d := os.Getenv("AIVOICEBUS_MACHINE_TOKENS_DIR"); d != "" {
		return d
	}
	return filepath.Join(config.Dir(), "machine-tokens")
}

// cleanToken checks that raw is one token as a node's token file holds it: one line, no spaces,
// no name: prefix, at least server.MinTokenLen characters. It never says what it holds.
func cleanToken(raw []byte) (string, error) {
	t := strings.TrimSpace(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	switch {
	case t == "":
		return "", errors.New("it is empty")
	case strings.ContainsAny(t, " \t\r\n:"):
		return "", errors.New("it is not a single token (it has a space, a line break or a name: prefix)")
	case len(t) < server.MinTokenLen:
		return "", fmt.Errorf("it is shorter than %d characters, so no node accepts it", server.MinTokenLen)
	case len(t) > 4096:
		return "", errors.New("it is far longer than any token")
	}
	return t, nil
}

// --- the deploy side ---------------------------------------------------------------------------

type handResult struct {
	Host      string `json:"host"`
	Task      string `json:"task,omitempty"`
	Session   string `json:"session,omitempty"` // the session that took it
	Approved  string `json:"approved_by,omitempty"`
	Installed bool   `json:"installed"`
	Answer    string `json:"answer_id,omitempty"`
	Said      string `json:"said,omitempty"`  // the receiver's own account, untrusted
	Proven    bool   `json:"proven"`          // a node saw the answer sent with HOST's machine token
	Proof     string `json:"proof,omitempty"` // what the nodes said
	Error     string `json:"error,omitempty"`
}

func tokenHand(args []string, s Streams) error {
	fs := newFlagSet("token hand")
	to := fs.String("to", "", "send the task to this one session on HOST, not to any session there")
	via := fs.String("via", "node", "where the encrypted file is held: node (one of the mesh's nodes) or pipe")
	wait := fs.Duration("wait", 30*time.Minute, "how long to wait for a session to take it, the person to approve and the install")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	var host string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		host, args = args[0], args[1:]
	}
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if rest := fs.Args(); host == "" && len(rest) == 1 {
		host = rest[0]
	} else if len(rest) > 0 || host == "" {
		return outbox.Usagef("usage: token hand HOST [--to SESSION] [--via node|pipe] [--wait 30m] [--json]")
	}
	host = strings.ToLower(host)
	if !reTokenHost.MatchString(host) {
		return outbox.Usagef("%q is not a machine name (the part of an identity before the slash: lowercase letters, digits, . _ -)", host)
	}
	if *to != "" && (!msg.ValidIdentity(*to) || msg.HostOf(*to) != host || strings.ContainsAny(*to, "*@,=")) {
		return outbox.Usagef("--to names one session on %s (%s/agent), not %q", host, host, *to)
	}
	me := identity.Me()
	if msg.HostOf(me) == host {
		return outbox.Usagef("%s is this machine: the machine you deploy from keeps the shared token, its own tokens are for the others", host)
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	res := handResult{Host: host}
	err = handOver(c, me, host, *to, *via, *wait, &res, s)
	if err != nil {
		res.Error = err.Error()
	}
	if *asJSON {
		if werr := writeJSON(s.Out, res); werr != nil {
			return werr
		}
		if err != nil {
			return exitError{exitCode(err)}
		}
		if !res.Proven {
			return exitError{1}
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !res.Proven {
		fmt.Fprintf(s.Out, "%s says it installed its token, but the nodes do not confirm it: %s\n", host, res.Proof)
		fmt.Fprintf(s.Out, "  it said: %s\n", attach.Sanitize(res.Said))
		return exitError{1}
	}
	fmt.Fprintf(s.Out, "%s now uses its own machine token: %s\n", host, res.Proof)
	fmt.Fprintf(s.Out, "  %s said: %s\n", view.Name(res.Session, msg.MaxIdentity), attach.Sanitize(res.Said))
	return nil
}

// handOver is the deploy side of the exchange; res says how far it got.
func handOver(c *client.Client, me, host, to, via string, wait time.Duration, res *handResult, s Streams) error {
	path := filepath.Join(machineTokensDir(), host)
	if fi, err := os.Lstat(path); err != nil {
		return fmt.Errorf("%s has no machine token here (%s): make it first with deploy/machine-token.sh add %s --apply", host, path, host)
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; not reading it", path)
	} else if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by other users (mode %04o): chmod 600 it first", path, fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tok, err := cleanToken(raw)
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	// The nodes must take it as HOST's own before anyone is bothered.
	info, err := (&client.Client{URLs: c.URLs, Token: tok}).TokenInfo()
	switch {
	case client.IsRejected(err):
		return fmt.Errorf("the nodes do not accept the token in %s: give it to them first with deploy/machine-token.sh add %s --apply", path, host)
	case err != nil:
		return fmt.Errorf("could not ask the nodes about the token: %v", err)
	case info.Observer || info.Host != host:
		return fmt.Errorf("the nodes say the token in %s is not a machine token bound to %s (bound to %q); not handing it over", path, host, info.Host)
	}
	fs, err := fileStore(c, via)
	if err != nil {
		return err
	}
	target := host
	if to != "" {
		target = to
	}
	body := fmt.Sprintf("%s %s\n%s hands %s its own machine token, to use instead of the mesh's shared token.\n"+
		"A session on %s takes this with: aivoicebus token install <this task's id>\n"+
		"That takes the task, asks the person on %s to approve, receives the token as a one-time end-to-end-encrypted file, "+
		"checks with the nodes that it is %s's own, and only then replaces the token file (the old one is put back if anything fails). "+
		"The token itself is in no message.", tokenHandTag, host, me, host, host, host, host)
	plan, err := outbox.Prepare(c, outbox.Request{To: target, Body: body, Kind: msg.KindTask, Prio: msg.PrioUrgent, TTL: "1h", Queue: true, TargetSaid: true}, s.Err)
	if err != nil {
		return err
	}
	task, err := plan.Send(c)
	if err != nil {
		return err
	}
	res.Task = task.ID
	fmt.Fprintf(s.Err, "sent task %s to %s: a session there runs `aivoicebus token install %s`; waiting up to %s\n", task.ID, target, task.ID, wait)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	deadline := time.Now().Add(wait)
	from := func(m msg.Message) bool { // a session on HOST, signed by HOST's machine key
		if to != "" && m.From != to {
			return false
		}
		return msg.HostOf(m.From) == host && m.Verified == msg.VerifiedSigned
	}
	progress := func(m msg.Message) {
		if from(m) {
			fmt.Fprintf(s.Err, "%s took it\n", view.Name(m.From, msg.MaxIdentity))
		}
	}

	// ready, from a session on HOST
	var ready msg.Message
	for ready.ID == "" {
		got, err := tasks.Wait(ctx, c, me, tasks.Filter{Thread: task.Thread, Progress: progress}, time.Until(deadline), 3*time.Second)
		if err != nil {
			return fmt.Errorf("no session on %s said ready within %s (task %s)", host, wait, task.ID)
		}
		for _, m := range got {
			_ = c.AckVia(m.ID, me, msg.ViaCLI, 0)
			if _, ok := tagged(m.Body, tokenFailedTag); ok && from(m) {
				res.Session, res.Said = m.From, view.OneLine(m.Body, 400)
				return fmt.Errorf("%s did not install it: %s", m.From, view.OneLine(m.Body, 400))
			}
			if h, ok := tagged(m.Body, tokenReadyTag); ok && h == host {
				if !from(m) {
					fmt.Fprintf(s.Err, "ignored a ready from %s: not a session on %s signed with its machine key\n", view.Name(m.From, msg.MaxIdentity), host)
					continue
				}
				ready = m
				break
			}
		}
	}
	res.Session = ready.From
	if _, rest, ok := strings.Cut(ready.Body, "approved by the person ("); ok { // the receiver's word, for the record
		res.Approved, _, _ = strings.Cut(rest, ")")
		res.Approved = view.OneLine(res.Approved, 40)
	}
	fmt.Fprintf(s.Err, "%s is ready; sending the link to it only\n", view.Name(ready.From, msg.MaxIdentity))

	// the token, as a one-time file; only the link goes in the thread, to that session
	l, _, err := fs.Put(ctx, []byte(tok+"\n"), true)
	if err != nil {
		return fmt.Errorf("could not store the encrypted token: %v", err)
	}
	l.Name, l.Size, l.SHA256 = "machine-token", 0, "" // a checksum of a token is a way to test guesses: none
	linkBody := tokenLinkTag + " " + host + "\nthe machine token of " + host + ": one read, five minutes, end-to-end encrypted\n" + l.Line()
	lp, err := outbox.Prepare(c, outbox.Request{To: ready.From, Re: ready.ID, Thread: task.Thread, Body: linkBody, File: true, TargetSaid: true}, s.Err)
	if err != nil {
		return err
	}
	link, err := lp.Send(c)
	if err != nil {
		return err
	}

	// the answer, from that session
	var answer msg.Message
	installed := false
	for answer.ID == "" {
		got, err := tasks.Wait(ctx, c, me, tasks.Filter{Thread: task.Thread, From: ready.From}, time.Until(deadline), 3*time.Second)
		if err != nil {
			return fmt.Errorf("%s did not answer within %s; the link (message %s) is good for five minutes, then nothing can read it", ready.From, wait, link.ID)
		}
		for _, m := range got {
			_ = c.AckVia(m.ID, me, msg.ViaCLI, 0)
			if m.From != ready.From || m.Verified != msg.VerifiedSigned {
				continue
			}
			if h, ok := tagged(m.Body, tokenInstalledTag); ok && h == host {
				answer, installed = m, true
				break
			}
			if _, ok := tagged(m.Body, tokenFailedTag); ok {
				answer = m
				break
			}
		}
	}
	res.Answer, res.Said = answer.ID, view.OneLine(attach.Sanitize(answer.Body), 400)
	if !installed {
		return fmt.Errorf("%s did not install it: %s", ready.From, view.OneLine(answer.Body, 400))
	}
	res.Installed = true

	// The proof: a node saw that answer arrive with HOST's machine token.
	res.Proven, res.Proof = proveSent(c, host, info.Name, answer.ID, 20*time.Second)
	return nil
}

// proveSent asks every node, for up to within, whether message id was sent as host with host's
// machine token (name): the nodes' own record, not the peer's word.
func proveSent(c *client.Client, host, name, id string, within time.Duration) (bool, string) {
	deadline := time.Now().Add(within)
	last := "no node answered"
	for {
		all, err := c.TokenSeen(host)
		switch {
		case client.IsNotFound(err):
			return false, "the nodes run a release that does not record which token a machine uses (token-seen); update them to prove it"
		case err != nil:
			last = err.Error()
		}
		for _, e := range all {
			if e.SendID != id {
				continue
			}
			if e.SendKind == "machine" && e.SendName == name {
				return true, fmt.Sprintf("node %s saw its answer %s sent with the machine token %q at %s", e.Node, id, name, e.SendAt.Local().Format("15:04:05"))
			}
			return false, fmt.Sprintf("node %s saw its answer %s sent with the %s token %q, not its own", e.Node, id, e.SendKind, e.SendName)
		}
		if len(all) > 0 {
			var said []string
			for _, e := range all {
				if e.SendID != "" {
					said = append(said, fmt.Sprintf("%s: last send %s with the %s token", e.Node, e.SendID, e.SendKind))
				} else {
					said = append(said, e.Node+": no send seen")
				}
			}
			last = "no node of this machine saw the answer arrive (" + strings.Join(said, "; ") + ")"
		}
		if time.Now().After(deadline) {
			return false, last
		}
		time.Sleep(time.Second)
	}
}

// --- the machine's side ------------------------------------------------------------------------

type installResult struct {
	Installed bool   `json:"installed"`
	Host      string `json:"host"`
	Kind      string `json:"kind,omitempty"` // of the new token, as the nodes say
	Name      string `json:"name,omitempty"` // its name in the nodes' token file
	Previous  string `json:"previous_kind,omitempty"`
	Path      string `json:"path,omitempty"`
	Approved  string `json:"approved_by,omitempty"` // terminal, approver key or phone
	Worker    string `json:"worker,omitempty"`
	Task      string `json:"task,omitempty"`
	From      string `json:"from,omitempty"`      // who handed it over
	Answer    string `json:"answer_id,omitempty"` // the answer sent to them, with the new token
	Restored  bool   `json:"restored,omitempty"`  // the old token was put back after a failure
	Error     string `json:"error,omitempty"`
}

// restartWorker restarts the worker service if one is installed (a variable so tests need no launchd).
var restartWorker = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "not installed"
	}
	file, _ := svcPaths(home)
	if _, err := os.Stat(file); err != nil {
		return "not installed"
	}
	if err := svcStart(file); err != nil {
		return "restart failed: " + err.Error()
	}
	return "restarted"
}

// approveInstall asks the person whether to replace this machine's token, and says how they said
// yes: at a terminal (y/N), else through the approver key (a signed `aivoicebus approve ID` from any
// terminal on this machine) or the phone's buttons. An agent's session has no terminal, and cannot
// sign with the approver key without its passphrase. A variable so tests can answer it.
var approveInstall = func(c *client.Client, me, what string, within time.Duration, s Streams) (string, error) {
	yes, err := askYesNo(s, what+"\nReplace this machine's token with it? [y/N] ")
	switch {
	case err == nil && yes:
		return "terminal", nil
	case err == nil:
		return "", errors.New("the person said no; nothing was changed")
	case !errors.Is(err, errNoTerminal):
		return "", err
	}
	key, kerr := config.ApproverPublic()
	nu := notifyURL()
	if kerr != nil && nu == "" {
		return "", errors.New("token install changes this machine's credential, so a person approves it, and there is no terminal here: run it in a terminal, or set up the approver key once (aivoicebus approver init, in a terminal) so the person can approve it from any terminal; nothing was changed")
	}
	text := "[approval needed] " + what + "\n\nA person approves in a terminal on this machine, with the approver passphrase: aivoicebus approve <this id>  (or: aivoicebus deny <this id>). It waits " + within.String() + ". An agent must not run either."
	code := ""
	if nu != "" && len(c.URLs) > 0 {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		code = hex.EncodeToString(b) // only in the notification; the request carries its hash
		text += "\nOr on the phone: the Approve and Deny buttons of the notification.\n" + msg.PhoneCheckLine(code)
	}
	plan, err := outbox.Prepare(c, outbox.Request{To: msg.HumanOf(me), Body: text, Prio: msg.PrioUrgent, Queue: true, TargetSaid: true}, io.Discard)
	if err != nil {
		return "", err
	}
	req, err := plan.Send(c)
	if err != nil {
		return "", err
	}
	if nu != "" {
		actions := ""
		if code != "" {
			u := strings.TrimRight(c.URLs[0], "/") + "/v1/decide?id=" + req.ID + "&code=" + code + "&say="
			actions = "http, Approve, " + u + "approve, method=POST, clear=true; http, Deny, " + u + "deny, method=POST, clear=true"
		}
		_ = tasks.NotifyActions(nu, "aivoicebus: approval needed", "replace the token of "+msg.HostOf(me)+"? "+view.OneLine(what, 200), actions)
	}
	how := "in a terminal on this machine: aivoicebus approve " + req.ID
	if kerr != nil {
		how = "on the phone (there is no approver key here)"
	} else if code != "" {
		how += ", or on the phone"
	}
	fmt.Fprintf(s.Err, "approval needed (%s): ask the person to approve it %s. Waiting up to %s.\n", req.ID, how, within)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	deadline := time.Now().Add(within)
	for time.Until(deadline) > time.Second {
		got, err := tasks.Wait(ctx, c, me, tasks.Filter{Thread: req.Thread}, time.Until(deadline), 3*time.Second)
		if err != nil {
			break
		}
		for _, a := range got {
			_ = c.AckVia(a.ID, me, msg.ViaCLI, 0)
			if say, got, ok := msg.ParsePhoneDecision(a.Body); ok && code != "" && subtle.ConstantTimeCompare([]byte(got), []byte(code)) == 1 {
				if say == "approve" {
					return "phone", nil
				}
				return "", errors.New("denied on the phone; nothing was changed")
			}
			if kerr != nil || msg.HostOf(a.From) != msg.HostOf(me) || a.Verified != msg.VerifiedSigned {
				continue // only a decision made on this machine, signed with its machine key...
			}
			word, ok := msg.CheckApproval(a.Body, req.ID, me, key)
			if !ok {
				continue // ...and with the approver key: a bare "approve" is not a person's decision
			}
			if word == "approve" {
				return "approver key", nil
			}
			return "", errors.New("the person denied it; nothing was changed")
		}
	}
	return "", fmt.Errorf("no approval within %s; nothing was changed", within)
}

func tokenInstall(args []string, s Streams) error {
	fs := newFlagSet("token install")
	wait := fs.Duration("wait", 30*time.Minute, "how long to wait for the person's approval, then for the link")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	var ref string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ref, args = args[0], args[1:]
	}
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if rest := fs.Args(); ref == "" && len(rest) == 1 {
		ref = rest[0]
	} else if len(rest) > 0 || ref == "" {
		return outbox.Usagef("usage: token install ID|LINK [--wait 30m] [--json]   (ID: the [token hand] task, or the message with the link)")
	}
	me := identity.Me()
	res := installResult{Host: msg.HostOf(me)}
	err := installExchange(ref, me, *wait, &res, s)
	if err != nil {
		res.Error = err.Error()
	}
	if *asJSON {
		if werr := writeJSON(s.Out, res); werr != nil {
			return werr
		}
		if err != nil {
			return exitError{exitCode(err)}
		}
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "installed: this machine (%s) now uses its own machine token %q, checked with the nodes (was: the %s token). %s\n", res.Host, res.Name, res.Previous, res.Path)
	fmt.Fprintf(s.Out, "worker: %s\n", res.Worker)
	if res.Answer != "" {
		fmt.Fprintf(s.Out, "told %s (message %s), with the new token\n", res.From, res.Answer)
	}
	return nil
}

// installExchange is the machine's side: ref is the hand task, a message with the link, or the link.
func installExchange(ref, me string, wait time.Duration, res *installResult, s Streams) error {
	host := msg.HostOf(me)
	if os.Getenv("AIVOICEBUS_TOKEN") != "" {
		return errors.New("AIVOICEBUS_TOKEN is set here (in the environment or ~/.config/aivoicebus/env), so the token file is not what this machine uses: remove it from there first; nothing was changed")
	}
	path := config.TokenPath("")
	if path == "" {
		return errors.New("no home directory to keep the token in")
	}
	res.Path = path
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	res.Previous = "unknown"
	if kind, _, err := c.TokenKind(); err == nil && kind != "" {
		res.Previous = kind
	}

	// what is handed over, and by whom
	var body string            // the message with the [attach] line
	var task, from msg.Message // the task, and the message the answer goes to
	if strings.Contains(ref, "://") {
		body = "[attach] " + strings.TrimSpace(ref)
	} else {
		ids := outbox.NewIDResolver(c, me)
		ids.Say = s.Err
		id, err := ids.Resolve(ref)
		if err != nil {
			return err
		}
		ms, err := c.Thread(id)
		if err != nil {
			return err
		}
		var m msg.Message
		for _, x := range ms {
			if x.ID == id {
				m = x
			}
		}
		if m.ID == "" {
			return outbox.NotFoundf("message %s was not found on the nodes", id)
		}
		if h, ok := tagged(m.Body, tokenHandTag); ok {
			if m.Kind != msg.KindTask {
				return outbox.Usagef("%s is not a task", id)
			}
			if h != host {
				return fmt.Errorf("task %s hands %s its token, and this machine is %s; nothing was changed", id, h, host)
			}
			task, from = m, m
		} else if _, found, _ := attach.Find(m.Body); found {
			body, from = m.Body, m
		} else {
			return outbox.Usagef("%s is neither a [token hand] task nor a message with a link", id)
		}
	}
	if from.ID != "" {
		res.From = from.From
		if from.Verified != msg.VerifiedSigned {
			return fmt.Errorf("message %s is not signed by %s's machine key (%s); not taking a credential from it", from.ID, msg.HostOf(from.From), from.Verified)
		}
	}
	if task.ID != "" {
		res.Task = task.ID
		cl, err := tasks.Claim(c, me, task.ID, "installing the token "+task.From+" hands this machine", 2*time.Second, s.Err)
		if err != nil {
			return err
		}
		if !cl.Mine {
			return fmt.Errorf("task %s is taken by %s: it installs the token, not this session", task.ID, cl.Owner.From)
		}
	}

	// the person approves before anything is fetched, so the link's five minutes are not spent waiting
	what := fmt.Sprintf("Replace the token of %s (%s, now the %s token) with the machine token", host, path, res.Previous)
	if from.ID != "" {
		what += " that " + view.Name(from.From, msg.MaxIdentity) + " hands it"
	}
	what += "? It is checked with the nodes first: only a machine token bound to " + host + " is installed."
	how, err := approveInstall(c, me, what, wait, s)
	if err != nil {
		if task.ID != "" {
			tokenAnswer(c, me, task, task.ID, tokenFailedTag+" "+host+"\n"+err.Error(), s)
		}
		return err
	}
	res.Approved = how

	answerTo := from.ID
	if task.ID != "" {
		ready := fmt.Sprintf("%s %s\napproved by the person (%s); waiting for the link, up to %s", tokenReadyTag, host, how, wait)
		readyMsg, err := tokenSay(c, me, task, task.ID, ready, s)
		if err != nil {
			return err
		}
		fmt.Fprintf(s.Err, "approved (%s); told %s, waiting for the link\n", how, task.From)
		link, err := waitLink(c, me, task, readyMsg, wait)
		if err != nil {
			tokenAnswer(c, me, task, readyMsg.ID, tokenFailedTag+" "+host+"\n"+err.Error(), s)
			return err
		}
		body, answerTo = link.Body, link.ID
	}

	name, err := fetchAndInstall(c, body, host, path, res)
	if err != nil {
		if task.ID != "" || from.ID != "" {
			tokenAnswer(c, me, from, answerTo, tokenFailedTag+" "+host+"\n"+err.Error(), s)
		}
		return err
	}
	res.Installed, res.Kind, res.Name = true, "machine", name
	config.Audit(me, "token install", "the machine token "+name+" for "+host+" from "+res.From+", approved by "+how)
	res.Worker = restartWorker()
	if from.ID != "" {
		nc, err := client.FromEnv() // the new token, read back from the file
		if err != nil {
			return err
		}
		said := fmt.Sprintf("%s %s\nthis machine now uses its own machine token (%s), checked with the nodes; this answer is sent with it. worker: %s", tokenInstalledTag, host, name, res.Worker)
		out, err := tokenSay(nc, me, from, answerTo, said, s)
		if err != nil {
			return fmt.Errorf("installed, but could not tell %s: %v", from.From, err)
		}
		res.Answer = out.ID
	}
	return nil
}

// waitLink waits for the link from whoever sent task, in its thread.
func waitLink(c *client.Client, me string, task, ready msg.Message, wait time.Duration) (msg.Message, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	deadline := time.Now().Add(wait)
	for time.Until(deadline) > time.Second {
		got, err := tasks.Wait(ctx, c, me, tasks.Filter{Thread: task.Thread, From: task.From}, time.Until(deadline), 3*time.Second)
		if err != nil {
			break
		}
		for _, m := range got {
			_ = c.AckVia(m.ID, me, msg.ViaCLI, 0)
			if m.From != task.From || m.Verified != msg.VerifiedSigned {
				continue
			}
			if _, ok := tagged(m.Body, tokenLinkTag); !ok {
				continue
			}
			if _, found, _ := attach.Find(m.Body); found {
				return m, nil
			}
		}
	}
	return msg.Message{}, fmt.Errorf("no link from %s within %s (after ready %s)", task.From, wait, ready.ID)
}

// fetchAndInstall fetches and decrypts the token, has the nodes confirm it is a machine token bound
// to host, and only then replaces the token file at path, putting the old one back if the nodes do
// not take it from the file. It returns the token's name in the nodes' token file.
func fetchAndInstall(c *client.Client, body, host, path string, res *installResult) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	data, _, _, err := attach.FetchFile(ctx, body, c.Token)
	switch {
	case errors.Is(err, attach.ErrGone):
		return "", errors.New("the link has expired or was read already (it is good for one read, for five minutes): ask for it again; nothing was changed")
	case err != nil:
		return "", fmt.Errorf("could not fetch the token: %v; nothing was changed", err)
	}
	tok, err := cleanToken(data)
	if err != nil {
		return "", fmt.Errorf("what was handed over is not a token: %v; nothing was changed", err)
	}
	if err := checkMachineToken(c.URLs, tok, host); err != nil {
		return "", err
	}
	info, _ := (&client.Client{URLs: c.URLs, Token: tok}).TokenInfo()

	real, err := filepath.EvalSymlinks(path) // a token kept in a password store's directory stays there
	if err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		real = path
	}
	old, oldErr := os.ReadFile(real)
	if oldErr == nil && strings.TrimSpace(string(old)) == tok {
		return info.Name, nil // installed already: nothing to change
	}
	if err := writeAtomic(real, []byte(tok+"\n")); err != nil {
		return "", fmt.Errorf("could not write %s: %v; nothing was changed", real, err)
	}
	// read back through the same path every command uses, and asked of the nodes again
	check := func() error {
		if got := config.Token(""); got != tok {
			return errors.New("the token file does not read back as the new token")
		}
		nc, err := client.FromEnv()
		if err != nil {
			return err
		}
		return checkMachineToken(nc.URLs, nc.Token, host)
	}
	if err := check(); err != nil {
		var rerr error
		if oldErr == nil {
			rerr = writeAtomic(real, old)
		} else {
			rerr = os.Remove(real)
		}
		if rerr != nil {
			return "", fmt.Errorf("after the switch: %v, and the old token could not be put back: %v", err, rerr)
		}
		res.Restored = true
		return "", fmt.Errorf("after the switch: %v; the old token is back in place", err)
	}
	return info.Name, nil
}

// checkMachineToken asks the nodes whether tok is a machine token bound to host.
func checkMachineToken(urls []string, tok, host string) error {
	info, err := (&client.Client{URLs: urls, Token: tok}).TokenInfo()
	switch {
	case client.IsRejected(err):
		return errors.New("the nodes do not accept it; nothing was changed")
	case client.IsNotFound(err):
		return errors.New("the nodes run a release too old to say what kind of token it is; nothing was changed")
	case err != nil:
		return fmt.Errorf("could not ask the nodes about it: %v; nothing was changed", err)
	case info.Observer:
		return errors.New("it is the observer token, not a machine token; nothing was changed")
	case info.Host == "":
		return errors.New("it is a shared token, not a machine token bound to this machine; nothing was changed")
	case info.Host != host:
		return fmt.Errorf("it is the machine token of %s, and this machine is %s; nothing was changed", info.Host, host)
	}
	return nil
}

// writeAtomic replaces path with data (mode 0600) through a file beside it, so a reader sees the old
// token or the new one, never a part of either.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// tokenSay replies to to (in its thread) with body.
func tokenSay(c *client.Client, me string, to msg.Message, re, body string, s Streams) (msg.Message, error) {
	plan, err := outbox.Prepare(c, outbox.Request{To: to.From, Re: re, Thread: to.Thread, Body: body, Prio: msg.PrioUrgent, TargetSaid: true}, s.Err)
	if err != nil {
		return msg.Message{}, err
	}
	return plan.Send(c)
}

// tokenAnswer tells the sender how it failed; a failure to tell them is only said here.
func tokenAnswer(c *client.Client, me string, to msg.Message, re, body string, s Streams) {
	if _, err := tokenSay(c, me, to, re, body, s); err != nil {
		fmt.Fprintf(s.Err, "could not tell %s: %v\n", to.From, err)
	}
}
