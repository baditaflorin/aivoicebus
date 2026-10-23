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

// heldRead is one inbox read a session waits in (a long poll): until when, and what it accepts
// (nil: a client that does not say, msg.FeatureWaitScope).
type heldRead struct {
	until time.Time
	scope *msg.WaitScope
}

// HoldRead marks me as waiting in a held inbox read until until, for what scope accepts (nil: the
// client did not say), and returns what ends it. A session may wait in several at once (standing
// by in the background while it waits for one reply): it listens while any of them takes anything.
func (s *Store) HoldRead(me string, until time.Time, scope *msg.WaitScope) (done func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heldN++
	id := s.heldN
	if s.held[me] == nil {
		s.held[me] = map[uint64]heldRead{}
	}
	s.held[me][id] = heldRead{until: until, scope: scope}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.held[me], id)
		if len(s.held[me]) == 0 {
			delete(s.held, me)
		}
	}
}

// NoteWaiting marks me as in a held read that does not say what it accepts (an older client)
// until until (zero: no longer), for the roster.
func (s *Store) NoteWaiting(me string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if until.IsZero() {
		delete(s.held[me], 0)
		return
	}
	if s.held[me] == nil {
		s.held[me] = map[uint64]heldRead{}
	}
	s.held[me][0] = heldRead{until: until}
}

// listenLocked is what the roster says of me's held reads: listening when one accepts anything
// (or does not say), and the wait that tells it (nil when none says): an open one first, else the
// newest narrow one, which makes the session busy.
func (s *Store) listenLocked(me string, now time.Time) (listening bool, waits *msg.WaitScope) {
	var narrow uint64
	for id, h := range s.held[me] {
		if !h.until.After(now) {
			continue
		}
		switch {
		case h.scope == nil:
			listening = true
		case h.scope.Open():
			listening, waits = true, h.scope
		case !listening && (narrow == 0 || id > narrow):
			narrow = id
		}
	}
	if !listening && narrow != 0 {
		waits = s.held[me][narrow].scope
	}
	if waits != nil {
		w := *waits
		waits = &w
	}
	return listening, waits
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
