package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// The test lane (msg/testlane.go): what the store does with a test message beyond hiding it from
// reads and stats (done where those are computed). It keeps the message in the log like any other,
// because the log is replicated by (origin, seq) and a record cannot be taken out; once the
// message has expired it replaces the body by a stub (msg.TestStub) in memory, in what peers are
// served, and in log.jsonl.

const (
	// laneBucket is added to the keys of the rate limits and brakes for a test message, so that
	// test traffic is counted apart from the real traffic of the same sender, machine and pair.
	laneBucket = "\x00test"
	// sweepEvery bounds how often expired test messages are looked for (on a read or a send; there
	// is no timer). rewriteEvery bounds how often log.jsonl is rewritten to drop the bodies.
	sweepEvery   = time.Minute
	rewriteEvery = 5 * time.Minute
)

// hiddenTestLocked reports whether m is left out of a read: a purged test message always, any
// other test message unless the read asked for the lane and its reader declared the opt-in.
func (s *Store) hiddenTestLocked(m msg.Message, lane bool, labels msg.Labels) bool {
	if !m.IsTest() {
		return false
	}
	return msg.TestPurged(m) || !lane || !msg.OptedIn(labels)
}

// sweepLocked replaces every test message past its expiry by its stub, and rewrites log.jsonl so the
// bodies are gone from disk too. It does nothing when there is nothing to do, and at most once a
// minute (and the rewrite once in five) when there is.
func (s *Store) sweepLocked(now time.Time) {
	if len(s.tests) == 0 && len(s.purgedRefs) == 0 {
		return
	}
	if now.Sub(s.lastSweep) >= sweepEvery {
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
				s.purgedRefs[ref] = true
			}
		}
		s.tests = keep
	}
	if len(s.purgedRefs) > 0 && now.Sub(s.lastRewrite) >= rewriteEvery {
		s.lastRewrite = now
		if err := s.rewriteLogLocked(); err != nil {
			s.logf("could not drop the bodies of purged test messages from the log (will try again): %v", err)
		}
	}
}

// renameLog puts the rewritten log in place; a variable so a test can make the power fail between the
// temporary file and the rename.
var renameLog = os.Rename

// rewriteLogLocked writes log.jsonl again with the records in purgedRefs as their stubs, to a
// temporary file that is synced and renamed over the log, so a crash leaves the old log or the new
// one and never half of either. A line too long to parse (see readLines) is written as the
// placeholder the node holds for it in memory. Every other line is copied byte for byte.
func (s *Store) rewriteLogLocked() error {
	path := filepath.Join(s.dir, "log.jsonl")
	tmp := path + ".purge"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(out)
	var werr error
	write := func(b []byte) {
		if werr == nil {
			_, werr = bw.Write(b)
		}
	}
	rerr := readLines(path, func(line []byte, over bool) {
		r, ok := headRecord(line)
		switch {
		case over && ok:
			b, _ := json.Marshal(quarantine(r.Origin, r.Seq))
			write(append(b, '\n'))
			return
		case over:
			return // not a record: replay skips it too
		}
		if !ok || !s.purgedRefs[testRef{r.Origin, r.Seq}] {
			write(line)
			return
		}
		cur := s.log[r.Origin]
		if r.Seq == 0 || r.Seq > uint64(len(cur)) {
			write(line)
			return
		}
		b, _ := json.Marshal(cur[r.Seq-1])
		write(append(b, '\n'))
	})
	if werr == nil {
		werr = rerr
	}
	if werr == nil {
		werr = bw.Flush()
	}
	if werr == nil {
		werr = out.Sync()
	}
	if cerr := out.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = renameLog(tmp, path)
	}
	if werr != nil {
		os.Remove(tmp)
		return werr
	}
	s.purgedRefs = map[testRef]bool{}
	return nil
}
