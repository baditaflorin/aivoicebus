package store

import (
	"errors"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Amendments (msg/amend.go): a message its sender sent to replace an earlier one. The claims are
// kept as they arrive, whether or not their message is held and whether or not their author may make
// them; what they come to is worked out when mail is read, so the outcome does not depend on the order
// records arrived in, and nothing is stored beyond the claims themselves.

// amendClaim is one identity's claim that a message replaces its re: the earliest time it made it.
type amendClaim struct {
	by string
	at time.Time
}

// maxAmendChain bounds a walk up the chain of amendments: ids are chosen by the sender, so a loop
// is possible, and a long chain is the sender's own doing.
const maxAmendChain = 32

// addAmendClaimLocked notes that by claims message aid replaces the one it answers. A second claim by
// the same identity keeps the earlier time.
func (s *Store) addAmendClaimLocked(aid, by string, at time.Time) {
	cs := s.amendClaims[aid]
	for i := range cs {
		if cs[i].by == by {
			if at.Before(cs[i].at) {
				cs[i].at = at
			}
			return
		}
	}
	s.amendClaims[aid] = append(cs, amendClaim{by: by, at: at})
	if i, held := s.idx[aid]; held {
		s.indexAmendLocked(s.msgs[i])
	}
}

// indexAmendLocked lists a, a message held that has a claim, under the message it answers, so a read
// of that message finds it without looking at every claim.
func (s *Store) indexAmendLocked(a msg.Message) {
	if a.Re == "" {
		return
	}
	for _, id := range s.amendsOf[a.Re] {
		if id == a.ID {
			return
		}
	}
	s.amendsOf[a.Re] = append(s.amendsOf[a.Re], a.ID)
}

// validAmendLocked is the message a replaces, when a is a valid amendment of it: both are held, a
// may replace it (msg.AmendmentPair), a claim by an identity on the machine that sent a is in the
// log, neither is forged, and a is not retracted.
func (s *Store) validAmendLocked(a msg.Message) (msg.Message, bool) {
	claims := s.amendClaims[a.ID]
	if len(claims) == 0 || a.Re == "" {
		return msg.Message{}, false
	}
	o, held := s.byIDLocked(a.Re)
	if !held || !msg.AmendmentPair(a, o) || s.forged[a.ID] || s.forged[o.ID] {
		return msg.Message{}, false
	}
	authorised := false
	for _, c := range claims {
		if msg.MayAmend(c.by, a.From) {
			authorised = true
			break
		}
	}
	if !authorised || s.retractedLocked(a.ID, a.From) {
		return msg.Message{}, false
	}
	return o, true
}

// amendmentOfLocked is the effective amendment of o: the latest valid one by (time, id).
func (s *Store) amendmentOfLocked(o msg.Message) (msg.Message, bool) {
	var best msg.Message
	found := false
	for _, id := range s.amendsOf[o.ID] {
		a, held := s.byIDLocked(id)
		if !held {
			continue
		}
		if _, ok := s.validAmendLocked(a); !ok {
			continue
		}
		if !found || a.At.After(best.At) || (a.At.Equal(best.At) && a.ID > best.ID) {
			best, found = a, true
		}
	}
	return best, found
}

// supersededLocked reports whether m was replaced, so a reader that has not read it is given what
// replaced it and not m: it has an effective amendment, or it is itself an amendment that lost to a
// later sibling.
func (s *Store) supersededLocked(m msg.Message) bool {
	if len(s.amendsOf) == 0 {
		return false
	}
	if _, ok := s.amendmentOfLocked(m); ok {
		return true
	}
	if o, ok := s.validAmendLocked(m); ok {
		if eff, ok := s.amendmentOfLocked(o); ok && eff.ID != m.ID {
			return true
		}
	}
	return false
}

// readVersionLocked is the nearest version before a, up its chain of amendments, that reader me read
// ("" if none): an fyi it only had counted does not count, as for a retraction notice.
func (s *Store) readVersionLocked(a msg.Message, me string) string {
	cur := a
	for range maxAmendChain {
		o, ok := s.validAmendLocked(cur)
		if !ok {
			return ""
		}
		if s.acks[o.ID][me] && !s.countOnly[o.ID][me] {
			return o.ID
		}
		cur = o
	}
	return ""
}

// replacedByShapeLocked is the set of messages m would replace if it is an amendment: the message it
// answers when that is its own machine's plain note to the same target in the same thread (the claim
// that makes it an amendment comes after it is sent, so only its shape is known), and the versions
// that message itself replaced. Only the first step is a guess: a chain of plain replies to oneself
// is no chain of amendments, so a loop of those is still seen, one message later than a loop of new messages.
func (s *Store) replacedByShapeLocked(m msg.Message) map[string]bool {
	p, ok := s.byIDLocked(m.Re)
	if !ok || !msg.AmendmentPair(m, p) {
		return nil
	}
	out := map[string]bool{p.ID: true}
	cur := p
	for range maxAmendChain {
		o, ok := s.validAmendLocked(cur)
		if !ok {
			break
		}
		out[o.ID] = true
		cur = o
	}
	return out
}

// amendViewLocked is m as a reader is shown it: itself, with Amends when it replaces an earlier
// message (and which version reader me had read, unless me is "": a thread read has no reader) and
// AmendedBy when it was replaced. The stored message is never changed.
func (s *Store) amendViewLocked(m msg.Message, me string) msg.Message {
	if len(s.amendsOf) == 0 {
		return m
	}
	if eff, ok := s.amendmentOfLocked(m); ok {
		m.AmendedBy = &msg.Amended{By: eff.ID, At: eff.At}
	}
	if o, ok := s.validAmendLocked(m); ok {
		m.Amends = &msg.Amends{Of: o.ID}
		if me != "" {
			m.Amends.Read = s.readVersionLocked(m, me)
		}
	}
	return m
}

// Errors Amend gives besides the ones Retract does (ErrUnknown).
var (
	// ErrBadAmend is what Amend says of an id that names nothing amendable, or an identity that cannot
	// claim to be anyone.
	ErrBadAmend = errors.New("not a message id (ten lowercase hex digits) and an identity")
	// ErrNotAmendment is what Amend says of a message that cannot replace the one it answers.
	ErrNotAmendment = errors.New("that message does not replace the one it answers: an amendment goes to the same target, in the same thread, from the same machine, and both are plain notes")
)

// ErrNotAmender is what Amend says when the identity is not on the machine that sent the message.
type ErrNotAmender struct{ Sender string }

func (e *ErrNotAmender) Error() string {
	return "only a session on " + msg.HostOf(e.Sender) + ", the machine that sent this message, can amend it"
}

// Amend records that by claims message aid replaces the message it answers, and says nothing when
// that claim is already held. The record is an ordinary ack (see msg/amend.go), committed like any
// other, and takes effect on this node at once. It is refused for a message this node does not hold,
// or whose original it does not hold (ErrUnknown: the client asks another node), for one that is no
// amendment (ErrNotAmendment) and for an identity not on the machine that sent it (*ErrNotAmender).
func (s *Store) Amend(aid, by string) error {
	if !msg.ValidRetractID(aid) || !msg.ValidIdentity(by) || by == QuarantineBy {
		return ErrBadAmend
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byIDLocked(aid)
	if !ok {
		return ErrUnknown
	}
	if !msg.MayAmend(by, a.From) {
		return &ErrNotAmender{Sender: a.From}
	}
	if a.Re == "" {
		return ErrNotAmendment
	}
	o, ok := s.byIDLocked(a.Re)
	if !ok {
		return ErrUnknown
	}
	if !msg.AmendmentPair(a, o) || s.forged[a.ID] || s.forged[o.ID] {
		return ErrNotAmendment
	}
	for _, c := range s.amendClaims[aid] {
		if c.by == by {
			return nil
		}
	}
	return s.commitLocked(msg.Record{Ack: &msg.Ack{ID: msg.AmendPrefix + aid, By: by, At: s.now().UTC()}})
}
