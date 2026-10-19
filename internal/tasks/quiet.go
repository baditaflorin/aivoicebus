package tasks

import (
	"fmt"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// A task that was taken and then went quiet is the one nobody hears about: watch's "untaken"
// alert has passed, and the sender's wait shows a claim and then nothing. Quiet-after is how long
// the session that took it may say nothing (no progress, answer or end) before its sender is told.

// QuietAfter is the default: a worker and a standby session say how it goes every few minutes,
// so ten minutes of nothing means something is wrong.
const QuietAfter = 10 * time.Minute

// Silent is a task this machine sent that someone took and then said nothing more about.
type Silent struct {
	ID, From, To, By  string
	TakenAt, LastWord time.Time
}

// LastWord reads a task's thread: who took it and when anyone other than its sender last said how
// it goes (a claim or progress). open is false once it was answered, ended, asked back about
// (any other message from someone else) or taken back.
func LastWord(task msg.Message, ms []msg.Message) (owner msg.TakeRef, last time.Time, taken, open bool) {
	if task.Retracted != nil {
		return owner, last, false, false
	}
	owner, taken = msg.TaskOwner(task.From, Takes(ms, task.ID))
	if !taken {
		return owner, last, false, true
	}
	last = owner.At
	for _, x := range ms {
		if x.ID == task.ID || x.From == task.From || x.Retracted != nil || x.At.Before(task.At) {
			continue
		}
		if x.Kind != msg.KindClaim && x.Kind != msg.KindProgress {
			return owner, last, true, false // answered, ended or asked back: the ball is not with them
		}
		if x.At.After(last) {
			last = x.At
		}
	}
	return owner, last, true, true
}

// SilentTasks lists the tasks the machine host sent lately (what outbox remembers) that someone
// took and has said nothing about for quiet.
func SilentTasks(c *client.Client, host string, quiet time.Duration, now time.Time) ([]Silent, error) {
	var out []Silent
	for _, s := range outbox.SentFrom(host) {
		if now.Sub(s.At) < quiet {
			continue
		}
		r, err := c.Receipts(s.ID)
		if err != nil {
			if client.IsUnreachable(err) {
				return out, err
			}
			continue
		}
		if r.Kind != msg.KindTask || r.Owner == nil || r.Expired {
			continue
		}
		task, ms, err := Find(c, s.ID)
		if err != nil {
			continue
		}
		owner, last, taken, open := LastWord(task, ms)
		if taken && open && now.Sub(last) >= quiet {
			out = append(out, Silent{ID: s.ID, From: s.From, To: r.To, By: owner.From, TakenAt: owner.At, LastWord: last})
		}
	}
	return out, nil
}

// SilentText is one silent task as a line for a person. It names ids, sessions and times only,
// never what the task or anyone in its thread said.
func SilentText(s Silent, now time.Time) string {
	return fmt.Sprintf("aivoicebus: task %s to %s was taken by %s at %s and has been silent since %s (%s): no progress, answer or end. aivoicebus status %s",
		s.ID, msg.SafeName(s.To), msg.SafeName(s.By), s.TakenAt.Local().Format("15:04"), s.LastWord.Local().Format("15:04"),
		outbox.SpanText(now.Sub(s.LastWord).Round(time.Minute)), s.ID)
}
