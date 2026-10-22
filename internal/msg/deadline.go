package msg

import "time"

// A deadline is the sender's own promise to find out what happened to a message: it is never on the
// wire (the message carries no marker, so no node and no reader needs to know about it) and it is
// judged here from the receipts a node reports (receipts.go). The review of a week of the hub's
// messages found every "reply within N minutes" window missed and the sender going ahead anyway,
// because "nobody objected" and "nobody saw it" look the same from the sender's side.

// The two kinds of deadline: a reply-by (silence is the good outcome, if someone read it) and an
// offer's until (a task nobody takes in time lapses).
const (
	DeadlineReply = "reply"
	DeadlineTake  = "take"
)

// The outcomes JudgeDeadline gives.
const (
	OutcomePending     = "pending"      // the deadline has not come and nothing has happened
	OutcomeObjected    = "objected"     // somebody replied
	OutcomeNoObjection = "no-objection" // it was read and nobody replied
	OutcomeNobodyRead  = "nobody-read"  // the deadline passed and nobody had read it: silence is not consent
	OutcomeTaken       = "taken"        // an offer someone took
	OutcomeNoTaker     = "no-taker"     // an offer nobody took in time
)

// MaxSenderDeadline bounds a reply-by and an until: a message or an offer that nobody settles for a day is
// not a deadline, and the default lifetime of an fyi is a day.
const MaxSenderDeadline = 24 * time.Hour

// Judgement is what a deadline came to.
type Judgement struct {
	Outcome string    `json:"outcome"`
	By      time.Time `json:"by"`
	// Read is how many sessions read the message at or before the deadline (a receipt with no time,
	// from an older node, counts); Of how many it could reach when it was sent (nil: not known).
	Read int  `json:"read"`
	Of   *int `json:"of,omitempty"`
	// Replies is how many answers it has, from the message and from any reminder sent for it. The
	// client cannot tell "stop" from "fine, go": any reply is the sender's cue to look.
	Replies int `json:"replies"`
	// LateReads is how many sessions read it only after the deadline.
	LateReads int `json:"late_reads,omitempty"`
	// TakenBy is who took an offer.
	TakenBy string `json:"taken_by,omitempty"`
}

// Final reports whether the outcome can no longer change.
func (j Judgement) Final() bool { return j.Outcome != OutcomePending }

// JudgeDeadline judges a deadline of kind by the receipts of the message (first) and of the
// reminders sent for it (rs[1:]), as of now. Only a receipt of the message itself is a read in time:
// a reminder is sent after the deadline, so what reads it is late. A count-only receipt (an fyi the
// session start only counted) is no read. An answer counts as a read (the node adds it), and a reply
// settles a reply-by at once, before the deadline.
func JudgeDeadline(kind string, rs []Receipts, by, now time.Time) Judgement {
	j := Judgement{Outcome: OutcomePending, By: by}
	if len(rs) == 0 {
		return j
	}
	j.Of = rs[0].Reach
	late := map[string]bool{}
	inTime := map[string]bool{}
	for i, r := range rs {
		j.Replies += r.Replies
		for _, rd := range r.Reads {
			if rd.Via == ViaHookCount || rd.By == r.From {
				continue
			}
			if i == 0 && (rd.At.IsZero() || !rd.At.After(by)) {
				inTime[rd.By] = true
			} else {
				late[rd.By] = true
			}
		}
	}
	j.Read = len(inTime)
	for who := range late {
		if !inTime[who] {
			j.LateReads++
		}
	}
	if kind == DeadlineTake {
		switch {
		case rs[0].Owner != nil:
			j.Outcome, j.TakenBy = OutcomeTaken, rs[0].Owner.By
		case !now.Before(by):
			j.Outcome = OutcomeNoTaker
		}
		return j
	}
	switch {
	case j.Replies > 0:
		j.Outcome = OutcomeObjected
	case now.Before(by):
	case j.Read > 0:
		j.Outcome = OutcomeNoObjection
	default:
		j.Outcome = OutcomeNobodyRead
	}
	return j
}
