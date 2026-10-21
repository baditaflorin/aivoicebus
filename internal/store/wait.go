package store

import (
	"sort"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Gen is the node's change counter and a channel closed at the next change: a reader that saw
// gen waits on the channel for anything newer.
func (s *Store) Gen() (uint64, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen, s.changed
}

// NoteWaiting marks me as listening until until (zero: no longer), for the roster.
func (s *Store) NoteWaiting(me string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.seen[me]; ok {
		v.waitingUntil = until
		s.seen[me] = v
	}
}

func hasReader(rs []msg.Receipt, by string) bool {
	for _, r := range rs {
		if r.By == by {
			return true
		}
	}
	return false
}

// Receipts says whether message id got through: who read it and when (its sender excluded), who
// took it when it is a task, and how many replies answer it. False when this node does not hold it.
func (s *Store) Receipts(id string) (msg.Receipts, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byIDLocked(id)
	if !ok {
		return msg.Receipts{}, false
	}
	r := msg.Receipts{ID: m.ID, From: m.From, To: m.To, Kind: m.Kind, At: m.At, Reach: m.Reach, Reads: []msg.Receipt{}, Expired: m.Expired(s.now().UTC())}
	for by := range s.acks[id] {
		if by == m.From {
			continue
		}
		rc, known := s.ackAt[id][by]
		if !known {
			rc = msg.Receipt{By: by}
		}
		r.Reads = append(r.Reads, rc)
	}
	sort.Slice(r.Reads, func(i, j int) bool {
		if !r.Reads[i].At.Equal(r.Reads[j].At) {
			return r.Reads[i].At.Before(r.Reads[j].At)
		}
		return r.Reads[i].By < r.Reads[j].By
	})
	if m.Kind == msg.KindTask {
		if o, taken := s.taskOwnerLocked(m); taken {
			r.Owner = &msg.Receipt{By: o.From, At: o.At}
		}
	}
	for _, i := range s.threads[m.Thread] {
		t := s.msgs[i]
		if t.Re != id || t.ID == id {
			continue
		}
		if _, replaces := s.validAmendLocked(t); replaces { // the sender's own correction is no answer, and its writer no reader
			continue
		}
		r.Replies++
		// Answering a message means it got through, even when the reader never acknowledged it
		// (`reply --re` without --ack): count the first answer as that session's read.
		if t.From != m.From && !s.acks[id][t.From] && !hasReader(r.Reads, t.From) {
			r.Reads = append(r.Reads, msg.Receipt{By: t.From, At: t.At, Via: msg.ViaReply})
		}
	}
	sort.SliceStable(r.Reads, func(i, j int) bool { return r.Reads[i].At.Before(r.Reads[j].At) })
	return r, true
}

// Leases lists the live leases: not withdrawn, not expired, not forged. msg.Holders says who holds what.
func (s *Store) Leases() []msg.Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	out := []msg.Lease{}
	for _, i := range s.leases {
		m := s.msgs[i]
		if s.forged[m.ID] || m.Expired(now) || s.retractedLocked(m.ID, m.From) {
			continue
		}
		if l, ok := msg.LeaseOf(m); ok {
			out = append(out, l)
		}
	}
	return out
}
