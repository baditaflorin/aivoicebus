package store

import (
	"slices"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Retraction notices (msg/retract.go): a session that read a message before it was retracted is
// told once, by a message made up when it reads its inbox and never stored. Everything here works
// from the indexes the log keeps (the claims, the receipts, each reader's due list), so a read costs
// what its own reader is owed and not the number of retractions in the log.

// noteReadersLocked notes every identity this node holds a receipt from for message mid, which is
// held and retracted, as possibly owed a notice about it.
func (s *Store) noteReadersLocked(mid string, c *claims) {
	if i, held := s.idx[mid]; !held || s.msgs[i].From != c.from {
		return
	}
	s.noticeIDLocked(mid, c) // reserves the ids its notice has
	for by := range s.acks[mid] {
		s.noteDueLocked(by, mid)
	}
}

// unsortReadersLocked marks the due lists that hold mid as out of order: its time or leak flag changed.
func (s *Store) unsortReadersLocked(mid string) {
	for by := range s.acks[mid] {
		if d := s.due[by]; d != nil && d.ids[mid] {
			d.sorted = false
		}
	}
}

// noteDueLocked adds message mid to reader's due list, unless it is there already.
func (s *Store) noteDueLocked(reader, mid string) {
	if reader == QuarantineBy {
		return
	}
	d := s.due[reader]
	if d == nil {
		d = &dueList{ids: map[string]bool{}, sorted: true}
		s.due[reader] = d
	}
	if d.ids[mid] {
		return
	}
	d.ids[mid] = true
	if n := len(d.order); n > 0 && s.dueCmp(mid, d.order[n-1]) < 0 {
		d.sorted = false
	}
	d.order = append(d.order, mid)
}

// dueCmp orders two retracted messages: leaks first (their notice is urgent, and the per-prompt hook
// shows only urgent ones, so ten older plain notices must not keep one out), then by the time
// notices about them are owed from (claims.due), then by id.
func (s *Store) dueCmp(a, b string) int {
	ra, rb := s.retr[a], s.retr[b]
	var ta, tb time.Time
	var la, lb bool
	if ra != nil {
		ta, la = ra.due, ra.eff.leak
	}
	if rb != nil {
		tb, lb = rb.due, rb.eff.leak
	}
	if la != lb {
		if la {
			return -1
		}
		return 1
	}
	if c := ta.Compare(tb); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// noticesLocked is what reader me is owed now: one notice per retracted message it had read, at most
// msg.MaxNotices, leaks first and then the earliest retractions. Entries that can no longer give a notice are
// dropped as they are met, so each costs one look; the read stops once it has its notices.
func (s *Store) noticesLocked(me string, now time.Time) []msg.Message {
	d := s.due[me]
	if d == nil {
		return nil
	}
	if !d.sorted {
		slices.SortFunc(d.order, s.dueCmp)
		d.sorted = true
	}
	var out []msg.Message
	var keep []string
	looked := 0
	for looked < len(d.order) && len(out) < msg.MaxNotices {
		mid := d.order[looked]
		looked++
		n, ok := s.noticeLocked(me, mid, now)
		if !ok {
			delete(d.ids, mid)
			continue
		}
		out = append(out, n)
		keep = append(keep, mid)
	}
	// the part looked at becomes the entries kept, in their order
	copy(d.order[looked-len(keep):], keep)
	d.order = d.order[looked-len(keep):]
	if len(d.order) == 0 {
		delete(s.due, me)
	} else if cap(d.order) > 2*len(d.order)+16 { // let the dropped front go
		d.order = slices.Clone(d.order)
	}
	return out
}

// noticeLocked is the notice reader me is owed about message mid, or false when it is owed none and
// never will be unless a later record notes it again: the message is not held or not retracted, me
// sent it or retracted it (an authorised claim of its own), the window has passed, the message was
// an fyi that was not flagged as a leak or that me only had counted, or me acked the notice.
func (s *Store) noticeLocked(me, mid string, now time.Time) (msg.Message, bool) {
	i, held := s.idx[mid]
	if !held {
		return msg.Message{}, false
	}
	m := s.msgs[i]
	if m.IsTest() { // a test message is cancelled, never announced: the lane reaches no hook, and nothing here may
		return msg.Message{}, false
	}
	r, retracted := s.retractionLocked(mid, m.From)
	c := s.retr[mid]
	if !retracted || m.From == me || s.forged[mid] || !s.acks[mid][me] || now.Sub(c.due) >= msg.RetractNoticeWindow {
		return msg.Message{}, false
	}
	if _, claimed := c.by[me]; claimed && msg.MayRetract(me, m.From) {
		return msg.Message{}, false
	}
	if m.Priority() == msg.PrioFYI && (!r.leak || s.countOnly[mid][me]) {
		return msg.Message{}, false
	}
	nid := s.noticeIDLocked(mid, c)
	if s.acks[nid][me] {
		return msg.Message{}, false
	}
	exp := c.due.Add(msg.RetractNoticeWindow)
	prio := msg.PrioAsk
	if r.leak {
		prio = msg.PrioUrgent
	}
	return msg.Message{ID: nid, From: msg.NoticeFrom, To: me, Thread: m.Thread, Re: m.ID, At: c.due, Exp: &exp, Prio: prio,
		Body: msg.NoticeBody(r.by, m.ID, m.At, r.leak)}, true
}

// noticeIDLocked is the id of the notice about message mid: the first probe whose id no held message
// has, so a message sent ahead with the id a notice would get does not stand in for it.
func (s *Store) noticeIDLocked(mid string, c *claims) string {
	if c.nid == "" {
		c.probe, c.nid = 0, msg.NoticeID(mid, 0)
		s.noticeIDs[c.nid] = true
	}
	for {
		if _, taken := s.idx[c.nid]; !taken {
			return c.nid
		}
		c.probe++
		c.nid = msg.NoticeID(mid, c.probe)
		s.noticeIDs[c.nid] = true
	}
}

// pruneDueLocked drops, from every reader's list, the messages retracted longer ago than the notice
// window: a reader that never reads again does not keep its list for ever.
func (s *Store) pruneDueLocked(now time.Time) {
	for reader, d := range s.due {
		d.order = slices.DeleteFunc(d.order, func(mid string) bool {
			c := s.retr[mid]
			if c == nil || now.Sub(c.due) >= msg.RetractNoticeWindow {
				delete(d.ids, mid)
				return true
			}
			return false
		})
		if len(d.order) == 0 {
			delete(s.due, reader)
		}
	}
}
