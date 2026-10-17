package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/hook"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

func whoami(args []string, s Streams) error {
	fs := newFlagSet("whoami")
	labels := fs.Bool("labels", false, "print this session's labels (key=value,...) instead of its identity")
	why := fs.Bool("why", false, "also say which tool was detected and how")
	asJSON := fs.Bool("json", false, "print the identity, labels and detection as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(s.Out, struct {
			Me     string     `json:"me"`
			Labels msg.Labels `json:"labels"`
			Why    string     `json:"why"`
		}{identity.Me(), identity.Labels(""), identity.Printable(identity.Explain().Why())})
	}
	if *labels {
		fmt.Fprintln(s.Out, identity.Labels("").String())
		return nil
	}
	fmt.Fprintln(s.Out, identity.Me())
	if *why {
		fmt.Fprintln(s.Out, identity.Printable(identity.Explain().Why()))
	}
	return nil
}

func hookCmd(args []string, s Streams) error {
	hook.Run(args, stdinOrNil(s.In), s.Out)
	return nil
}

func send(args []string, s Streams) error { return sendWith(args, s, false) }

// reply is send with --re taken from the last unread message when it is not given.
func reply(args []string, s Streams) error { return sendWith(args, s, true) }

// sendWith is send and reply. reply answers the newest unread message unless --re says which.
func sendWith(args []string, s Streams, reply bool) error {
	name := "send"
	if reply {
		name = "reply"
	}
	fs := newFlagSet(name)
	to := fs.String("to", "", "all | host | host/agent | */agent | host/* | @repo=x,branch=y | @here | @+group (see: aivoicebus usage); default for --re: whoever wrote that message")
	re := fs.String("re", "", "reply to message id (or its first characters, if they name one message)")
	thread := fs.String("thread", "", "thread id (default: new, or the reply's)")
	prio := fs.String("prio", "", strings.Join(msg.PrioNames(), " | ")+" (default ask): fyi is only counted at session start, urgent is shown first")
	kind := fs.String("kind", "", strings.Join(msg.KindNames(), " | ")+": a question to several sessions stops being delivered once someone answers it (an answer needs --re)")
	ttl := fs.String("ttl", "", "how long the message stays relevant, e.g. 90m, 2h, 3d; expired messages are not delivered. Default: 1d for fyi and 3d otherwise when it can reach several sessions, never for one session")
	queue := fs.Bool("queue", false, "send a selector message even if no session seen in the last 24h matches it (it waits for one that starts later)")
	dry := fs.Bool("dry-run", false, "check the message and say who it would reach and what it would cost, without sending")
	force := fs.Bool("force", false, "send despite the guardrails (urgent to many sessions, a long body to many sessions)")
	file := fs.String("file", "", "send a file: it is encrypted here, held for 5 minutes (read once) by the pipe or by a node (--via), and the message carries the link; the reader runs: aivoicebus get ID")
	via := fs.String("via", "", "with --file: pipe (the pipe server, default) or node (this mesh's own nodes hold it); default $AIVOICEBUS_FILE_VIA, else pipe")
	tail := fs.String("tail", "", "point at a live log held by tail server: a stream name, or the word new for a fresh one; feed it with: cmd | aivoicebus tail push --stream NAME")
	keep := fs.Bool("keep", false, "with --file: let several sessions read the file (it is not burned by the first read)")
	wait := fs.Bool("wait", false, "with --file: wait up to 5 minutes and say whether it was read")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	waitReply := fs.Duration("wait-reply", 0, "after sending, wait this long (e.g. 10m) for the answer in the thread and print it: two agents can go back and forth without anyone relaying (exit 6 if nothing came)")
	ackIt := new(bool)
	end := new(bool)
	if reply {
		ackIt = fs.Bool("ack", false, "mark the message replied to as read once the reply is sent")
		end = fs.Bool("end", false, "this reply closes the conversation: the other side's wait stops, and nobody needs to answer it")
	}
	trustRole := fs.Bool("trust-role", false, "use a role alias (@build) that nobody pinned and one session claims; never for urgent or file/log sends. Better: aivoicebus role pin build 'host/claude.*'")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	body := strings.Join(fs.Args(), " ")
	if *end {
		if *kind != "" && *kind != msg.KindEnd {
			return outbox.Usagef("--end and --kind %s say different things; use one", *kind)
		}
		*kind = msg.KindEnd
		if *waitReply > 0 {
			return outbox.Usagef("--end closes the conversation, so there is no reply to wait for")
		}
	}
	if *file != "" || *tail != "" {
		if *file != "" && *tail != "" {
			return errors.New("use --file or --tail, not both")
		}
		c, err := client.FromEnv()
		if err != nil {
			return err
		}
		if reply && *re == "" {
			return outbox.Usagef("reply with --file or --tail needs --re ID: nothing is inferred for an attachment")
		}
		return sendAttachment(c, outbox.Request{To: *to, Re: *re, Thread: *thread, Prio: *prio, TTL: *ttl, Kind: *kind,
			Queue: *queue, Force: *force, Body: body, TrustRole: *trustRole}, sendOpts{file: *file, tail: *tail, via: *via, keep: *keep, wait: *wait, dry: *dry, waitReply: *waitReply}, s)
	}
	if body == "" || body == "-" {
		b, _ := io.ReadAll(s.In)
		body = strings.TrimSpace(string(b))
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	inferred, err := inferRe(c, re, *to, reply, s)
	if err != nil {
		return err
	}
	plan, err := outbox.Prepare(c, outbox.Request{To: *to, Body: body, Re: *re, Thread: *thread, Prio: *prio,
		TTL: *ttl, Kind: *kind, Queue: *queue, Force: *force, TrustRole: *trustRole, TargetSaid: inferred}, s.Err)
	if err != nil {
		return err
	}
	if *dry {
		if *asJSON {
			return writeJSON(s.Out, planView(plan))
		}
		plan.Describe(s.Out)
		return nil
	}
	if inferred {
		// Someone may have written since the target was chosen: it was only safe to guess while it
		// was the one candidate, so look again and refuse rather than answer a different message.
		if replyRaceHook != nil {
			replyRaceHook()
		}
		if m, err := outbox.LastUnread(c, identity.Me()); err != nil || m.ID != plan.Msg.Re {
			if err == nil {
				err = outbox.Usagef("the unread message changed while the reply was being prepared (it was %s, it is now %s): nothing was sent; run it again or pick one with --re ID", plan.Msg.Re, m.ID)
			}
			return err
		}
	}
	if plan.Role != nil { // before the message goes: who the alias came to, in a line a script can read
		roleOut := s.Out
		if *asJSON { // stdout stays one JSON document; the line is still printed before anything is sent
			roleOut = s.Err
		}
		fmt.Fprintln(roleOut, plan.Role.Line())
	}
	for _, l := range plan.CapLines() { // who a capability route resolved to, before anything goes out
		fmt.Fprintln(s.Err, "  "+l)
	}
	out, err := plan.Send(c)
	if err != nil {
		return err
	}
	if *ackIt && plan.Msg.Re != "" { // the reply is out: its message is handled
		_ = c.AckVia(plan.Msg.Re, identity.Me(), msg.ViaCLI, 0)
	}
	if *asJSON {
		return writeJSON(s.Out, sentView{ID: out.ID, Thread: out.Thread, To: view.Name(out.To, msg.MaxIdentity), Re: out.Re, Reach: reachOf(plan), Notes: append([]string{}, plan.Notes...)})
	}
	fmt.Fprintln(s.Out, "sent", out.ID, "thread", out.Thread)
	if len(plan.Notes) > 0 {
		fmt.Fprintln(s.Err, "  "+strings.Join(plan.Notes, "; "))
	}
	fmt.Fprintln(s.Err, "undo: aivoicebus retract "+out.ID) // on stderr with the notes: stdout stays one line a script can read the id from
	if len(plan.Suspect) > 0 {
		fmt.Fprintln(s.Err, "if that went out: aivoicebus retract --leak "+out.ID)
	}
	if *waitReply > 0 {
		me := identity.Me()
		fmt.Fprintf(s.Err, "waiting up to %s for an answer in thread %s...\n", waitReply.String(), out.Thread)
		got, err := waitFor(c, me, tasks.Filter{Thread: out.Thread, Progress: claimProgress(s)}, *waitReply, 3*time.Second)
		if errors.Is(err, tasks.ErrTimeout) {
			fmt.Fprintf(s.Err, "no answer in %s; later: aivoicebus wait --thread %s\n", waitReply.String(), out.Thread)
			return exitError{exitTimeout}
		}
		if err != nil {
			return err
		}
		showArrived(c, me, got, true, s)
	}
	return nil
}

// retractCmd takes a sent message back. The id may come before or after the flags
// (`retract ID --leak` and `retract --leak ID` are the same). With no id it is this session's last
// message and no other: --last is the person's way to say "whatever this machine sent last".
func retractCmd(args []string, s Streams) error {
	const usage = "usage: retract [ID] [--leak] [--dry-run] [--last] [--as IDENTITY]"
	fs := newFlagSet("retract")
	leak := fs.Bool("leak", false, "the message held a credential: say so in the tombstone and the dashboard (rotate the credential; hiding the message does not unsend it)")
	dry := fs.Bool("dry-run", false, "say which message this would take back and what would happen, without writing anything")
	last := fs.Bool("last", false, "with no id, take back the latest message sent from this machine by any session on it, not only this session's own last message (the default); it asks for no id")
	as := fs.String("as", "", "retract as IDENTITY (host/agent), which must be on the machine that sent the message; needs the shared token, for an operator who is not on that machine")
	asJSON := fs.Bool("json", false, "print the outcome as JSON (the id and the report)")
	yes := fs.Bool("yes", false, "retract by an abbreviated id without asking (an abbreviation is only looked up among what this machine sent, at least 6 characters)")
	var id string
	var given bool
	rest := args
	for {
		if err := parseFlags(fs, rest, s); err != nil {
			return err
		}
		a := fs.Args()
		if len(a) == 0 {
			break
		}
		if given {
			return outbox.Usagef("%s", usage)
		}
		id, given, rest = a[0], true, a[1:]
	}
	if given && id == "" { // `retract "$ID"` with $ID unset must not mean "the last message"
		return outbox.Usagef("%s: the id is empty", usage)
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	if given && len(strings.TrimSpace(id)) < 10 { // retract 3fa9c0 names the message by its first characters
		ids := outbox.NewIDResolver(c, identity.Me())
		ids.Say = s.Err
		abbrev := id
		var to string
		var at time.Time
		if id, to, at, err = ids.ResolveSent(abbrev); err != nil {
			return err
		}
		fmt.Fprintf(s.Err, "retract target: %s to %s, sent %s ago\n", id, view.Name(to, 60), outbox.SpanText(time.Since(at).Round(time.Second)))
		if !*dry && !*yes {
			// Taking a message back is not undone by reading again, and an abbreviation is a
			// guess: a person at a terminal confirms it, anything else must say --yes.
			if !isTerminal(s.In) {
				return outbox.Usagef("retracting by an abbreviation needs --yes when there is no terminal to ask on (the target is named above)")
			}
			fmt.Fprint(s.Err, "take it back? [y/N] ")
			line, _ := bufio.NewReader(s.In).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				return outbox.Usagef("not confirmed: nothing was taken back")
			}
		}
	} else if given {
		id = strings.ToLower(strings.TrimSpace(id))
	}
	if !*asJSON {
		return outbox.Retract(c, id, outbox.RetractOptions{Leak: *leak, DryRun: *dry, As: *as, Last: *last}, s.Out)
	}
	var report strings.Builder
	err = outbox.Retract(c, id, outbox.RetractOptions{Leak: *leak, DryRun: *dry, As: *as, Last: *last}, &report)
	if err != nil {
		return err // its kind (not found, ...) decides the exit code, so it is not re-wrapped with the report
	}
	return writeJSON(s.Out, struct {
		ID     string `json:"id,omitempty"`
		DryRun bool   `json:"dry_run"`
		Report string `json:"report"`
	}{id, *dry, strings.TrimSpace(report.String())})
}

func inbox(args []string, s Streams) error {
	fs := newFlagSet("inbox")
	all := fs.Bool("all", false, "include already-acked")
	ack := fs.Bool("ack", false, "mark shown messages as read")
	watch := fs.Duration("watch", 0, "poll interval (e.g. 20s); 0 = once")
	asJSON := fs.Bool("json", false, "print the messages as a JSON array (with --watch: one compact array per poll that has any)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	c.Labels = identity.Labels("")
	for {
		me := identity.Me()
		ms, err := c.Inbox(me, *all)
		if err != nil {
			return err
		}
		if *asJSON && (len(ms) > 0 || *watch == 0) {
			if err := writeMessages(s.Out, ms, *watch > 0); err != nil {
				return err
			}
		}
		if len(ms) > 0 {
			if !*asJSON {
				view.ForAgent(s.Out, ms, 0)
			}
			if *ack {
				for _, m := range ms {
					if m.Retracted != nil { // --all lists a withdrawn message as a tombstone: nobody read it, so it is no read receipt
						continue
					}
					_ = c.AckVia(m.ID, me, msg.ViaInbox, utf8.RuneCountInString(m.Body))
				}
			}
		}
		if *watch == 0 {
			if len(ms) == 0 && !*asJSON {
				fmt.Fprintln(s.Out, "(no unread messages)")
			}
			return nil
		}
		time.Sleep(*watch)
	}
}

func ack(args []string, s Streams) error {
	if wantsHelp(args) {
		fmt.Fprintln(s.Out, "usage: ack ID [ID...]   mark messages as read (an id may be its first 4+ characters if they name one message)")
		return nil
	}
	if len(args) == 0 {
		return nil
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	ids := outbox.NewIDResolver(c, identity.Me())
	ids.Say = s.Err
	full := make([]string, len(args)) // resolved first: an ambiguous one acknowledges nothing
	for i, id := range args {
		if full[i], err = ids.Resolve(id); err != nil {
			return err
		}
	}
	for _, id := range full {
		if err := c.AckVia(id, identity.Me(), msg.ViaCLI, 0); err != nil {
			return err
		}
	}
	return nil
}

func thread(args []string, s Streams) error {
	fs := newFlagSet("thread")
	asJSON := fs.Bool("json", false, "print the messages as a JSON array")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	rest := takeJSON(fs.Args(), asJSON)
	if len(rest) != 1 {
		return outbox.Usagef("usage: thread <id> [--json]")
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	ids := outbox.NewIDResolver(c, identity.Me())
	ids.Say = s.Err
	id, err := ids.Resolve(rest[0])
	if err != nil {
		return err
	}
	ms, err := c.Thread(id)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		return outbox.NotFoundf("no thread with message %s on the nodes (a typo, or it has not reached them yet)", id)
	}
	if *asJSON {
		return writeMessages(s.Out, ms, false)
	}
	view.Thread(s.Out, ms, ms, 0)
	return nil
}

// writeMessages prints ms as a JSON array ([] for none); compact on one line when streaming.
func writeMessages(w io.Writer, ms []msg.Message, compact bool) error {
	ms = view.SanitiseMessages(ms) // JSON is read by programs that print it: C1 and bidi controls go too
	if compact {
		return json.NewEncoder(w).Encode(ms)
	}
	return writeJSON(w, ms)
}

// replyRaceHook runs between choosing a reply's target and re-checking it, for tests.
var replyRaceHook func()

// inferRe fills in --re for a reply that names none, but only when exactly one unread message
// could be answered (see outbox.LastUnread) and no --to was given: a recipient named by hand is
// never threaded under a message nobody chose. It says on stderr whom it will answer.
func inferRe(c *client.Client, re *string, to string, reply bool, s Streams) (bool, error) {
	if !reply || *re != "" {
		return false, nil
	}
	if to != "" {
		return false, outbox.Usagef("reply --to needs --re ID: a reply is threaded under the message it answers, and nothing is inferred when you name the recipient (use send for a new message)")
	}
	m, err := outbox.LastUnread(c, identity.Me())
	if err != nil {
		return false, err
	}
	*re = m.ID
	fmt.Fprintf(s.Err, "replying to %s from %s: %s\n", m.ID, view.Name(m.From, 60), view.OneLine(m.Body, 60))
	return true, nil
}

// sentView is `send --json`'s answer.
type sentView struct {
	ID     string   `json:"id"`
	Thread string   `json:"thread"`
	To     string   `json:"to"`
	Re     string   `json:"re,omitempty"`
	Reach  *int     `json:"reach"` // sessions it reaches; null when the nodes could not say
	Notes  []string `json:"notes"`
}

func reachOf(p outbox.Plan) *int {
	if !p.Known {
		return nil
	}
	return &p.Reach
}

// planView is `send --dry-run --json`'s answer.
func planView(p outbox.Plan) any {
	m := p.Msg
	return struct {
		DryRun   bool     `json:"dry_run"`
		From     string   `json:"from"`
		To       string   `json:"to"`
		Re       string   `json:"re,omitempty"`
		Priority string   `json:"priority"`
		Kind     string   `json:"kind,omitempty"`
		TTL      int64    `json:"ttl_seconds"`
		Expiry   int64    `json:"default_expiry_seconds"`
		Reach    *int     `json:"reach"`
		Context  int      `json:"context_chars"`
		Body     int      `json:"body_chars"`
		Notes    []string `json:"notes"`
	}{true, view.Name(m.From, msg.MaxIdentity), view.Name(m.To, msg.MaxIdentity), m.Re, m.Priority(), m.Kind, m.TTL, int64(p.Expiry / time.Second), reachOf(p), p.Context,
		utf8.RuneCountInString(m.Body), append([]string{}, p.Notes...)}
}

// who lists the sessions the answering node has heard from, with the labels
// each declared. Sessions not seen within --within (default 24h) are hidden, and
// the output says how many, so a stale entry is never mistaken for a live one.
// `who @repo=x` lists only those a message to that selector would reach, which
// previews a send before it goes out.
func who(args []string, s Streams) error {
	fs := newFlagSet("who")
	within := fs.String("within", "24h", "only sessions seen this recently (90m, 24h, 3d)")
	all := fs.Bool("all", false, "include sessions not seen within --within")
	asJSON := fs.Bool("json", false, "print the sessions as JSON (with --tools: the same list; the capabilities are in the labels)")
	tools := fs.Bool("tools", false, "group the sessions by tool and repo instead of listing them")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	var span time.Duration
	if !*all {
		var err error
		if span, err = msg.ParseSpan(*within); err != nil {
			return outbox.Usagef("--within: %v", err)
		}
	}
	var sel msg.Selector
	if a := fs.Args(); len(a) > 0 {
		if role, ok := msg.IsRoleAlias(strings.ToLower(a[0])); ok { // @build: who holds the role, and who a send would reach
			c, err := client.FromEnv()
			if err != nil {
				return err
			}
			outbox.WhoRole(s.Out, c, role, identity.Me())
			return nil
		}
		to, err := outbox.ExpandTarget(a[0]) // @here and @+group mean here what they mean to send
		if err != nil {
			return err
		}
		if sel, err = msg.ParseSelector(to); err != nil {
			return fmt.Errorf("bad selector %q: %v", a[0], err)
		}
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-span)
	hidden, shown := 0, 0
	rows := []msg.RosterEntry{} // what --json prints and --tools groups
	show := func(me string, seen time.Time, labels msg.Labels, title string) {
		if !*all && seen.Before(cutoff) {
			hidden++
			return
		}
		shown++
		if *asJSON || *tools {
			rows = append(rows, msg.RosterEntry{Me: me, Seen: seen, Labels: labels, Title: title})
			return
		}
		line := fmt.Sprintf("%-32s last seen %s", view.Name(me, 64), seen.Local().Format("01-02 15:04"))
		if t := view.Title(title); t != "" {
			line += "  " + t
		}
		if l := labels.String(); l != "" {
			line += "  " + view.Name(l, 300)
		}
		fmt.Fprintln(s.Out, line)
	}

	roster, err := c.Roster()
	switch {
	case err == nil:
		for _, e := range roster {
			if sel == nil || sel.Match(e.Labels) {
				show(e.Me, e.Seen, e.Labels, e.Title)
			}
		}
	case client.IsNotFound(err):
		// A node without a roster: list identities without labels.
		if sel != nil {
			return errors.New("this node does not report labels, so a selector cannot be previewed")
		}
		w, err := c.Who()
		if err != nil {
			return err
		}
		for _, k := range slices.Sorted(maps.Keys(w)) {
			show(k, w[k], nil, "")
		}
	default:
		return err
	}

	switch {
	case *asJSON:
		return writeJSON(s.Out, struct {
			Sessions []msg.RosterEntry `json:"sessions"`
			Hidden   int               `json:"hidden"` // seen, but not within --within
		}{view.SanitiseRoster(rows), hidden})
	case *tools:
		printToolGroups(s.Out, rows)
	}
	recent := "in the last " + *within
	switch {
	case sel != nil && shown == 0 && hidden == 0:
		fmt.Fprintf(s.Out, "(no session this node knows of matches %s)\n", sel)
	case sel != nil && shown == 0:
		fmt.Fprintf(s.Out, "(no session seen %s matches %s; %d older one(s) hidden, --all shows them)\n", recent, sel, hidden)
	case hidden > 0:
		fmt.Fprintf(s.Out, "(%d session(s) not seen %s hidden; --all shows them)\n", hidden, recent)
	}
	return nil
}
