package cli

import (
	"fmt"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// viaWords says how a reader was shown a message, for status.
var viaWords = map[string]string{
	msg.ViaHook:      "shown at a session start or prompt",
	msg.ViaHookCount: "only counted at a session start (fyi)",
	msg.ViaInbox:     "listed by aivoicebus inbox",
	msg.ViaCLI:       "read with the CLI (wait, claim, ack)",
}

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
	kind := ""
	if r.Kind != "" {
		kind = " [" + r.Kind + "]"
	}
	fmt.Fprintf(s.Out, "%s%s from %s to %s, sent %s\n", r.ID, kind, view.Name(r.From, msg.MaxIdentity), view.Name(r.To, msg.MaxIdentity), r.At.Local().Format("01-02 15:04:05"))
	if r.Reach != nil {
		fmt.Fprintf(s.Out, "  it could reach %d session(s) when it was sent\n", *r.Reach)
	}
	if len(r.Reads) == 0 {
		fmt.Fprintln(s.Out, "  not read yet by anyone")
	} else {
		fmt.Fprintf(s.Out, "  read by %d:\n", len(r.Reads))
		for _, rd := range r.Reads {
			when := "time not known"
			if !rd.At.IsZero() {
				when = rd.At.Local().Format("15:04:05") + " (" + time.Since(rd.At).Round(time.Second).String() + " ago)"
			}
			how := viaWords[rd.Via]
			if how == "" {
				how = view.OneLine(rd.Via, 20)
			}
			fmt.Fprintf(s.Out, "    %-40s %s, %s\n", view.Name(rd.By, msg.MaxIdentity), when, how)
		}
	}
	switch {
	case r.Kind == msg.KindTask && r.Owner != nil:
		fmt.Fprintf(s.Out, "  taken by %s at %s\n", view.Name(r.Owner.By, msg.MaxIdentity), r.Owner.At.Local().Format("15:04:05"))
	case r.Kind == msg.KindTask:
		fmt.Fprintf(s.Out, "  not taken yet: `aivoicebus ping %s` shows who is there\n", view.Name(r.To, msg.MaxIdentity))
	}
	if r.Replies > 0 {
		fmt.Fprintf(s.Out, "  %d repl%s: aivoicebus thread %s\n", r.Replies, map[bool]string{true: "y", false: "ies"}[r.Replies == 1], r.ID)
	}
	if r.Expired {
		fmt.Fprintln(s.Out, "  expired: it is no longer delivered")
	}
	return nil
}
