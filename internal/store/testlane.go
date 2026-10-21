package store

import (
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// The test lane (msg/testlane.go): what the store does with a test message beyond hiding it from
// reads and stats (done where those are computed). It keeps the message in the log like any other,
// because the log is replicated by (origin, seq) and a record cannot be taken out; once the
// message has expired it replaces the body by a stub (msg.TestStub) in memory, and so in what peers
// and exports are served. The file keeps the body until the log is rewritten (a later change): a
// restart replays it and the sweep at Open replaces it by its stub again.

const (
	// laneBucket is added to the keys of the rate limits and brakes for a test message, so that
	// test traffic is counted apart from the real traffic of the same sender, machine and pair.
	laneBucket = "\x00test"
	// sweepEvery bounds how often expired test messages are looked for (on a read or a send; there
	// is no timer).
	sweepEvery = time.Minute
)

// hiddenTestLocked reports whether m is left out of a read: a purged test message always, any
// other test message unless the read asked for the lane and its reader declared the opt-in.
func (s *Store) hiddenTestLocked(m msg.Message, lane bool, labels msg.Labels) bool {
	if !m.IsTest() {
		return false
	}
	return msg.TestPurged(m) || !lane || !msg.OptedIn(labels)
}

// sweepLocked replaces every test message past its expiry by its stub. It does nothing when there is
// nothing to do, and at most once a minute when there is.
func (s *Store) sweepLocked(now time.Time) {
	if len(s.tests) == 0 || now.Sub(s.lastSweep) < sweepEvery {
		return
	}
	s.lastSweep = now
	keep := s.tests[:0]
	for _, ref := range s.tests {
		recs := s.log[ref.origin]
		if ref.seq == 0 || ref.seq > uint64(len(recs)) || recs[ref.seq-1].Msg == nil {
			continue
		}
		r := &recs[ref.seq-1]
		i, held := s.idx[r.Msg.ID]
		if !held {
			continue
		}
		if m := s.msgs[i]; !msg.TestPurged(m) {
			if m.Exp == nil || !now.After(*m.Exp) {
				keep = append(keep, ref)
				continue
			}
			s.msgs[i] = msg.TestStub(m)
		}
		if !msg.TestPurged(*r.Msg) {
			stub := msg.TestStub(*r.Msg)
			r.Msg = &stub
		}
	}
	s.tests = keep
}
