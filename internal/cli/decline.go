package cli

import (
	"fmt"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// declineCmd is `aivoicebus decline TASK_ID ["why"]`: not this session. The nodes offer a task sent to an
// audience to one session at a time (msg/offer.go); a decline passes it to the next one at once instead of
// after msg.OfferWindow, and tells the sender why. It is not the conversation state `declined`, which says
// the work will not be done at all.
func declineCmd(args []string, s Streams) error {
	fs := newFlagSet("decline")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() < 1 || !reMsgID.MatchString(fs.Arg(0)) {
		return outbox.Usagef(`usage: decline TASK_ID ["why not this session"]   (the next session is offered it at once)`)
	}
	id := fs.Arg(0)
	c, err := sessionClient()
	if err != nil {
		return err
	}
	r, err := tasks.Decline(c, identity.Me(), claimer(), id, strings.Join(fs.Args()[1:], " "), s.Err)
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "declined task %s (%s): %s is told, and the next session is offered it\n", id, r.ID, view.Name(r.Task.From, msg.MaxIdentity))
	if h, herr := c.Health(); herr == nil && !h.Has(msg.FeatureOffers) {
		fmt.Fprintf(s.Out, "  the node runs %s, which shows the task to every session anyway (offers came with the release after 0.13.1)\n", view.OneLine(h.Version, 20))
	}
	switch {
	case r.Told:
		fmt.Fprintf(s.Out, "  nobody else is about to take it: the person on %s was told (%s)\n", view.Name(msg.HostOf(msg.EscalationTo(r.Task)), msg.MaxIdentity), r.Escalated)
	case r.Escalated != "":
		fmt.Fprintf(s.Out, "  the person was told already (%s)\n", r.Escalated)
	}
	return nil
}
