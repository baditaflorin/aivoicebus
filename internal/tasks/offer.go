package tasks

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Offers (msg/offer.go): a task sent to an audience is offered to one session at a time. Decline is
// "not this session", shared by the CLI, the MCP server and the worker; Escalate tells the person a
// task nobody took, shared by the hooks and Decline.

// Declined is how a decline went.
type Declined struct {
	Task msg.Message
	ID   string     // the decline that was sent
	Held *msg.Offer // the offer this session held, as its node said just before (nil when it held none)
	// Escalated is the message that told the person, when this decline left the task with nobody else to
	// go to (or it was untaken long enough), and Told whether this decline was what sent it.
	Escalated string
	Told      bool
}

// Decline passes task id on: a fyi progress message to its sender, in its thread, saying not this
// session (and why), and the task marked read here. A node holding it ends this session's offer at
// once and offers the task to the next session. When nobody else is left to offer it to, or it was
// untaken for msg.EscalateAfter, the person is told (Escalate). A task this session sent, holds, or that
// someone else took, or one sent to it by name, is refused with what to do instead, and nothing is sent.
func Decline(c *client.Client, me string, who Caller, id, note string, warn io.Writer) (Declined, error) {
	task, ms, err := Find(c, id)
	if err != nil {
		return Declined{}, err
	}
	r := Declined{Task: task}
	switch {
	case task.From == me:
		return r, outbox.Usagef("you sent task %s; a session it reaches declines it. To take it back, run `aivoicebus cancel %s`", id, id)
	case who != Person && msg.PersonOnly(task):
		return r, &PersonOnlyError{Task: task}
	case !msg.Offered(task):
		return r, fmt.Errorf("task %s was sent to %s by name, so there is no next session to offer it to: to say it will not be done, run `aivoicebus state %s declined \"why\"`", id, view.Name(task.To, msg.MaxIdentity), id)
	}
	if owner, taken := msg.TaskOwner(task.From, Takes(ms, id)); taken {
		if owner.From == me {
			return r, fmt.Errorf("task %s is yours (claim %s): to give it back, withdraw the claim with `aivoicebus retract %s`, and the nodes offer it again", id, owner.ID, owner.ID)
		}
		return r, fmt.Errorf("task %s was already taken by %s at %s: there is nothing to pass on (`aivoicebus queue` lists the open ones)", id, view.Name(owner.From, msg.MaxIdentity), view.WhenSec(owner.At, time.Now()))
	}
	for _, m := range ms {
		if m.From == me && m.Retracted == nil && msg.IsDecline(m) {
			r.ID = m.ID
			return r, fmt.Errorf("you declined task %s already at %s (%s): to take it after all, run `aivoicebus claim %s`", id, view.WhenSec(m.At, time.Now()), m.ID, id)
		}
	}
	if inbox, err := c.Inbox(me, false); err == nil { // what this session's node says of its offer
		for _, m := range inbox {
			if m.ID == id {
				r.Held = m.Offer
			}
		}
	}
	plan, err := outbox.Prepare(c, outbox.Request{To: task.From, Re: id, Thread: task.Thread, Kind: msg.KindProgress, Prio: msg.PrioFYI, Body: msg.DeclineBody(note), TargetSaid: true}, warn)
	if err != nil {
		return r, err
	}
	out, err := plan.Send(c)
	if err != nil {
		return r, err
	}
	r.ID = out.ID
	_ = c.AckVia(id, me, msg.ViaCLI, 0) // passed on: stop showing it here
	if h := r.Held; h != nil && (h.Left == 0 || h.Escalate) {
		r.Escalated, r.Told, err = Escalate(c, me, task, h.Before+1, time.Now())
		if err != nil {
			fmt.Fprintf(warn, "  the person could not be told that nobody else is left to take it (%v): `aivoicebus status %s` shows who was offered it\n", err, id)
		}
	}
	return r, nil
}

// EscalationBody is what the person is told about task t that nobody took: who sent what where, how long
// ago, to how many sessions it was offered, the first words of it (none for a sealed one), and what to run.
// The words are the sender's (checked for credentials when they were sent), cut and flattened.
func EscalationBody(t msg.Message, offered int, now time.Time) string {
	quote := ""
	if !msg.IsSealed(t.Body) {
		if s := view.OneLine(view.Sanitise(t.Body), 200); s != "" {
			quote = fmt.Sprintf(` It says: "%s".`, strings.ReplaceAll(s, `"`, "'"))
		}
	}
	return fmt.Sprintf("%s Nobody took task %s, which %s sent to %s %s ago; the nodes offered it to %d session(s) there, one at a time.%s "+
		"To have it done, tell one of your sessions to take it (aivoicebus claim %s); to drop it, ask %s to withdraw it. aivoicebus status %s shows who was offered it.",
		msg.PersonTag, t.ID, msg.SafeName(t.From), msg.SafeName(t.To), Span(max(now.Sub(t.At), 0).Truncate(time.Minute)), max(offered, 1), quote, t.ID, msg.SafeName(t.From), t.ID)
}

// Escalate tells the person nobody took task t (msg.EscalationTo), under the id every client derives
// from the task's (msg.EscalationID), so several sessions noticing at once send one message: a node that
// holds it already refuses another, and that is "told", not an error. It lives as long as the task. told
// reports whether this call is what sent it.
func Escalate(c *client.Client, me string, t msg.Message, offered int, now time.Time) (id string, told bool, err error) {
	m := msg.Message{ID: msg.EscalationID(t.ID), From: me, To: msg.EscalationTo(t), Re: t.ID, Thread: t.Thread, Body: EscalationBody(t, offered, now)}
	if t.Priority() == msg.PrioUrgent {
		m.Prio = msg.PrioUrgent
	}
	if t.Exp != nil {
		if left := t.Exp.Sub(now); left >= time.Minute {
			m.TTL = int64(left / time.Second)
		}
	}
	if _, err := c.SendOnce(m); err != nil {
		if client.IDTaken(err) {
			return m.ID, false, nil
		}
		return m.ID, false, err
	}
	return m.ID, true, nil
}
