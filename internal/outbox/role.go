package outbox

import (
	"fmt"
	"io"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// ResolveRole finds the live session holding role (a send to @role goes to it). The nodes' roster
// is the only source: a node too old to have one cannot say who holds a role, and nothing is
// guessed. The resolution is filled in as far as it got when there is an error (see msg.ResolveRole).
func ResolveRole(c *client.Client, role, me string, o msg.RoleOpts) (msg.RoleResolution, error) {
	roster, err := c.RosterAll() // a decision, not a display: probes are counted
	switch {
	case client.IsNotFound(err):
		return msg.RoleResolution{Role: role}, fmt.Errorf("@%s cannot be resolved: this node does not report who is around: send to the session by identity (`aivoicebus who` lists them), or ask the user to update the nodes", role)
	case err != nil:
		return msg.RoleResolution{Role: role}, fmt.Errorf("@%s cannot be resolved: the nodes could not say who is around (%v): send to the session by identity, or try again", role, err)
	}
	text, err := config.ReadRoles()
	if err != nil { // a roles file that cannot be trusted is no pin: say so rather than fall back to none
		return msg.RoleResolution{Role: role}, fmt.Errorf("@%s cannot be resolved: the roles file is unusable: %v", role, err)
	}
	return msg.ResolveRole(role, roster, msg.ParseRolePins(text), me, time.Now(), o)
}

// WhoRole prints who a send to @role would reach, for `who @role`: every holder, then the outcome.
func WhoRole(w io.Writer, c *client.Client, role, me string) {
	res, err := ResolveRole(c, role, me, msg.RoleOpts{}) // as a plain send would see it: an unpinned role is explained, not resolved
	now := time.Now()
	for _, h := range res.Live {
		fmt.Fprintf(w, "%-32s last seen %s  holds @%s\n", msg.SafeName(h.Me), view.When(h.Seen, now), role)
	}
	for _, h := range res.Stale {
		fmt.Fprintf(w, "%-32s last seen %s  holds @%s (stale: %s ago)\n", msg.SafeName(h.Me), view.When(h.Seen, now), role, msg.SpanWords(now.Sub(h.Seen)))
	}
	for _, me := range res.Out {
		fmt.Fprintf(w, "%-32s claims @%s but is outside your pin: ignored\n", msg.SafeName(me), role)
	}
	if err != nil {
		fmt.Fprintln(w, "("+err.Error()+")")
		return
	}
	fmt.Fprintln(w, "("+res.Describe()+")")
}
