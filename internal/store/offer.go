package store

import (
	"sort"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Offers (see msg/offer.go). A task sent to an audience is delivered to one session at a time: the
// one that holds its offer, or, while nobody does, the best of the live sessions it can reach, which
// then holds it for msg.OfferWindow. Who has been offered a task is what this node delivered it to
// (offered, its own memory) and who acknowledged it (the receipts every node shares, so a session
// shown it through another node counts here too). A claim ends every offer, as it always ended
// delivery to everyone but the owner; a decline ends the decliner's at once.

// maxOffered bounds how many sessions this node offers one task to: past it the task waits for a
// claim by id, or for the person.
const maxOffered = 256

// offerKeep is how long a task's offers are remembered after it was sent; a task lives at most
// msg.MaxDeadline, but one nobody took in two days is no longer being offered round.
const offerKeep = 48 * time.Hour

// offerRec is one session this node delivered an offered task to, and when it first did.
type offerRec struct {
	by string
	at time.Time
}

// declineRef is one decline the log holds (msg.IsDecline), for the task its Re names.
type declineRef struct {
	id, from string
	at       time.Time
	note     string
}

// offerStepsLocked is who has been offered task m and what came of it, earliest first: every session
// this node delivered it to or that acknowledged it, each once at its earliest time, and anyone who
// declined or took it without having been offered it.
func (s *Store) offerStepsLocked(m msg.Message, now time.Time) []msg.OfferStep {
	at := map[string]time.Time{}
	note := func(by string, t time.Time) {
		if by == m.From || by == "" {
			return
		}
		if cur, ok := at[by]; !ok || t.Before(cur) {
			at[by] = t
		}
	}
	for _, o := range s.offered[m.ID] {
		note(o.by, o.at)
	}
	for by := range s.acks[m.ID] {
		t := m.At // an acknowledgement with no time known is as old as the task
		if rc, ok := s.ackAt[m.ID][by]; ok && !rc.At.IsZero() {
			t = rc.At
		}
		note(by, t)
	}
	declined := map[string]declineRef{}
	for _, d := range s.declined[m.ID] {
		if _, dup := declined[d.from]; dup || s.retractedLocked(d.id, d.from) { // a withdrawn decline counts for nothing
			continue
		}
		declined[d.from] = d
		note(d.from, d.at)
	}
	owner, taken := s.taskOwnerLocked(m)
	if taken {
		note(owner.From, owner.At)
	}
	steps := make([]msg.OfferStep, 0, len(at))
	for by, t := range at {
		st := msg.OfferStep{By: by, At: t, Until: t.Add(msg.OfferWindow)}
		d, said := declined[by]
		switch {
		case taken && owner.From == by:
			st.Outcome = msg.OfferClaimed
			st.Until = owner.At
		case said:
			st.Outcome, st.Note = msg.OfferDeclined, d.note
			st.Until = d.at
		case taken:
			st.Outcome = msg.OfferPassed
			if owner.At.Before(st.Until) {
				st.Until = owner.At
			}
		case now.Before(st.Until):
			st.Outcome = msg.OfferOpen
		default:
			st.Outcome = msg.OfferPassed
		}
		if st.Until.Before(st.At) {
			st.Until = st.At
		}
		steps = append(steps, st)
	}
	sort.Slice(steps, func(i, j int) bool {
		if !steps[i].At.Equal(steps[j].At) {
			return steps[i].At.Before(steps[j].At)
		}
		return steps[i].By < steps[j].By
	})
	return steps
}

// offerTiersLocked ranks the live sessions this node knows that task m can reach and that have not
// been offered it yet (msg.OfferTier; those that would not be offered it are left out).
func (s *Store) offerTiersLocked(m msg.Message, steps []msg.OfferStep, now time.Time) map[string]int {
	had := make(map[string]bool, len(steps))
	for _, st := range steps {
		had[st.By] = true
	}
	tiers := map[string]int{}
	for k, v := range s.seen {
		if k == m.From || had[k] || msg.IsHuman(k) || msg.IsProbe(k) || msg.IsSystemSender(k) || !msg.MatchesLabeled(m.To, k, v.labels) {
			continue
		}
		e := msg.RosterEntry{Me: k, Seen: v.at, Title: v.title}
		if t := s.lastSent[k]; t.After(e.Seen) { // a session that sends is about too
			e.Seen = t
		}
		e.Listening, e.Waits = s.listenLocked(k, now)
		if t := msg.OfferTier(e, m, now); t >= 0 {
			tiers[k] = t
		}
	}
	return tiers
}

// offerForLocked says whether untaken task m (msg.Offered) goes to me in this read, and what it is told
// about its offer. A session's own read (read) may start its offer; a look on someone's behalf never does.
func (s *Store) offerForLocked(m msg.Message, me string, read bool, now time.Time) (*msg.Offer, bool) {
	steps := s.offerStepsLocked(m, now)
	mine, other := -1, false
	for i, st := range steps {
		switch {
		case st.By == me:
			mine = i
		case st.Outcome == msg.OfferOpen:
			other = true
		}
	}
	if mine >= 0 { // offered before: it holds the offer still, or it is over for it
		if steps[mine].Outcome != msg.OfferOpen {
			return nil, false
		}
		return s.offerViewLocked(m, me, steps, mine, nil, now), true
	}
	if other || !read || len(s.offered[m.ID]) >= maxOffered {
		return nil, false
	}
	tiers := s.offerTiersLocked(m, steps, now)
	t, ok := tiers[me]
	if !ok {
		return nil, false
	}
	for _, o := range tiers {
		if o < t { // a better one is about: it is offered first
			return nil, false
		}
	}
	s.offered[m.ID] = append(s.offered[m.ID], offerRec{by: me, at: now})
	steps = append(steps, msg.OfferStep{By: me, At: now, Until: now.Add(msg.OfferWindow), Outcome: msg.OfferOpen})
	return s.offerViewLocked(m, me, steps, len(steps)-1, tiers, now), true
}

// offerViewLocked is what me is told of its offer, steps[mine]. tiers, when the caller has them, are
// the live sessions not offered it yet.
func (s *Store) offerViewLocked(m msg.Message, me string, steps []msg.OfferStep, mine int, tiers map[string]int, now time.Time) *msg.Offer {
	if tiers == nil {
		tiers = s.offerTiersLocked(m, steps, now)
	}
	left := len(tiers)
	if _, in := tiers[me]; in {
		left--
	}
	o := &msg.Offer{Since: steps[mine].At, Until: steps[mine].Until, Before: mine, Left: left}
	if _, told := s.escalationLocked(m.ID); !told && now.Sub(m.At) >= msg.EscalateAfter {
		o.Escalate = true
	}
	return o
}

// escalationLocked is the message that told the person nobody took task id, when this node holds it.
func (s *Store) escalationLocked(id string) (msg.Message, bool) {
	i, ok := s.idx[msg.EscalationID(id)]
	if !ok || !msg.IsEscalation(s.msgs[i]) || s.retractedLocked(s.msgs[i].ID, s.msgs[i].From) {
		return msg.Message{}, false
	}
	return s.msgs[i], true
}

// escalationSettledLocked reports whether e, which told the person nobody took a task, has nothing
// left to say: the task was taken, withdrawn or has expired. Its job is done, so it is not delivered.
func (s *Store) escalationSettledLocked(e msg.Message, now time.Time) bool {
	t, ok := s.byIDLocked(e.Re)
	if !ok {
		return false // not here yet: the person is told all the same
	}
	if _, taken := s.taskOwnerLocked(t); taken {
		return true
	}
	return s.retractedLocked(t.ID, t.From) || t.Expired(now)
}

// pruneOffersLocked forgets the offers of tasks that no longer go round: not held, taken, withdrawn,
// expired, or sent more than offerKeep ago.
func (s *Store) pruneOffersLocked(now time.Time) {
	for id := range s.offered {
		m, ok := s.byIDLocked(id)
		if !ok || now.Sub(m.At) > offerKeep || m.Expired(now) || s.retractedLocked(m.ID, m.From) {
			delete(s.offered, id)
			continue
		}
		if _, taken := s.taskOwnerLocked(m); taken {
			delete(s.offered, id)
		}
	}
}
