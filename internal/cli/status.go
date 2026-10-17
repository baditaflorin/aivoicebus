package cli

import (
	"fmt"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// statusCmd answers "did it get through?" for a message this machine sent (or any it can see):
// who read it and when, who took it if it is a task, and how many replies it has.
func statusCmd(args []string, s Streams) error {
	fs := newFlagSet("status")
	asJSON := fs.Bool("json", false, "print the receipts as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 1 || !reMsgID.MatchString(fs.Arg(0)) {
		return outbox.Usagef("usage: status MESSAGE_ID [--json]   (the id `send` or `task` printed)")
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	h, err := c.Health()
	if err == nil && !h.Has(msg.FeatureReceipts) {
		return fmt.Errorf("the node runs %s, which cannot report receipts yet (it needs 0.4.0); `aivoicebus thread %s` shows the replies", view.OneLine(h.Version, 20), fs.Arg(0))
	}
	r, err := c.Receipts(fs.Arg(0))
	if err != nil {
		if client.IsUnreachable(err) {
			return err
		}
		return outbox.NotFoundf("message %s was not found on the nodes (status takes the full id)", fs.Arg(0))
	}
	if *asJSON {
		return writeJSON(s.Out, r)
	}
	view.Receipts(s.Out, r)
	return nil
}
