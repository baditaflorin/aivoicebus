package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// doneCmd is the person's "I already did it" or "it is not needed": one machine-wide settle (see
// msg/settle.go). The message stops being shown to every session on this machine, the later ones
// too, and its sender is told in the thread. Like approve it is the person's: it refuses in an
// agent's session, because the node cannot tell the person from an agent. A message that must be
// answered (an approval request, a [token hand] task, a question) is never settled.
func doneCmd(args []string, s Streams) error {
	fs := newFlagSet("done")
	all := fs.Bool("all", false, "settle everything that waits for the person on this machine (never an approval, a [token hand] or a question)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	words, err := trailingFlags(fs, s)
	if err != nil {
		return err
	}
	usage := outbox.Usagef(`usage: done ID [ID...] ["why"]   |   done --all ["why"]   (the person's: already done by hand, or not needed)`)
	var ids []string
	for len(words) > 0 && msg.ValidID(words[0]) {
		ids, words = append(ids, words[0]), words[1:]
	}
	why := strings.Join(words, " ")
	if (*all && len(ids) > 0) || (!*all && len(ids) == 0) {
		return usage
	}
	if err := personOnly("done"); err != nil {
		return err
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	me := identity.Me()
	if *all {
		if ids, err = waitingForPerson(c, me, s.Err); err != nil {
			return err
		}
		if len(ids) == 0 {
			fmt.Fprintln(s.Out, "nothing waits for the person on this machine that done may settle")
			return nil
		}
	}
	var failed int
	for _, id := range ids {
		if err := settleOne(c, me, id, why, s); err != nil {
			fmt.Fprintf(s.Err, "%s: %v\n", id, err)
			failed++
		}
	}
	if failed > 0 {
		return exitError{exitFailure}
	}
	return nil
}

// waitingForPerson lists the ids of what waits for this machine's person and may be settled; a
// message that must be answered is counted on w, not listed.
func waitingForPerson(c *client.Client, me string, w io.Writer) ([]string, error) {
	ms, err := c.Peek(msg.HumanOf(me))
	if err != nil {
		return nil, err
	}
	var ids []string
	answer := 0
	for _, m := range ms {
		switch {
		case !msg.IsHuman(m.To) || m.Retracted != nil:
		case msg.MustAnswer(m):
			answer++
		default:
			ids = append(ids, m.ID)
		}
	}
	if answer > 0 {
		fmt.Fprintf(w, "%d more must be answered, not settled (aivoicebus answer | approve | token install)\n", answer)
	}
	return ids, nil
}

// settleOne settles message id for the machine of me, and tells its sender.
func settleOne(c *client.Client, me, id, why string, s Streams) error {
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
		return outbox.NotFoundf("message %s was not found on the nodes: check the id with `aivoicebus queue` or `aivoicebus inbox --all`", id)
	}
	if msg.MustAnswer(m) {
		return fmt.Errorf("it must be answered, not settled (`aivoicebus queue` lists what waits for you): %s", msg.PersonStep(m))
	}
	host := msg.HostOf(me)
	if h := msg.HostOf(m.To); h != host && m.To != "all" && !msg.IsSelector(m.To) && !strings.HasPrefix(m.To, "*/") && h != "" {
		return fmt.Errorf("it is addressed to %s, not to this machine (%s): nothing here shows it to a session (`aivoicebus inbox --all` lists what reaches this machine)", view.Name(m.To, msg.MaxIdentity), host)
	}
	human := msg.HumanOf(me)
	hs, _ := c.Healths()
	var old []string
	for _, h := range hs {
		if !h.Has(msg.FeatureSettle) {
			old = append(old, view.OneLine(h.Node, 40))
		}
	}
	if len(hs) > 0 && len(old) == len(hs) { // no node applies a settle: the older way
		return settleOld(c, me, m, why, old, s)
	}
	if err := c.Settle(id, human); err != nil {
		return err
	}
	config.Audit(me, "done", fmt.Sprintf("%s %q", id, view.OneLine(why, 200)))
	told := tellSender(c, human, m, msg.DoneBody(msg.DoneByPersonTag, why), msg.KindProgress)
	fmt.Fprintf(s.Out, "settled %s for %s: no session on this machine is shown it again (`aivoicebus thread %s` still reads it). %s\n", id, host, id, told)
	if len(old) > 0 {
		fmt.Fprintf(s.Out, "  node(s) %s run an older release and do not apply it yet: sessions that read from them are still shown it until they are updated\n", strings.Join(old, ", "))
	}
	return nil
}

// settleOld is done against nodes that predate settles: a task is claimed and ended (the claim hides it
// from the other sessions, the end tells its sender), and a plain message cannot be settled there.
func settleOld(c *client.Client, me string, m msg.Message, why string, old []string, s Streams) error {
	if m.Kind != msg.KindTask {
		return fmt.Errorf("the nodes (%s) run an older release that cannot settle a plain message: it is still shown to every session on this machine until they are updated (`aivoicebus nodes`)", strings.Join(old, ", "))
	}
	r, err := tasks.Claim(c, me, tasks.Person, m.ID, "done by the person", 0, s.Err)
	if err != nil && !errors.Is(err, tasks.ErrOwnTask) {
		return err
	}
	if !r.Mine && r.ClaimID == "" && !r.Already {
		return fmt.Errorf("the task is already held by %s: ask them to end it, or read it with `aivoicebus thread %s`", view.Name(r.Owner.From, msg.MaxIdentity), m.ID)
	}
	config.Audit(me, "done", fmt.Sprintf("%s %q (claimed and ended: the nodes predate settles)", m.ID, view.OneLine(why, 200)))
	told := tellSender(c, me, m, msg.DoneBody(msg.DoneByPersonTag, why), msg.KindEnd)
	fmt.Fprintf(s.Out, "claimed and ended task %s (the nodes, %s, run an older release that cannot settle it for the machine). %s\n", m.ID, strings.Join(old, ", "), told)
	return nil
}

// tellSender writes the one-line thread message to the sender of m and says how it went.
func tellSender(c *client.Client, from string, m msg.Message, body, kind string) string {
	if msg.IsSystemSender(m.From) || m.From == from {
		return ""
	}
	plan, err := outbox.Prepare(c, outbox.Request{To: m.From, Re: m.ID, Thread: m.Thread, Kind: kind, Body: body, TargetSaid: true, From: from}, io.Discard)
	if err == nil {
		_, err = plan.Send(c)
	}
	if err != nil {
		return "Its sender could not be told: " + view.OneLine(err.Error(), 160)
	}
	return view.Name(m.From, msg.MaxIdentity) + " was told."
}
