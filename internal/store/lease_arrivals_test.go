package store

import (
	"context"
	"testing"
	"time"
)

func TestManagedArrivalTapeMatchesOriginalPrefixAndClockSemantics(t *testing.T) {
	for _, monotonic := range []bool{false, true} {
		t.Run(map[bool]string{false: "wall clock", true: "monotonic clock"}[monotonic], func(t *testing.T) {
			base := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
			if monotonic {
				base = time.Now()
			}
			s, _ := projectionFixture(t, nil, base)
			var reference []time.Time
			for i := 0; i < 1200; i++ {
				at := base.Add(time.Duration(i) * time.Second)
				if i > 900 {
					at = base.Add(-time.Hour + time.Duration(i)*time.Second)
				} // out-of-order after rollback
				s.noteManagedArrivalLocked(at)
				reference = append(reference, at)
			}
			for _, cutoff := range []time.Time{base.Add(-time.Hour), base.Add(500 * time.Second), base.Add(1000 * time.Second), base.Add(2 * time.Hour)} {
				for len(reference) > 0 && !reference[0].After(cutoff) {
					reference = reference[1:]
				}
				got, err := s.managedLastHourLocked(cutoff)
				if err != nil || got != uint64(len(reference)) {
					t.Fatalf("cutoff%s got%d want%d: %v", cutoff, got, len(reference), err)
				}
			}
		})
	}
}

func TestManagedArrivalCoalescingIsBoundedAndNoOpStatsReadOnly(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	s, _ := projectionFixture(t, nil, now)
	for i := 0; i < 100000; i++ {
		s.noteManagedArrivalLocked(now)
	}
	if s.managedArrivalN != 1 || s.managedArrivalHot[0].Count != 100000 {
		t.Fatal("identical replay timestamps were not coalesced")
	}
	if count, err := s.managedLastHourLocked(now.Add(-time.Hour)); err != nil || count != 100000 {
		t.Fatal("coalesced arrivals lost their count", count, err)
	}
	before := s.history.db.Stats().TxStats
	for i := 0; i < 100; i++ {
		if count, err := s.managedLastHourLocked(now.Add(-time.Hour)); err != nil || count != 100000 {
			t.Fatal(count, err)
		}
	}
	after := s.history.db.Stats().TxStats
	if before.GetWrite() != after.GetWrite() {
		t.Fatal("unchanged Stats caused metadata writes")
	}
	if count, err := s.managedLastHourLocked(now); err != nil || count != 0 {
		t.Fatal("exact one-hour cutoff was not pruned", count, err)
	}
}

func TestManagedArrivalTapeSurvivesIndexGenerationAndKeepsBufferBound(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	s, _ := projectionFixture(t, nil, now)
	for i := 0; i < 1500; i++ {
		s.noteManagedArrivalLocked(now.Add(time.Duration(i) * time.Nanosecond))
		if s.managedArrivalN > 256 {
			t.Fatal("unbounded resident arrival segments")
		}
	}
	p, err := s.history.pin(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// This is the same derived-generation rebuild used by test-lane rewrites;
	// neither disk-spilled timestamps nor the fixed pending buffer may disappear.
	s.mu.Lock()
	s.history.retire(true)
	err = s.rebuildLeaseHistory()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if count, err := s.managedLastHourLocked(now.Add(-time.Hour)); err != nil || count != 1500 {
		t.Fatal("arrival history was lost across the raw/index generation", count, err)
	}
}
