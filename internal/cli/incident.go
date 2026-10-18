package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/credstore"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/secrets"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

const incidentUsage = `usage: aivoicebus incident WHAT [--msg ID]... [--apply] [--deploy DIR] [--retire]

WHAT is the credential that leaked:
  shared          the mesh's shared token (run this on the machine you deploy from)
  machine HOST    the machine token of HOST (deploy/machine-token.sh made it)
  observer        the read-only observer token of the dashboard
  secret NAME     a credential saved with 'aivoicebus secret save NAME'
  message ID      a message held a credential and you do not know which (or it is not one of these)

Without --apply it only plans: which deploy script revokes or rotates the credential (and its dry
run), which messages held it (--msg, and every message on the nodes whose text holds its value),
and who read them and when. --apply does it, in that order, after you type "incident" at a
terminal: the credential first, then the messages are retracted as leaks (the nodes tell everyone
who read them), then one line in this machine's audit log. It is the person's command: it refuses
in an agent's session and without a terminal. No secret is ever printed.`

// incidentPlan is everything one leak response will do, worked out before anything is done.
type incidentPlan struct {
	what, name string   // the kind given on the command line, and its HOST/NAME/ID
	label      string   // the credential in words
	values     []string // its value(s): only matched against and masked, never printed
	steps      []incidentStep
	manual     []string // what only a person can do; done before --apply goes on
	msgs       []leakedMsg
	notes      []string
	deployDir  string
}

// incidentStep is one deploy script run: dry run in the plan, with --apply once confirmed.
type incidentStep struct {
	script string
	args   []string
	why    string
}

type leakedMsg struct {
	m     msg.Message
	how   string // "given" or "found in the log"
	rec   *msg.Receipts
	audit []config.AuditEntry
}

var incidentHost = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`) // what deploy/machine-token.sh takes

func incidentCmd(args []string, s Streams) error {
	if len(args) == 0 || wantsHelp(args) {
		fmt.Fprintln(s.Err, incidentUsage)
		if len(args) > 0 {
			return exitError{0}
		}
		return exitError{2}
	}
	what := args[0]
	rest := args[1:]
	var name string
	switch what {
	case "machine", "secret", "message":
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return outbox.Usagef("%s", incidentUsage)
		}
		name, rest = rest[0], rest[1:]
	case "shared", "observer":
	default:
		return outbox.Usagef("%s", incidentUsage)
	}
	fs := newFlagSet("incident")
	var ids []string
	fs.Func("msg", "a message that held it (repeat for more)", func(v string) error { ids = append(ids, v); return nil })
	apply := fs.Bool("apply", false, "do it, after confirming at a terminal (default: plan only)")
	deploy := fs.String("deploy", "", "the deploy/ directory of an aivoicebus checkout (default $AIVOICEBUS_DEPLOY_DIR, else ./deploy)")
	retire := fs.Bool("retire", false, "shared: also retire the old token now (machines not yet moved to the new one stop working)")
	if err := parseFlags(fs, rest, s); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return outbox.Usagef("%s", incidentUsage)
	}
	if *apply {
		// Refused before anything is read: an agent must not even get as far as the question.
		if err := personOnly("incident --apply"); err != nil {
			return err
		}
		if d := identity.Implicit(); d.Tool != "" { // an agent that did not say so: its tool is above this process
			return fmt.Errorf("incident --apply is for the user, in their own terminal: this runs inside %s (%s); nothing was done", d.Tool, d.Source)
		}
		if !incidentTerminal(s.In) {
			return errors.New("incident --apply needs a person at a terminal to confirm; nothing was done (run it without --apply to see the plan)")
		}
	}
	if *retire && what != "shared" {
		return outbox.Usagef("--retire is for the shared token")
	}

	p, err := planIncident(what, name, ids, *deploy, *retire)
	if err != nil {
		return err
	}
	// Everything printed goes through a mask of every secret this machine holds and the leaked
	// value itself: a deploy script, a message or a name cannot put one on the screen.
	out := maskSecrets(s.Out, append(slices.Clone(p.values), config.SecretValues()...))
	defer out.Close()
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	p.gather(c)
	p.print(out, *apply)
	if !*apply {
		fmt.Fprintln(out, "\nPLAN ONLY: nothing was changed. To do it, run this again with --apply in your own terminal.")
		return nil
	}
	for _, st := range p.steps {
		if _, err := os.Stat(st.script); err != nil {
			return fmt.Errorf("%s is not there: nothing was done (pass --deploy DIR, the deploy/ directory of an aivoicebus checkout)", st.script)
		}
	}
	prompt := "\nType incident to do steps 1-4 now: "
	if len(p.manual) > 0 {
		prompt = "\nThe steps marked [you] come first. When they are done, type incident to do the rest now: "
	}
	answer, err := askLine(s, prompt)
	if err != nil {
		return fmt.Errorf("incident --apply needs a person at a terminal to confirm: %v; nothing was done", err)
	}
	if answer != "incident" {
		return errors.New("not confirmed: nothing was done")
	}
	return p.apply(c, out)
}

// incidentTerminal reports whether a person can answer on in (a variable so tests can be one).
var incidentTerminal = func(in io.Reader) bool { _, ok := terminalFile(in); return ok }

func planIncident(what, name string, ids []string, deploy string, retire bool) (*incidentPlan, error) {
	p := &incidentPlan{what: what, name: name, deployDir: deployDir(deploy)}
	script := func(file string) string {
		return filepath.Join(p.deployDir, file)
	}
	switch what {
	case "shared":
		p.label = "the shared token"
		f := os.Getenv("AIVOICEBUS_TOKEN_FILE")
		if f == "" {
			f = filepath.Join(config.Dir(), "token")
		}
		p.values = config.SecretFileLines(f)
		if len(p.values) == 0 {
			p.notes = append(p.notes, "there is no shared token at "+f+": run this on the machine you deploy from, or messages that held it cannot be found by their text")
		}
		rt := script("rotate-token.sh")
		p.steps = []incidentStep{
			{rt, []string{"add"}, "every node accepts the old and a new token (the new one waits at ~/.config/aivoicebus/token.next)"},
			{rt, []string{"promote"}, "the nodes talk to each other with the new token"},
		}
		if retire {
			p.steps = append(p.steps, incidentStep{rt, []string{"retire"}, "the old token stops working everywhere; machines not moved yet stop working too"})
		} else {
			p.notes = append(p.notes, "the leaked token keeps working until the last step: move every machine to the new token (~/.config/aivoicebus/token.next, or a client bundle), then "+rt+" retire --apply (or pass --retire to do it now)")
		}
	case "machine":
		host := strings.ToLower(name)
		if !incidentHost.MatchString(host) {
			return nil, outbox.Usagef("machine HOST: %q is not a host (lowercase letters, digits, . _ -: the part of an identity before the slash)", view.OneLine(name, 60))
		}
		p.name = host
		p.label = "the machine token of " + host
		p.values = config.SecretFileLines(filepath.Join(machineTokensDir(), host))
		if len(p.values) == 0 {
			p.notes = append(p.notes, "no machine token of "+host+" in "+machineTokensDir()+": run this where deploy/machine-token.sh made it")
		}
		p.steps = []incidentStep{{script("machine-token.sh"), []string{"revoke", host}, "no node accepts " + host + "'s token any more"}}
		p.notes = append(p.notes, host+" cannot reach the mesh until it has a new token: deploy/machine-token.sh add "+host+" --apply, then install it there by hand or with a client bundle ('token hand' needs "+host+" to read its mail)")
	case "observer":
		p.label = "the observer token"
		f := filepath.Join(config.Dir(), "observer-token")
		p.values = config.SecretFileLines(f)
		if len(p.values) == 0 {
			p.notes = append(p.notes, "there is no observer token at "+f+": run this on the machine you deploy from")
		}
		// No deploy script swaps only the observer token: deploy/install.sh carries it with a full install.
		p.manual = []string{
			"mv " + f + " " + f + ".leaked && " + script("install.sh") + " --gen-observer-token",
			"AIVOICEBUS_BIN=<the released linux binary> " + script("install.sh") + " --apply   (every node gets the new observer token; the old one stops working)",
			"give the dashboard the new token, then delete " + f + ".leaked",
		}
	case "secret":
		base, err := secretBase()
		if err != nil {
			return nil, err
		}
		if err := credstore.ValidName(name); err != nil {
			return nil, err
		}
		p.label = "the saved credential " + name
		if v, err := credstore.Load(base, name); err == nil {
			p.values = []string{v}
		} else {
			p.notes = append(p.notes, "it is not saved here ("+view.OneLine(err.Error(), 100)+"), so messages that held it cannot be found by their text")
		}
		where := "where it was issued"
		if hs, err := credstore.Hosts(base, name); err == nil && len(hs) > 0 {
			where += " (it is used with " + strings.Join(hs, ", ") + ")"
		}
		p.manual = []string{"revoke " + name + " " + where + ": nothing here can do that for you"}
		p.notes = append(p.notes, "--apply also removes this machine's copy (aivoicebus secret rm "+name+"); save the new one with: aivoicebus secret save "+name)
	case "message":
		ids = append([]string{name}, ids...)
		p.label = "a credential in message " + strings.ToLower(name)
		p.manual = []string{"revoke or rotate the credential it held. If it is the mesh's, say which instead of 'message': incident shared | machine HOST | observer | secret NAME --msg " + strings.ToLower(name)}
	}
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if !msg.ValidRetractID(id) {
			return nil, outbox.Usagef("--msg %q is not a message id (ten lowercase hex digits)", view.OneLine(id, 40))
		}
		if !slices.ContainsFunc(p.msgs, func(l leakedMsg) bool { return l.m.ID == id }) {
			p.msgs = append(p.msgs, leakedMsg{m: msg.Message{ID: id}, how: "given"})
		}
	}
	return p, nil
}

// deployDir is where the deploy scripts are: --deploy, else $AIVOICEBUS_DEPLOY_DIR, else ./deploy.
func deployDir(flagDir string) string {
	d := flagDir
	if d == "" {
		d = os.Getenv("AIVOICEBUS_DEPLOY_DIR")
	}
	if d == "" {
		d = "deploy"
	}
	if abs, err := filepath.Abs(d); err == nil {
		return abs
	}
	return d
}

// gather fills in the messages: those given are looked up, and every message on the nodes whose
// text holds the leaked value is added; then who read each one, and what this machine's audit log
// says about it. Nothing is written.
func (p *incidentPlan) gather(c *client.Client) {
	found := secrets.NewCanaries("")
	for _, v := range p.values {
		found.Add(v)
	}
	if len(p.values) > 0 {
		recs, err := c.SyncAll(map[string]uint64{})
		if err != nil {
			p.notes = append(p.notes, "could not search the messages on the nodes for it (that needs the shared or observer token): name them with --msg ID")
		}
		for _, r := range recs {
			if r.Msg == nil || r.Msg.Retracted != nil {
				continue
			}
			b, _ := json.Marshal(r.Msg) // every field, not only the text
			if _, hit := found.Find(r.Msg.Body + "\n" + string(b)); !hit {
				continue
			}
			if i := slices.IndexFunc(p.msgs, func(l leakedMsg) bool { return l.m.ID == r.Msg.ID }); i >= 0 {
				p.msgs[i].how = "given, and found in the log"
				continue
			}
			p.msgs = append(p.msgs, leakedMsg{m: *r.Msg, how: "found in the log"})
		}
	}
	audit, _ := config.ReadAudit()
	for i := range p.msgs {
		l := &p.msgs[i]
		if l.m.From == "" { // given: read it from its thread
			if ms, err := c.Thread(l.m.ID); err == nil {
				for _, m := range ms {
					if m.ID == l.m.ID {
						l.m = m
					}
				}
			}
		}
		if r, err := c.Receipts(l.m.ID); err == nil {
			l.rec = &r
		}
		for _, e := range audit {
			if strings.Contains(e.Detail, l.m.ID) {
				l.audit = append(l.audit, e)
			}
		}
	}
}

func (p *incidentPlan) print(w io.Writer, apply bool) {
	fmt.Fprintf(w, "incident: %s leaked\n", p.label)
	fmt.Fprintln(w, "\n1. Revoke or rotate it, first, so it is worthless before anything else is said about it")
	for _, m := range p.manual {
		fmt.Fprintln(w, "   [you] "+m)
	}
	for i, st := range p.steps {
		fmt.Fprintf(w, "   run: %s %s --apply\n        %s\n", st.script, strings.Join(st.args, " "), st.why)
		if i > 0 {
			continue // a later step's dry run depends on the earlier one having been applied
		}
		fmt.Fprintln(w, "        its dry run now:")
		if err := runScript(indent(w, "          | "), st.script, st.args...); err != nil {
			fmt.Fprintf(w, "          (the dry run failed: %s)\n", view.OneLine(err.Error(), 200))
		}
	}
	if p.what == "secret" {
		fmt.Fprintf(w, "   then remove this machine's copy (aivoicebus secret rm %s)\n", p.name)
	}

	fmt.Fprintln(w, "\n2. Retract the messages that held it, as leaks (the nodes tell each session that read one, at its next inbox read)")
	if len(p.msgs) == 0 {
		fmt.Fprintln(w, "   none found: name one with --msg ID")
	}
	me := identity.Me()
	for _, l := range p.msgs {
		if l.m.From == "" {
			fmt.Fprintf(w, "   %s  not found on the nodes (%s)\n", l.m.ID, l.how)
			continue
		}
		state := ""
		switch r := l.m.Retracted; {
		case r != nil && r.Leak:
			state = "  already retracted as a leak"
		case r != nil:
			state = "  already retracted; it will be flagged as a leak"
		}
		fmt.Fprintf(w, "   %s  from %s to %s, sent %s UTC (%s)%s\n", l.m.ID, view.Name(l.m.From, 60), view.Name(l.m.To, 60), l.m.At.UTC().Format("2006-01-02 15:04"), l.how, state)
		if msg.HostOf(l.m.From) != msg.HostOf(me) {
			fmt.Fprintf(w, "      sent from another machine: retracted as %s, which needs the shared token here\n", view.Name(l.m.From, 60))
		}
	}

	fmt.Fprintln(w, "\n3. Who read them, and when (from the nodes' receipts and this machine's audit log)")
	for _, l := range p.msgs {
		if l.rec == nil {
			fmt.Fprintf(w, "   %s: the nodes gave no receipts\n", l.m.ID)
			continue
		}
		if len(l.rec.Reads) == 0 {
			fmt.Fprintf(w, "   %s: nobody has read it\n", l.m.ID)
		}
		for _, r := range l.rec.Reads {
			via := ""
			if r.Via != "" {
				via = " (" + view.OneLine(r.Via, 20) + ")"
			}
			fmt.Fprintf(w, "   %s: read by %s at %s UTC%s\n", l.m.ID, view.Name(r.By, 60), r.At.UTC().Format("2006-01-02 15:04:05"), via)
		}
		if o := l.rec.Owner; o != nil {
			fmt.Fprintf(w, "   %s: taken as a task by %s at %s UTC\n", l.m.ID, view.Name(o.By, 60), o.At.UTC().Format("2006-01-02 15:04:05"))
		}
		if l.rec.Replies > 0 {
			fmt.Fprintf(w, "   %s: %d repl(ies) in its thread: aivoicebus thread %s\n", l.m.ID, l.rec.Replies, l.m.ID)
		}
		for _, e := range l.audit {
			fmt.Fprintf(w, "   %s: audit #%d %s %s %s\n", l.m.ID, e.Seq, e.At.UTC().Format("2006-01-02 15:04:05"), msg.SafeName(e.Who), view.OneLine(e.Action, 30))
		}
	}
	fmt.Fprintln(w, "   Anyone who read one may still have it in their context or notes: the retraction notice tells them; it does not erase it.")

	fmt.Fprintln(w, "\n4. Write the incident to this machine's audit log (aivoicebus audit)")
	if len(p.notes) > 0 {
		fmt.Fprintln(w, "\nNotes:")
		for _, n := range p.notes {
			fmt.Fprintln(w, "  - "+n)
		}
	}
}

// apply does the plan, in order, and stops at the first credential step that fails: retracting
// while the credential still works would only point at it.
func (p *incidentPlan) apply(c *client.Client, w io.Writer) error {
	me := identity.Me()
	subject := p.what
	if p.name != "" {
		subject += " " + p.name
	}
	config.Audit(me, "incident-start", subject)
	fmt.Fprintln(w, "\n1. Revoking or rotating it")
	for _, st := range p.steps {
		fmt.Fprintf(w, "   %s %s --apply\n", filepath.Base(st.script), strings.Join(st.args, " "))
		if err := runScript(indent(w, "   | "), st.script, append(slices.Clone(st.args), "--apply")...); err != nil {
			config.Audit(me, "incident", subject+": stopped, "+filepath.Base(st.script)+" "+strings.Join(st.args, " ")+" failed; nothing retracted")
			return fmt.Errorf("%s %s --apply failed (%v): stopped here, nothing was retracted. Fix it and run this again", filepath.Base(st.script), strings.Join(st.args, " "), err)
		}
	}
	if p.what == "secret" {
		base, err := secretBase()
		if err == nil {
			err = credstore.Remove(base, p.name)
		}
		if err != nil {
			fmt.Fprintf(w, "   this machine's copy was not removed: %s\n", view.OneLine(err.Error(), 120))
		} else {
			fmt.Fprintf(w, "   removed this machine's copy of %s\n", p.name)
		}
	}

	fmt.Fprintln(w, "\n2. Retracting the messages that held it")
	var done, failed []string
	for _, l := range p.msgs {
		if l.m.From == "" {
			failed = append(failed, l.m.ID)
			fmt.Fprintf(w, "   %s: not found on the nodes, not retracted\n", l.m.ID)
			continue
		}
		o := outbox.RetractOptions{Leak: true}
		if msg.HostOf(l.m.From) != msg.HostOf(me) {
			o.As = l.m.From
		}
		if err := outbox.Retract(c, l.m.ID, o, indent(w, "   ")); err != nil {
			failed = append(failed, l.m.ID)
			fmt.Fprintf(w, "   %s: NOT retracted: %s\n", l.m.ID, view.OneLine(err.Error(), 200))
			continue
		}
		done = append(done, l.m.ID)
	}
	readers := map[string]bool{}
	for _, l := range p.msgs {
		if l.rec != nil {
			for _, r := range l.rec.Reads {
				readers[r.By] = true
			}
		}
	}
	detail := fmt.Sprintf("%s: %d step(s) applied; retracted %s; not retracted %s; %d reader(s)", subject, len(p.steps), idList(done), idList(failed), len(readers))
	if len(p.manual) > 0 {
		detail += "; the person confirmed the manual steps"
	}
	config.Audit(me, "incident", detail)
	fmt.Fprintln(w, "\n3. Readers are told by the nodes at their next inbox read (see step 3 of the plan above for who)")
	fmt.Fprintln(w, "\n4. Written to the audit log: "+detail)
	for _, n := range p.notes {
		fmt.Fprintln(w, "   still to do: "+n)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d message(s) were not retracted: %s", len(failed), strings.Join(failed, " "))
	}
	return nil
}

func idList(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ",")
}

// runScript runs a deploy script with no input, its output and errors (one stream) to w.
func runScript(w io.Writer, script string, args ...string) error {
	cmd := exec.Command(script, args...)
	cmd.Stdin = nil
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

// indent prefixes every line written to w.
func indent(w io.Writer, prefix string) io.Writer { return &indentWriter{w: w, prefix: prefix, bol: true} }

type indentWriter struct {
	w      io.Writer
	prefix string
	bol    bool
}

func (iw *indentWriter) Write(p []byte) (int, error) {
	var b strings.Builder
	for _, c := range string(p) {
		if iw.bol {
			b.WriteString(iw.prefix)
		}
		b.WriteRune(c)
		iw.bol = c == '\n'
	}
	if _, err := io.WriteString(iw.w, b.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}

// maskSecrets wraps w so that none of values gets through, even split across writes.
func maskSecrets(w io.Writer, values []string) io.WriteCloser {
	var chain []io.WriteCloser
	out := w
	seen := map[string]bool{}
	for _, v := range values {
		if v = strings.TrimSpace(v); len(v) < 8 || seen[v] {
			continue
		}
		seen[v] = true
		r := credstore.Redactor(out, v)
		chain = append(chain, r)
		out = r
	}
	return &maskChain{w: out, chain: chain}
}

type maskChain struct {
	w     io.Writer
	chain []io.WriteCloser
}

func (m *maskChain) Write(p []byte) (int, error) { return m.w.Write(p) }

func (m *maskChain) Close() error {
	for i := len(m.chain) - 1; i >= 0; i-- { // outermost first, so what it held reaches the inner ones
		_ = m.chain[i].Close()
	}
	return nil
}
