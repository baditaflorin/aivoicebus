package store

import (
	"errors"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Settles (msg/settle.go): the person on a machine says a message needs no one there any more. The
// claims are kept as they arrive; whether one hides a message is worked out when mail is read.

// addSettleLocked notes that the identity by settled message mid for its machine. The earliest
// settle by a machine's person is kept. Only the machine's person counts (msg.MaySettle); anything
// else is a read receipt for nothing, which is what an older node makes of every settle.
func (s *Store) addSettleLocked(mid, by string, at time.Time) {
	if !msg.IsHuman(by) || !msg.ValidIdentity(by) {
		return
	}
	host := msg.HostOf(by)
	if s.settles[mid] == nil {
		s.settles[mid] = map[string]time.Time{}
	}
	if cur, ok := s.settles[mid][host]; !ok || at.Before(cur) {
		s.settles[mid][host] = at
	}
}

// settledForLocked reports whether the person on me's machine settled m, which may be settled
// (msg.Settleable: nothing that must be answered is ever hidden, whoever wrote the claim).
func (s *Store) settledForLocked(m msg.Message, me string) bool {
	if len(s.settles) == 0 {
		return false
	}
	_, ok := s.settles[m.ID][msg.HostOf(me)]
	return ok && msg.Settleable(m)
}

// Settled lists the machines whose person settled message id, with when.
func (s *Store) Settled(id string) map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]time.Time{}
	for h, at := range s.settles[id] {
		out[h] = at
	}
	return out
}

// ErrNotSettler is what Settle says of an identity that is not a machine's person.
var ErrNotSettler = errors.New("only the person on a machine (<machine>/human) settles a message for it: run `aivoicebus done ID` in your own terminal")

// ErrMustAnswer is what Settle says of a message that must be answered (msg.MustAnswer).
var ErrMustAnswer = errors.New("that message must be answered (an approval request, a [token hand] task or a question for the person): it is never settled: answer it (`aivoicebus approve`, `aivoicebus token install` or `aivoicebus answer` with its id, in the person's terminal)")

// Settle records that the person by settles message id for their machine, as an ordinary ack (see
// msg/settle.go). It is refused for a message this node does not hold (ErrUnknown: the client asks
// another node), for one that must be answered, and for an identity that is not a machine's person.
// A second settle by the same machine is a no-op.
func (s *Store) Settle(id, by string) error {
	if !msg.ValidRetractID(id) || !msg.ValidIdentity(by) || by == QuarantineBy {
		return ErrBadSettle
	}
	if !msg.IsHuman(by) {
		return ErrNotSettler
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byIDLocked(id)
	if !ok {
		return ErrUnknown
	}
	if !msg.Settleable(m) {
		return ErrMustAnswer
	}
	if _, done := s.settles[id][msg.HostOf(by)]; done {
		return nil
	}
	return s.commitLocked(msg.Record{Ack: &msg.Ack{ID: msg.SettlePrefix + id, By: by, At: s.now().UTC()}})
}

// ErrBadSettle is what Settle says of a request that names no message or no identity.
var ErrBadSettle = errors.New("a settle names a message by its id (ten lowercase hex digits) and the person who settles it")
