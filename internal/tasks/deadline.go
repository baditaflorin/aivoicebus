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

// A sender that asks for a deadline (send --reply-by, task --until) finds out at the deadline what
// became of the message, instead of taking silence for consent. The judgement is msg.JudgeDeadline;
// this file asks the nodes for the receipts, says the result in words, finds the deadlines nobody was
// waiting on (for `watch`) and sends the reminders of an escalation.

// JudgeDeadline judges d as of now from the receipts of its message and of the reminders sent for it.
// A reminder the nodes no longer know is skipped; the message itself failing is the error: nothing
// can be said about a deadline whose message the nodes cannot report on.
func JudgeDeadline(c *client.Client, d outbox.Deadline, now time.Time) (msg.Judgement, error) {
	r, err := c.Receipts(d.ID)
	if err != nil {
		return msg.Judgement{}, err
	}
	if r.Reach == nil {
		r.Reach = d.Reach
	}
	rs := []msg.Receipts{r}
	for _, id := range d.Reminders {
		if rr, err := c.Receipts(id); err == nil {
			rs = append(rs, rr)
		}
	}
	return msg.JudgeDeadline(d.Kind, rs, d.By, now), nil
}

// ReadClause is "read by 0 of 1, replies 0": what the receipts say, in the words of the report.
func ReadClause(j msg.Judgement) string {
	of := ""
	if j.Of != nil {
		of = fmt.Sprintf(" of %d", *j.Of)
	}
	late := ""
	if j.LateReads > 0 {
		late = fmt.Sprintf(" (and %d only after the deadline)", j.LateReads)
	}
	return fmt.Sprintf("read by %d%s%s, replies %d", j.Read, of, late, j.Replies)
}

// Span is d as a short span for a sentence: 15m, not 15m0s; 25h, not 25h0m0s; 90s stays 1m30s.
func Span(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// DeadlineClause is the verdict of d as a sentence for the sender, with no message id in it (the
// caller knows which message): `status ID` writes it under the receipts.
func DeadlineClause(d outbox.Deadline, j msg.Judgement, now time.Time) string {
	by := view.WhenSec(j.By, now)
	if d.Kind == msg.DeadlineTake {
		switch j.Outcome {
		case msg.OutcomeTaken:
			return fmt.Sprintf("offered until %s: taken by %s", by, msg.SafeName(j.TakenBy))
		case msg.OutcomeNoTaker:
			return fmt.Sprintf("offered until %s (%s): no taker, so it expired and is no longer delivered: not doing it", by, Span(d.By.Sub(d.Sent)))
		}
		return fmt.Sprintf("offered until %s: not taken yet", by)
	}
	switch j.Outcome {
	case msg.OutcomeNobodyRead:
		return fmt.Sprintf("reply-by %s passed: %s: nobody read it, so silence is not consent", by, ReadClause(j))
	case msg.OutcomeNoObjection:
		return fmt.Sprintf("reply-by %s passed: %s: it was read and nobody objected", by, ReadClause(j))
	case msg.OutcomeObjected:
		return fmt.Sprintf("reply-by %s: %s: somebody replied (aivoicebus thread %s)", by, ReadClause(j), d.Thread)
	}
	return fmt.Sprintf("reply-by %s is %s away: %s so far", by, Span(max(d.By.Sub(now), 0)), ReadClause(j))
}

// DeadlineText is the verdict of d as one line for a person or an agent (a terminal, a phone): it
// names ids and sessions, never what the message said.
func DeadlineText(d outbox.Deadline, j msg.Judgement, now time.Time) string {
	line := fmt.Sprintf("aivoicebus: message %s to %s: %s", d.ID, msg.SafeName(d.To), DeadlineClause(d, j, now))
	if Alarming(j) {
		line += ". aivoicebus status " + d.ID
	}
	return line
}

// Alarming reports whether the outcome is one a person should be told about unasked: nobody read it,
// somebody replied, or an offer lapsed. A message that was read with no reply, or an offer that was
// taken, is good news and only `status` shows it.
func Alarming(j msg.Judgement) bool {
	switch j.Outcome {
	case msg.OutcomeNobodyRead, msg.OutcomeObjected, msg.OutcomeNoTaker:
		return true
	}
	return false
}

// DueReport is a deadline that passed with nobody reporting it, and what it came to.
type DueReport struct {
	Deadline outbox.Deadline
	Judged   msg.Judgement
	Line     string
}

// Due lists the deadlines the machine host remembers that have passed and were not reported, judged
// at now. A deadline the nodes cannot report on is left for the next look; an unreachable mesh stops
// the look and returns what it had.
func Due(c *client.Client, host string, now time.Time) ([]DueReport, error) {
	var out []DueReport
	for _, d := range outbox.DeadlinesFrom(host) {
		if !d.Reported.IsZero() || now.Before(d.By) {
			continue
		}
		j, err := JudgeDeadline(c, d, now)
		if err != nil {
			if client.IsUnreachable(err) {
				return out, err
			}
			if now.Sub(d.By) > staleDeadline { // the nodes forgot the message: nothing will ever be said about it
				outbox.MarkReported(host, d.ID, now)
			}
			continue
		}
		out = append(out, DueReport{Deadline: d, Judged: j, Line: DeadlineText(d, j, now)})
	}
	return out, nil
}

// staleDeadline is how long past its deadline a deadline the nodes can no longer report on is looked for
// before `watch` gives it up (an expired or withdrawn message the nodes forgot).
const staleDeadline = 24 * time.Hour

// MaxReminders is how many live sessions an escalation re-delivers a message to.
const MaxReminders = 3

// utcClock is t as a UTC clock time, with the date when it is not today (UTC), for a reader on
// another machine whose own zone is not the sender's.
func utcClock(t, now time.Time) string {
	t, now = t.UTC(), now.UTC()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04:05") + "Z"
	}
	return t.Format("2006-01-02 15:04:05") + "Z"
}

// ReminderBody is what an escalation sends to a live session in place of the message nobody read: who
// sent what to whom, by when a reply was asked for, the first words of it (none for a private message),
// and how to answer. The words are the sender's own (already checked for credentials when they were
// sent) and are cut and flattened.
func ReminderBody(d outbox.Deadline, said string, now time.Time) string {
	quote := ""
	if said != "" { // a --private message's words are never repeated in the clear
		quote = fmt.Sprintf(" It said: %q.", view.OneLine(said, 400))
	}
	return fmt.Sprintf("[reminder] %s sent a message to %s (id %s, thread %s) that asks for a reply by %s, and nobody has read it.%s Answer it with: aivoicebus reply --re %s \"...\"",
		msg.SafeName(d.From), msg.SafeName(d.To), d.ID, d.Thread, utcClock(d.By, now), quote, d.ID)
}

// Remind sends an urgent reminder of d, in its thread and replying to it, to each of targets, and
// returns the ids that went. An urgent message shows at the target's next prompt, which is the point;
// a handful of sessions never trips the urgent fan-out guard. A target that fails is reported to warn
// and skipped, so one dead session does not stop the rest.
func Remind(c *client.Client, d outbox.Deadline, said string, targets []string, now time.Time, warn io.Writer) ([]string, error) {
	var ids []string
	var firstErr error
	for _, to := range targets {
		p, err := outbox.Prepare(c, outbox.Request{To: to, Re: d.ID, Thread: d.Thread, Prio: msg.PrioUrgent, Body: ReminderBody(d, said, now), TargetSaid: true}, warn)
		if err == nil {
			var out msg.Message
			if out, err = p.Send(c); err == nil {
				ids = append(ids, out.ID)
				continue
			}
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("reminding %s: %w", msg.SafeName(to), err)
		}
		fmt.Fprintf(warn, "  could not remind %s: %v\n", view.Name(to, msg.MaxIdentity), err)
	}
	if len(ids) == 0 {
		return nil, firstErr
	}
	return ids, nil
}
