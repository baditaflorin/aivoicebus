package store

import (
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestLeaseAckMetadataPreservesUnknownAndDuplicateReceiptCounts(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	unknown := "1234567890"
	m := projectionLease(0, now, now.Add(time.Hour))
	read := msg.Ack{ID: unknown, By: "ordinary/reader", At: now, Via: msg.ViaHookCount}
	full := read
	full.At = now.Add(time.Minute)
	full.Via = msg.ViaInbox
	claim := msg.Ack{ID: msg.RetractPrefix + m.ID, By: m.From, At: now}
	unauth := claim
	unauth.By = "other/reader"
	leak := claim
	leak.ID = msg.RetractLeakPrefix + m.ID
	leak.At = now.Add(time.Minute)
	ordinary := msg.Message{ID: unknown, Thread: unknown, From: "ordinary/sender", To: "ordinary/reader", Body: "important ordinary history", At: now}
	records := []msg.Record{
		{Origin: "peer", Seq: 1, Ack: &read}, {Origin: "duplicate", Seq: 1, Ack: &read},
		{Origin: "peer", Seq: 2, Ack: &full}, {Origin: "peer", Seq: 3, Ack: &claim},
		{Origin: "peer", Seq: 4, Ack: &unauth}, {Origin: "peer", Seq: 5, Msg: &m},
		{Origin: "peer", Seq: 6, Ack: &claim}, {Origin: "peer", Seq: 7, Ack: &leak},
		{Origin: "peer", Seq: 8, Msg: &ordinary},
	}
	s, _ := projectionFixture(t, records, now)
	counters, err := s.history.counters()
	if err != nil {
		t.Fatal(err)
	}
	if counters.receipts != 1 || counters.messages != 1 || counters.retracted != 1 || counters.inert != 1 {
		t.Fatalf("derived counts lost unknown or repeated data: %+v", counters)
	}
	stats := s.Stats()
	if counters.receipts != uint64(stats.Acks) || counters.retracted != uint64(stats.Retracted) || counters.inert != uint64(stats.RetractionsIneffective) {
		t.Fatalf("derived counts differ from original public counts: %+v %+v", counters, stats)
	}
	for _, id := range []string{unknown, m.ID} {
		if pending, err := s.history.deferredTarget(id); err != nil || !pending {
			t.Fatal("ACK-before-target relation not retained", id, err)
		}
	}
	if err := s.history.clearDeferredTarget(unknown); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.history.deferredTarget(unknown); err != nil || pending {
		t.Fatal("derived pending marker not cleared", err)
	}
}

func TestLeaseAckMetadataUnknownForeverAndLateOrdinaryAreNotManaged(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	id := "1234567890"
	read := msg.Ack{ID: id, By: "reader/session", At: now.Add(time.Hour), Via: msg.ViaInbox}
	retract := msg.Ack{ID: msg.RetractPrefix + id, By: "sender/session", At: now.Add(2 * time.Hour)}
	for _, withMessage := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown forever", true: "ordinary arrives later"}[withMessage], func(t *testing.T) {
			records := []msg.Record{{Origin: "peer", Seq: 1, Ack: &read}, {Origin: "peer", Seq: 2, Ack: &retract}}
			if withMessage {
				m := msg.Message{ID: id, Thread: id, From: "sender/session", To: "reader/session", At: now, Body: "ordinary bytes"}
				records = append(records, msg.Record{Origin: "peer", Seq: 3, Msg: &m})
			}
			s, _ := projectionFixture(t, records, now)
			counts, err := s.history.counters()
			if err != nil {
				t.Fatal(err)
			}
			if counts.receipts != 1 || counts.messages != 0 || counts.retracted != 0 || counts.inert != 0 {
				t.Fatalf("ordinary or unknown data was claimed as scoped history: %+v", counts)
			}
			if got := s.Stats(); got.Acks != 1 || (withMessage && got.Retracted != 1) || (!withMessage && got.Retracted != 0) {
				t.Fatalf("reference semantics changed before offload: %+v", got)
			}
		})
	}
}
