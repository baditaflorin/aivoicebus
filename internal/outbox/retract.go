package outbox

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/secrets"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// excerptRunes is how much of the body is shown to say which message is being taken back.
const excerptRunes = 60

// What stands in for the text of a message that must not be printed again: one that looks like a
// credential, or that the sender said held one.
const (
	withheld        = "[withheld: looks like a credential]"
	withheldFlagged = "[withheld: flagged as a credential]"
)

// retractCaveat is what every retraction is said to do, in the words the docs use: where it hides
// the text, who is told, and that it does not erase it. The dashboard is a separate deploy, so it is not
// promised to hide anything yet. Sessions that already read the message are told only by nodes that
// make notices (msg.RetractNoticeSince), so this says which.
const (
	retractErase  = "It does NOT erase it: it stays in node logs, exports and the dashboard archive, and in contexts that already showed it. If it held a credential, rotate it."
	retractTold   = "Sessions that already read it are told once, by nodes that make notices, at that session's next inbox read: its next session start, or (for a leak, which is urgent) its next prompt in a running Claude Code session; a session that never reads its inbox again is not told. "
	retractCaveat = "This hides the text from inboxes and threads on nodes that apply retractions, and from the dashboard once it runs the matching release. " + retractTold + retractErase

	// retractCaveatRecorded is the same where the retraction was only recorded as a plain ack:
	// whether the text is hidden depends on the nodes that apply it.
	retractCaveatRecorded = "Where a node applies it, this hides the text from inboxes and threads, and from the dashboard once it runs the matching release. " + retractTold + retractErase
)

// RetractOptions are what the person (or agent) asks for.
type RetractOptions struct {
	Leak   bool   // the text held a credential: flag the retraction so
	DryRun bool   // do everything but write
	As     string // the identity to retract as, for an operator with the shared token (never set by the MCP tool)

	// Last, with no id, takes back the latest message sent from this machine by any session on
	// it. Without it only this session's own last message is picked: another session's is shown
	// and its id asked for. It is the CLI's --last; the MCP tool has no such option, so an agent
	// can never withdraw something it did not send.
	Last bool

	Agent bool // the caller is an agent tool: its refusals say "pass the id", not "use --last"
}

// Retract takes back message id: its text stops being delivered and shown, and a tombstone says
// who withdrew it. With no id it is this session's last message (see sent.json), and nothing else
// unless o.Last. The CLI and the MCP tool both call it, so they check and say the same things, and
// everything goes to w in order: which message, what was done, how many sessions had read it, which
// nodes still deliver it, and what retraction cannot do.
//
// The identity retracted as is this session's, or o.As. A message may be retracted by any identity
// on the machine that sent it, and the node enforces that; checking here first only gives a plain
// reason. A body that looks like a credential is retracted as one (leak), and the text of a
// message retracted as a leak is never printed.
func Retract(c *client.Client, id string, o RetractOptions, w io.Writer) error {
	by := identity.Me()
	if o.As != "" {
		if by = strings.ToLower(strings.TrimSpace(o.As)); !msg.ValidIdentity(by) {
			return errors.New("--as: not an identity (host/agent): `aivoicebus whoami` shows this session's")
		}
	}
	last, exact := id == "", true
	var cand sentMsg
	switch {
	case last:
		e, ex, ok := lastSent(by, clock(), o.Last)
		if !ok {
			return errors.New("no message sent from this machine in the last " + sentWindowText + " is remembered here: give the message id (`aivoicebus inbox --all` and `aivoicebus thread <id>` show ids)")
		}
		cand, exact, id = e, ex, e.ID
	case o.Last:
		return errors.New("--last takes the machine's latest message: run it again without the id")
	case !msg.ValidRetractID(id):
		return fmt.Errorf("%q is not a message id (ten lowercase hex digits): `aivoicebus inbox --all` lists ids", oneLine(id, 40))
	}

	m, found, err := lookup(c, id)
	var kind string
	var secret bool
	if found && m.Retracted == nil {
		kind, secret = credentialKind(m.Body)
	}
	text := shown(m, o.Leak, secret)
	switch {
	case err != nil:
		text = "[could not be read: no node answered]"
	case !found:
		text = "[not found on the nodes]"
	}
	if last && !exact && !o.Last && !o.DryRun {
		// Another session's message is not this session's to take back unasked: it is named, and
		// nothing is written. Taking it back is one more command, with its id.
		how := "retract it with: aivoicebus retract " + id + " (or `aivoicebus retract --last` for whatever this machine sent last)"
		if o.Agent {
			how = "if that is the message to withdraw, call aivoicebus_retract with id " + id
		}
		return fmt.Errorf("nothing sent as %s is remembered, so nothing was taken back. %s; %s", view.Name(by, 80), candidate(cand, text), how) // nextstep: how
	}
	switch {
	case err != nil:
		return err
	case !found:
		return NotFoundf("message %s was not found on the nodes (a typo, or it has not reached them yet): try again in a minute, or check the id with `aivoicebus inbox --all`", id)
	}
	switch {
	case !last:
		fmt.Fprintf(w, "message %s to %s: %s\n", m.ID, view.Name(m.To, 80), text)
	case exact:
		fmt.Fprintf(w, "last message sent by this session: %s to %s: %s\n", m.ID, view.Name(m.To, 80), text)
	default:
		fmt.Fprintln(w, candidate(cand, text))
		if !o.Last { // a dry run: say what a real one would do
			note := "a real run with no id would refuse: this session sent nothing of its own; pass the id"
			if !o.Agent {
				note += " or --last"
			}
			fmt.Fprintln(w, "note: "+note)
		}
	}

	if !msg.MayRetract(by, m.From) {
		if o.As != "" {
			return fmt.Errorf("--as %s is on %s, but message %s was sent from %s: only a session on that machine can retract it: ask the user there to run `aivoicebus retract %s`", view.Name(by, 80), view.Name(msg.HostOf(by), 60), id, view.Name(msg.HostOf(m.From), 60), id)
		}
		return fmt.Errorf("message %s was sent from %s; this machine is %s; only that machine can retract it: ask the user there to run `aivoicebus retract %s`", id, view.Name(msg.HostOf(m.From), 60), view.Name(msg.HostOf(by), 60), id)
	}
	if o.As != "" {
		if err := needSharedToken(c, by); err != nil {
			return err
		}
	}
	if by != m.From {
		fmt.Fprintf(w, "note: retracting as %s a message sent as %s\n", view.Name(by, 80), view.Name(m.From, 80))
	}

	leak := o.Leak
	mesh := viewMesh(c)
	if r := m.Retracted; r != nil && !(leak && !r.Leak) {
		fmt.Fprintf(w, "already retracted by %s at %s", view.Name(r.By, 80), view.When(r.At, time.Now()))
		if r.Leak {
			fmt.Fprint(w, " (flagged as having held a credential)")
		}
		fmt.Fprintln(w, "; nothing to do")
		finish(w, mesh, false)
		return nil
	}
	if m.Retracted != nil {
		fmt.Fprintln(w, "it was retracted without saying it held a credential; adding that")
	} else if secret && !leak {
		leak = true
		fmt.Fprintf(w, "the text looks like a %s: retracting it as one that held a credential (--leak)\n", kind)
	}

	if c.Warn == nil {
		c.Warn = w // "only 1 node took the write" belongs with the rest of what is said here
	}
	res, err := c.Retract(id, by, leak, o.DryRun)
	if err != nil {
		return explainRetract(err, id)
	}
	report(w, res, id, leak, o.DryRun, mesh)
	finish(w, mesh, res.State == msg.RetractRecorded)
	return nil
}

// candidate says which message a retraction without an id would be about: the machine's latest,
// with who sent it and when, which is what a person needs to decide whether it is the one.
func candidate(e sentMsg, text string) string {
	return fmt.Sprintf("last message from this machine: %s sent as %s to %s at %s: %s", e.ID, view.Name(e.From, 80), view.Name(e.To, 80), view.When(e.At, time.Now()), text)
}

// shown is how a message is named before it is taken back: the start of its text cut to one line,
// unless it is already withdrawn (there is no text), looks like a credential, or is being retracted
// as one: then the text is not printed at all. The text is data from the nodes, shown for the sender
// to recognise.
func shown(m msg.Message, leak, secret bool) string {
	switch {
	case m.Retracted != nil:
		return "[withdrawn: text no longer shown]"
	case secret:
		return withheld
	case leak:
		return withheldFlagged
	}
	return `"` + oneLine(m.Body, excerptRunes) + `"`
}

// credentialKind says what in body looks like a credential (never the text): a secret this
// machine holds, else the strongest shape secrets.Find knows, weak ones included, since the cost of
// flagging a harmless message is a stronger tombstone and the cost of missing one is a credential
// that is not rotated. It looks at the body as it is and as it would be printed (see oneLine): a
// token with a zero-width space in it is no token to the detector but is one on the screen.
func credentialKind(body string) (string, bool) {
	if kind, ok := credentialShape(body); ok {
		return kind, true
	}
	if flat := oneLine(body, len(body)); flat != body {
		return credentialShape(flat)
	}
	return "", false
}

func credentialShape(text string) (string, bool) {
	if f, found := localSecrets().Find(text); found {
		return f.Kind, true
	}
	found := secrets.Find(text)
	for _, f := range found {
		if f.Strong {
			return f.Kind, true
		}
	}
	if len(found) > 0 {
		return found[0].Kind, true
	}
	return "", false
}

// needSharedToken explains --as to a machine token, which the node would refuse with a bare 403:
// it can only act as its own machine. A node too old to say is asked anyway.
func needSharedToken(c *client.Client, by string) error {
	ti, err := c.TokenInfo()
	switch {
	case err != nil:
		return nil
	case ti.Observer:
		return errors.New("this token is read-only (an observer token) and cannot retract: ask the user to run it where the shared token is")
	case ti.Host != "" && !strings.EqualFold(ti.Host, msg.HostOf(by)):
		return fmt.Errorf("--as needs the shared token: this machine's token can only act as %s/...; ask the user to run it where the shared token is", oneLine(ti.Host, 60))
	}
	return nil
}

// explainRetract adds to a refusal what the sender can do about it.
func explainRetract(err error, id string) error {
	var se *client.StatusError
	if errors.As(err, &se) && se.Code == http.StatusUnprocessableEntity {
		return fmt.Errorf("%w. Try again in a minute: message %s may still be replicating to the nodes", err, id)
	}
	return err
}

// report says what the node did, or would do for a dry run.
func report(w io.Writer, res msg.RetractResult, id string, leak, dry bool, mesh meshView) {
	state := res.State
	if dry {
		state = res.Would
		if state == "" { // no node could say: a node that predates retraction has no dry run, and another may not hold the message yet
			fmt.Fprintln(w, "dry run: nothing was written. No node that answered could say what a real run would do (a node that predates retractions cannot, and another may not hold the message yet); a real run would record the retraction as a plain ack.")
		} else {
			fmt.Fprintln(w, "dry run: nothing was written")
		}
	}
	flagged := ""
	if leak {
		flagged = ", flagged as having held a credential"
	}
	verb := func(real, would string) string {
		if dry {
			return would
		}
		return real
	}
	switch state {
	case msg.RetractRetracted:
		fmt.Fprintf(w, "%s %s%s\n", verb("retracted", "would retract"), id, flagged)
	case msg.RetractEscalated:
		fmt.Fprintf(w, "%s %s as having held a credential (it was retracted already)\n", verb("flagged", "would flag"), id)
	case msg.RetractAlready:
		fmt.Fprintf(w, "already retracted; %s\n", verb("nothing was written", "a real run would write nothing"))
	case msg.RetractRecorded:
		// All the client knows is that a node took the plain ack (the route nodes that
		// predate retractions take): not which one, or whether it applied it. What the nodes run is said from health.
		fmt.Fprintf(w, "recorded the retraction of %s%s as a plain ack: it is in the log and replicates, but the node that took it may predate retractions and not apply it\n", id, flagged)
		if mesh.known && !mesh.applies {
			fmt.Fprintln(w, "no node of this mesh applies retractions yet, so nothing hides the message until one does")
		} else {
			fmt.Fprintln(w, "nodes that apply retractions will hide it once they hold the message")
		}
	}
	if res.Expired {
		fmt.Fprintln(w, "it had already expired, so hiding it changes little")
	}
	switch {
	case res.ReadBy < 0:
		fmt.Fprintln(w, "how many other sessions had read it: unknown (the nodes that answered cannot say)")
	case res.ReadBy == 0:
		fmt.Fprintln(w, "no other session had read it, as far as the node that answered knows")
	default:
		fmt.Fprintf(w, "%d other session(s) had already read it (at least: counted on the node that answered). Nodes that make notices tell them once; others do not: say so yourself if they may have acted on it.\n", res.ReadBy)
	}
	if leak && state != msg.RetractAlready {
		fmt.Fprintln(w, "rotate that credential now: withdrawing the message does not unsend it")
	}
}

// finish names the nodes that still deliver the message, and says what retraction is not. When the
// retraction was only recorded as a plain ack, it does not claim that the text is hidden.
func finish(w io.Writer, mesh meshView, recorded bool) {
	for _, n := range mesh.old {
		fmt.Fprintf(w, "%s still delivers it: it does not apply retractions until it is upgraded\n", n)
	}
	if recorded {
		fmt.Fprintln(w, retractCaveatRecorded)
		return
	}
	fmt.Fprintln(w, retractCaveat)
}

// meshView is what the node that answers says of the releases the nodes run: the ones before
// msg.RetractSince, named, and whether any release is known and any is that new.
type meshView struct {
	old            []string
	known, applies bool
}

func viewMesh(c *client.Client) meshView {
	var v meshView
	for _, r := range nodeReleases(c) {
		v.known = true
		if release.Newer(msg.RetractSince, r.version) {
			v.old = append(v.old, r.label+" ("+r.version+")")
		} else {
			v.applies = true
		}
	}
	return v
}

// oneLine is view.OneLine, the one place that makes peer text one safe line (what retract prints
// is what a reader is shown, and the leak check below looks at exactly that).
func oneLine(s string, n int) string { return view.OneLine(s, n) }
