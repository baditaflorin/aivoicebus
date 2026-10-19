package tasks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// StateWait is how a wait for a conversation state ended.
type StateWait struct {
	View    msg.StateView // the thread's state when the wait ended
	Reached bool          // it got to the state waited for, or past it on the main line
	// Arrived holds ordinary messages to this session in the thread (an answer, a question) that came
	// before the state did: the other side has something to say, so the wait ends for it. Unread.
	Arrived []msg.Message
}

// stateRecheck is how often a wait for a state reads the whole thread even when nothing arrived for
// this session: a state said to someone else (a third session watching) still counts.
const stateRecheck = 30 * time.Second

// WaitState waits up to d until the thread holding message id gets to the conversation state want
// (msg.StateView.Reached), becomes final without getting there, turns stuck while it waits, or an
// ordinary message to me arrives in it. A thread that is stuck already when the wait starts is waited
// on until it moves. Claims and progress are reported through progress; state messages for me are
// marked read. ErrTimeout when none of that happened in d (View is the last state seen).
func WaitState(ctx context.Context, c *client.Client, me, id, want string, d, every time.Duration, progress func(msg.Message)) (StateWait, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	if progress == nil {
		progress = func(msg.Message) {} // claims and progress never end the wait
	}
	var last StateWait
	thread := ""
	stuckAt := time.Time{} // the stuck step that was there already: only a newer one ends the wait
	first := true
	for {
		ms, err := c.Thread(id)
		switch {
		case err == nil && len(ms) > 0:
			v := msg.ThreadState(ms)
			last.View = v
			for _, m := range ms {
				if m.ID == id {
					thread = m.Thread
				}
			}
			if first && v.State == msg.StateStuck && want != msg.StateStuck {
				stuckAt = v.At
			}
			first = false
			newlyStuck := v.State == msg.StateStuck && v.At.After(stuckAt)
			if v.Reached(want) || !v.CanReach(want) || newlyStuck {
				last.Reached = v.Reached(want)
				return last, nil
			}
		case err != nil && !client.IsUnreachable(err):
			return last, err
		}
		if thread == "" {
			thread = id
		}
		chunk := min(time.Until(deadlineOf(ctx)), stateRecheck)
		if chunk < time.Second {
			return last, ErrTimeout
		}
		got, err := Wait(ctx, c, me, Filter{Thread: thread, Progress: progress, Stop: isState}, chunk, every)
		if errors.Is(err, ErrTimeout) {
			if ctx.Err() != nil {
				return last, ErrTimeout
			}
			continue
		}
		if err != nil {
			return last, err
		}
		for _, m := range got {
			if isState(m) {
				_ = c.AckVia(m.ID, me, msg.ViaCLI, 0)
				progress(m)
				continue
			}
			last.Arrived = append(last.Arrived, m)
		}
		if len(last.Arrived) > 0 {
			if ms, err := c.Thread(id); err == nil && len(ms) > 0 {
				last.View = msg.ThreadState(ms)
				last.Reached = last.View.Reached(want)
			}
			return last, nil
		}
	}
}

func isState(m msg.Message) bool { _, _, ok := msg.StateOf(m); return ok }

func deadlineOf(ctx context.Context) time.Time {
	d, _ := ctx.Deadline()
	return d
}

// ErrRefused is a state move the rules do not allow (msg.StateView.CheckMove); nothing was sent.
var ErrRefused = errors.New("refused") // nextstep: MoveState wraps it with the rule's reason, which says what to do instead

// MoveState checks the move against the thread's state and sends it: a progress message to the
// other side, in the thread of id, replying to id.
func MoveState(c *client.Client, me, id, state, note string, warn io.Writer) (msg.Message, error) {
	ms, err := c.Thread(id)
	if err != nil {
		return msg.Message{}, err
	}
	var at msg.Message
	for _, m := range ms {
		if m.ID == id {
			at = m
		}
	}
	if at.ID == "" {
		return msg.Message{}, outbox.NotFoundf("message %s was not found on the nodes: check the id with `aivoicebus inbox --all`, or try again in a moment (it may not have reached this node yet)", id)
	}
	v := msg.ThreadState(ms)
	if err := v.CheckMove(me, state); err != nil {
		return msg.Message{}, fmt.Errorf("%w, nothing was sent: %v", ErrRefused, err)
	}
	to := v.Offerer // the taker tells whoever offered it
	if me == v.Offerer {
		to = v.Taker
		if to == "" {
			for _, m := range ms {
				if m.ID == v.Root {
					to = m.To
				}
			}
		}
	}
	if to == "" || to == me {
		to = at.From
	}
	plan, err := outbox.Prepare(c, outbox.Request{To: to, Re: id, Thread: at.Thread, Kind: msg.KindProgress, Body: msg.StateBody(state, note), TargetSaid: true}, warn)
	if err != nil {
		return msg.Message{}, err
	}
	out, err := plan.Send(c)
	if err != nil {
		return msg.Message{}, err
	}
	if out.ID != plan.Msg.ID {
		fmt.Fprintf(warn, "  the same words went to %s within the last 10 minutes as %s, and the node keeps one copy: say it with another note to move it again\n", to, out.ID)
	}
	return out, nil
}
