package cli

import (
	"fmt"
	"sort"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// staleNow is the clock --close-stale judges a thread's quiet by (a variable so tests can move it).
var staleNow = time.Now

// closeStale ends a conversation that went quiet: no message for staleAfter, not ended, and not in
// a final state (done, declined). It writes an [end] whose body says why and sums the thread up
// (view.StaleSummary). Nothing does this on its own: only the conversation's sender, or the person
// at their own terminal, may ask for it.
func closeStale(c *client.Client, ms []msg.Message, staleAfter time.Duration, s Streams) error {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].At.Before(ms[j].At) })
	var root *msg.Message
	for i := range ms {
		if ms[i].ID == ms[i].Thread && ms[i].Re == "" {
			root = &ms[i]
		}
		if ms[i].Kind == msg.KindEnd && ms[i].Retracted == nil {
			return fmt.Errorf("thread %s is already ended (by %s): nothing to close; `aivoicebus thread %s --digest` sums it up", ms[i].Thread, view.Name(ms[i].From, msg.MaxIdentity), ms[i].Thread)
		}
	}
	if root == nil {
		return outbox.NotFoundf("the nodes do not hold the message that started this thread, so who may close it is not known: ask the user to run `aivoicebus thread %s --close-stale` in their terminal", ms[0].Thread)
	}
	me := identity.Me()
	agent, why := identity.AgentCaller()
	by := "the conversation's sender"
	switch {
	case agent && me != root.From:
		return fmt.Errorf("only the conversation's sender (%s) or the user may close it as stale, and this session (%s) is neither: ask the sender, or ask the user to run `aivoicebus thread %s --close-stale` in their terminal", view.Name(root.From, msg.MaxIdentity), why, root.ID)
	case !agent:
		by = "the user"
	}
	if v := msg.ThreadState(ms); v.Final() {
		return fmt.Errorf("thread %s is closed already: %s by %s. Nothing to close; `aivoicebus thread %s --digest` sums it up", root.ID, v.State, view.Name(v.By, msg.MaxIdentity), root.ID)
	}
	last := ms[len(ms)-1]
	idle := staleNow().Sub(last.At)
	if idle < staleAfter {
		return fmt.Errorf("thread %s is not stale: its last message is %s old, less than --stale-after %s. Run it again later, or close it now with `aivoicebus reply --end --re %s`", root.ID, idle.Round(time.Second), staleAfter, last.ID)
	}
	plan, err := outbox.Prepare(c, outbox.Request{Re: last.ID, Kind: msg.KindEnd, Body: view.StaleSummary(ms, idle, by), TargetSaid: true}, s.Err)
	if err != nil {
		return err
	}
	out, err := c.Send(plan.Msg)
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "closed thread %s as stale (quiet for %s): sent [end] %s to %s with a summary\n", root.ID, idle.Round(time.Minute), out.ID, view.Name(out.To, msg.MaxIdentity))
	return nil
}
