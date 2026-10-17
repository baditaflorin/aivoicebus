// Package tasks is taking a task and waiting for an answer, shared by the CLI, the MCP server and
// the worker, so every way of claiming weighs claims the same way (msg.TaskOwner).
package tasks

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// Takes lists the claims and answers in a thread that still stand, as msg.TaskOwner weighs them.
func Takes(ms []msg.Message, taskID string) []msg.TakeRef {
	var out []msg.TakeRef
	for _, m := range ms {
		if m.Re == taskID && msg.Takes(m) && m.Retracted == nil {
			out = append(out, msg.TakeRef{ID: m.ID, From: m.From, At: m.At})
		}
	}
	return out
}

// Find reads the thread a task is in and returns the task and the whole thread.
func Find(c *client.Client, id string) (msg.Message, []msg.Message, error) {
	ms, err := c.Thread(id)
	if err != nil {
		return msg.Message{}, nil, err
	}
	for _, m := range ms {
		if m.ID == id {
			if m.Kind != msg.KindTask {
				return msg.Message{}, nil, outbox.Usagef("%s is not a task (it is a %s); answer it with: aivoicebus reply --re %s", id, KindWord(m.Kind), id)
			}
			return m, ms, nil
		}
	}
	return msg.Message{}, nil, outbox.NotFoundf("task %s was not found on the nodes (claim takes the full 10-character id)", id)
}

// KindWord names a kind for a person: "plain message" for none.
func KindWord(k string) string {
	if k == "" {
		return "plain message"
	}
	return k
}

// Owner is who holds the task now, from a fresh read of its thread.
func Owner(c *client.Client, task msg.Message) (msg.TakeRef, bool, error) {
	ms, err := c.Thread(task.ID)
	if err != nil {
		return msg.TakeRef{}, false, err
	}
	owner, taken := msg.TaskOwner(task.From, Takes(ms, task.ID))
	return owner, taken, nil
}

// ErrOwnTask is a claim on a task this session sent.
var ErrOwnTask = errors.New("you sent this task; a session that receives it claims it")

// Claimed is how a claim went.
type Claimed struct {
	Task    msg.Message
	Mine    bool        // this session holds the task
	Already bool        // it held the task before this claim (nothing was sent)
	Owner   msg.TakeRef // who holds it, when not Mine
	ClaimID string      // the claim this session sent, "" when none was
}

// BeforeSend runs between the check that the task is free and the claim going out. Tests use it
// to make another session claim at that moment; it is nil otherwise.
var BeforeSend func()

// Claim takes task id for me. It sends a claim only when nobody holds the task, then waits settle
// for a claim made on another node at the same moment to replicate, and reads the thread again:
// the earliest claim wins, so a later look can find the task is someone else's after all. A task
// is marked read once it is this session's. warn gets what a send warns about (a weak credential shape).
func Claim(c *client.Client, me, id, body string, settle time.Duration, warn io.Writer) (Claimed, error) {
	task, ms, err := Find(c, id)
	if err != nil {
		return Claimed{}, err
	}
	r := Claimed{Task: task}
	if task.From == me {
		return r, ErrOwnTask
	}
	if owner, taken := msg.TaskOwner(task.From, Takes(ms, id)); taken {
		r.Owner, r.Mine, r.Already = owner, owner.From == me, owner.From == me
		return r, nil
	}
	if body = strings.TrimSpace(body); body == "" {
		body = "taking this"
	}
	if BeforeSend != nil {
		BeforeSend()
	}
	plan, err := outbox.Prepare(c, outbox.Request{To: task.From, Re: id, Thread: task.Thread, Kind: msg.KindClaim, Body: body, TargetSaid: true}, warn)
	if err != nil {
		return r, err
	}
	out, err := plan.Send(c)
	if err != nil {
		return r, err
	}
	r.ClaimID = out.ID
	time.Sleep(settle)
	owner, taken, err := Owner(c, task)
	switch {
	case err != nil: // the claim went out and nothing says otherwise: it stands
		r.Mine = true
	case taken && owner.From != me:
		r.Owner = owner
	default:
		r.Mine = true
	}
	if r.Mine {
		_ = c.AckVia(id, me, msg.ViaCLI, 0) // it is being handled: stop showing it on every prompt
	}
	return r, nil
}

// Filter says which arriving messages end a wait. With Progress set, a claim (someone took the
// task) does not end it: it is reported through Progress and marked read, and the wait goes on
// for the answer itself.
type Filter struct {
	Thread, From string
	Progress     func(msg.Message)
}

// Match reports whether m is what the wait is for.
func (f Filter) Match(m msg.Message) bool {
	if f.Thread != "" && m.Thread != f.Thread {
		return false
	}
	if f.From != "" && m.From != f.From && msg.HostOf(m.From) != f.From && !strings.HasPrefix(m.From, f.From+".") {
		return false
	}
	return true
}

// ErrTimeout is a wait that saw nothing arrive.
var ErrTimeout = errors.New("nothing arrived")

// Wait polls the inbox until unread messages that match f are there, d has passed (ErrTimeout) or
// ctx is done (ErrTimeout too). Each poll is one small request; the caller's agent spends no
// tokens while it waits.
func Wait(ctx context.Context, c *client.Client, me string, f Filter, d, every time.Duration) ([]msg.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	for {
		ms, err := c.Inbox(me, false)
		if err != nil && !client.IsUnreachable(err) {
			return nil, err
		}
		var got []msg.Message
		for _, m := range ms {
			if !f.Match(m) {
				continue
			}
			if m.Kind == msg.KindClaim && f.Progress != nil {
				f.Progress(m)
				_ = c.AckVia(m.ID, me, msg.ViaCLI, 0)
				continue
			}
			got = append(got, m)
		}
		if len(got) > 0 {
			sort.SliceStable(got, func(i, j int) bool { return got[i].At.Before(got[j].At) })
			return got, nil
		}
		select {
		case <-ctx.Done():
			return nil, ErrTimeout
		case <-time.After(every):
		}
	}
}
