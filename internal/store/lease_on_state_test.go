package store

import (
	"encoding/json"
	"fmt"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func onStateStore(t *testing.T, mode LeaseRetentionMode, records []msg.Record, clock *time.Time) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(f)
	for _, r := range records {
		if err := encoder.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, "on-state", WithClock(func() time.Time { return *clock }), WithLeaseRetention(LeaseRetentionOptions{Mode: mode, Hosts: []string{"0docker-builder"}, MaxHotRecords: 1, MaxHotBytes: 1, CacheBytes: 1}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestLeaseRetentionOnPermanentMapsStayEmptyBeyondHotBudget(t *testing.T) {
	const count = 5003
	now := onViewsNow
	var records []msg.Record
	for i := 0; i < count; i++ {
		m := projectionLease(i, now, now.Add(time.Hour))
		records = append(records, msg.Record{Origin: "synthetic", Seq: uint64(len(records) + 1), Msg: &m})
		a := msg.Ack{ID: m.ID, By: fmt.Sprintf("reader/session-%d", i), At: now}
		records = append(records, msg.Record{Origin: "synthetic", Seq: uint64(len(records) + 1), Ack: &a})
	}
	s := onStateStore(t, LeaseRetentionOn, records, &now)
	if len(s.msgs) != 0 || len(s.idx) != 0 || len(s.leases) != 0 || len(s.threads) != 0 || len(s.byTo) != 0 || len(s.lastSent) != 0 || len(s.acks) != 0 || len(s.ackAt) != 0 || len(s.log) != 0 || len(s.arrivals) != 0 || s.managedArrivalN > 256 {
		t.Fatal("managed history entered permanent payload/identity/index maps")
	}
	got, err := s.LeasesChecked()
	if err != nil || len(got) != count {
		t.Fatal("physical budget limited logical claims", len(got), err)
	}
	status := s.LeaseRetentionStats()
	if status.HotRecords != 0 || status.HotBytes != 0 || status.CacheBytes != 0 || status.ColdRecords != count {
		t.Fatal("managed physical retention metrics incorrect", status)
	}
	before := s.history.db.Stats().TxStats
	for i := 0; i < 10; i++ {
		ls, err := s.LeasesChecked()
		if err != nil || len(ls) != count {
			t.Fatal(err)
		}
	}
	if after := s.history.db.Stats().TxStats; after.GetWrite() != before.GetWrite() {
		t.Fatal("completed healthy lease GETs wrote derived metadata")
	}
}

func TestLeaseRetentionOnMixedArrivalClockPrefixMatchesOff(t *testing.T) {
	var expected []int
	for _, mode := range []LeaseRetentionMode{LeaseRetentionOff, LeaseRetentionObserve, LeaseRetentionOn} {
		now := onViewsNow
		s := onStateStore(t, mode, nil, &now)
		var counts []int
		for i, when := range []time.Time{onViewsNow.Add(-2 * time.Hour), onViewsNow, onViewsNow.Add(-2 * time.Hour)} {
			now = when
			m := projectionLease(i, now, now.Add(time.Hour))
			if i != 1 {
				m.Kind = ""
				m.From = "ordinary/sender"
				m.To = "ordinary/reader"
				m.Body = "ordinary synthetic arrival"
			}
			if _, err := s.Merge([]msg.Record{{Origin: "peer", Seq: uint64(i + 1), Msg: &m}}); err != nil {
				t.Fatal(err)
			}
		}
		for _, when := range []time.Time{onViewsNow, onViewsNow.Add(-3 * time.Hour), onViewsNow.Add(2 * time.Hour)} {
			now = when
			counts = append(counts, s.Stats().LastHour)
		}
		if mode == LeaseRetentionOff {
			expected = counts
		} else if !reflect.DeepEqual(counts, expected) {
			t.Fatal("splitting managed/ordinary tape changed prefix semantics", mode, counts, expected)
		}
	}
}

func TestLeaseRetentionOnDeferredCrossOriginReceiptsKeepRawArrivalOrder(t *testing.T) {
	now := onViewsNow
	id := "000000bb01"
	m := msg.Message{ID: id, Thread: id, From: "sender/a", To: "reader/b", At: now, Body: "ordinary late target"}
	// Origin lexical order differs from raw arrival order; Via upgrade is not a
	// commutative fold, so replay must use the latter, exactly like legacy addAck.
	first := msg.Ack{ID: id, By: "reader/b", At: now.Add(time.Hour), Via: msg.ViaInbox}
	second := first
	second.At = now
	second.Via = msg.ViaHookCount
	records := []msg.Record{{Origin: "z-first", Seq: 1, Ack: &first}, {Origin: "a-second", Seq: 1, Ack: &second}, {Origin: "target", Seq: 1, Msg: &m}}
	var golden msg.Receipts
	for _, mode := range []LeaseRetentionMode{LeaseRetentionOff, LeaseRetentionObserve, LeaseRetentionOn} {
		s := onStateStore(t, mode, records, &now)
		got, ok := s.Receipts(id)
		if !ok {
			t.Fatal("target missing")
		}
		if mode == LeaseRetentionOff {
			golden = got
		} else if !reflect.DeepEqual(got, golden) {
			t.Fatal("ACK projection reordered arrival semantics", mode, got, golden)
		}
	}
}
