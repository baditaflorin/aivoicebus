package outbox

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// maxAmendHops bounds how far Amend follows a message that was already amended to its latest version.
const maxAmendHops = 32

// AmendOptions are what the person (or agent) asks for.
type AmendOptions struct {
	DryRun bool // do everything but send and write
	Force  bool // send despite the long-body guard (the CLI's --force; the MCP tool has none)
}

// Amend replaces message id, which this machine sent, with a corrected text (msg/amend.go): the new
// text goes to exactly the audience of the original, in its thread, at its priority and for what is
// left of its life, and nodes that apply amendments then give a reader that has not read the original
// only the new text, and one that has read it the new text marked as replacing what it read. The
// original stays, with a forward link, in threads and `inbox --all`; nothing is erased.
//
// It is the one place that decides what may be amended and what is said about it, so the CLI and the
// MCP tool check and say the same things. Everything goes to w in order: which message, what is done,
// how many sessions had read the original, and what an amendment cannot do. It returns the message
// sent (zero for a dry run).
func Amend(c *client.Client, id, text string, o AmendOptions, w io.Writer) (msg.Message, error) {
	me := identity.Me()
	id, text = strings.ToLower(strings.TrimSpace(id)), strings.TrimSpace(text)
	switch {
	case !msg.ValidRetractID(id):
		return msg.Message{}, Usagef("%q is not a message id (ten lowercase hex digits): `aivoicebus inbox --all` lists ids", oneLine(id, 40))
	case text == "":
		return msg.Message{}, Usagef("amend needs the corrected text: `aivoicebus amend %s \"the corrected message\"`", id)
	}
	if c.Warn == nil {
		c.Warn = w // "only 1 node took the write" belongs with the rest of what is said here
	}
	orig, found, err := lookup(c, id)
	switch {
	case err != nil:
		return msg.Message{}, err
	case !found:
		return msg.Message{}, NotFoundf("message %s was not found on the nodes (a typo, or it has not reached them yet): try again in a minute, or check the id with `aivoicebus inbox --all`", id)
	}
	// What is amended is the latest version: a message that already has an amendment is not amended
	// again beside it, so two corrections cannot race into two live texts.
	for hops := 0; orig.AmendedBy != nil && orig.Retracted == nil; hops++ {
		next, ok, err := lookup(c, orig.AmendedBy.By)
		switch {
		case hops >= maxAmendHops:
			return msg.Message{}, fmt.Errorf("message %s has been amended more than %d times in a row: amend the latest one by its id (`aivoicebus thread %s` shows the chain)", id, maxAmendHops, id)
		case err != nil:
			return msg.Message{}, err
		case !ok:
			return msg.Message{}, NotFoundf("message %s was already amended by %s, which is not on these nodes yet: try again in a minute", orig.ID, view.OneLine(orig.AmendedBy.By, msg.MaxRef))
		}
		fmt.Fprintf(w, "message %s was already amended by %s: amending that one\n", orig.ID, next.ID)
		orig = next
	}
	kind, secret := credentialKind(orig.Body)
	if orig.Retracted == nil && !orig.Private && !msg.IsSealed(orig.Body) {
		fmt.Fprintf(w, "replacing message %s to %s: %s\n", orig.ID, view.Name(orig.To, 80), shown(orig, false, secret))
		if secret {
			fmt.Fprintf(w, "note: the text being replaced looks like a %s: amending does not erase it; retract it with --leak and rotate the credential (`aivoicebus retract --leak %s`)\n", kind, orig.ID)
		}
	}
	switch {
	case orig.Retracted != nil:
		return msg.Message{}, Usagef("message %s was withdrawn by %s: there is nothing to amend; send the corrected text as a new message (`aivoicebus send`)", orig.ID, msg.TombstoneWho(orig.Retracted.By))
	case !msg.MayAmend(me, orig.From):
		return msg.Message{}, fmt.Errorf("message %s was sent from %s; this machine is %s; only that machine can amend it: ask the user there to run `aivoicebus amend %s \"...\"`", orig.ID, view.Name(msg.HostOf(orig.From), 60), view.Name(msg.HostOf(me), 60), orig.ID)
	case orig.Kind != "":
		return msg.Message{}, Usagef("message %s is a %s, not a plain note: amend replaces notes (a task or a question would reach its readers twice). Withdraw it with `aivoicebus retract %s` and send the corrected one", orig.ID, orig.Kind, orig.ID)
	case orig.Private || msg.IsSealed(orig.Body):
		return msg.Message{}, Usagef("message %s was sent --private: the corrected text would have to be sealed too, and amend does not seal. Withdraw it with `aivoicebus retract %s` and send the corrected text again with --private", orig.ID, orig.ID)
	}

	req := Request{To: orig.To, Body: text, Re: orig.ID, Prio: orig.Prio, Queue: true, Amending: true, Force: o.Force} // the audience and the priority were chosen when the original was sent
	if orig.Exp != nil {
		if left := time.Until(*orig.Exp); left > 0 {
			req.TTL = min(max(left, msg.MinTTL), msg.MaxTTL).Truncate(time.Second).String()
		}
	}
	plan, err := Prepare(c, req, w)
	if err != nil {
		return msg.Message{}, err
	}
	readers, known := readersOf(c, orig.ID)
	h, herr := c.Health()
	if herr == nil && !h.Has(msg.FeatureAmend) {
		fmt.Fprintf(w, "warning: the node that answered (%s) does not apply amendments: sessions it serves get the corrected text as an ordinary reply, beside the original, until it is upgraded\n", oneLine(h.Node, 40))
	}
	if o.DryRun {
		plan.Describe(w)
		fmt.Fprintln(w, readersLine(orig.ID, readers, known, true))
		return msg.Message{}, nil
	}

	out, err := plan.Send(c)
	if err != nil {
		return msg.Message{}, err
	}
	if err := c.Amend(out.ID, me); err != nil {
		return out, fmt.Errorf("the corrected text went out as message %s, but nodes would not link it to %s as its replacement (%w): until they do it is an ordinary reply beside the original. Link it with `aivoicebus ack amend:%s`, or take it back with `aivoicebus retract %s`", out.ID, orig.ID, err, out.ID, out.ID)
	}
	fmt.Fprintf(w, "amended %s as %s thread %s\n", orig.ID, out.ID, out.Thread)
	if len(plan.Notes) > 0 {
		fmt.Fprintln(w, "  "+strings.Join(plan.Notes, "; "))
	}
	fmt.Fprintln(w, readersLine(orig.ID, readers, known, false))
	fmt.Fprintf(w, "undo: `aivoicebus retract %s` takes the new text back and delivers the original again to sessions that have not read either.\n", out.ID)
	fmt.Fprintln(w, "It does NOT erase the original: it stays in node logs, exports, the dashboard and `aivoicebus thread`, with a link to its replacement, and in contexts that already showed it.")
	return out, nil
}

// readersOf is how many other sessions this node says read message id in full (a count of an fyi at
// session start is not a read), and whether it could say at all.
func readersOf(c *client.Client, id string) (n int, known bool) {
	rc, err := c.Receipts(id)
	if err != nil {
		return 0, false
	}
	for _, r := range rc.Reads {
		if r.Via != msg.ViaHookCount {
			n++
		}
	}
	return n, true
}

// readersLine says who gets what: the sessions that read the original are told it was replaced, the
// others get only the new text. For a dry run it says what would happen.
func readersLine(id string, readers int, known, dry bool) string {
	one, many := "gets", "get"
	if dry {
		one, many = "would get", "would get"
	}
	switch {
	case !known:
		return fmt.Sprintf("how many sessions had read %s: unknown (the nodes that answered cannot say). Those that did %s the new text marked as replacing it; the others %s only the new text.", id, many, many)
	case readers == 0:
		return fmt.Sprintf("no other session had read %s, as far as the node that answered knows: every reader %s only the new text.", id, one)
	}
	return fmt.Sprintf("%d other session(s) had read %s (at least: counted on the node that answered): at their next inbox read they %s the new text marked \"[amended ...]\", as a reply in the same thread; the rest %s only the new text.", readers, id, many, many)
}
