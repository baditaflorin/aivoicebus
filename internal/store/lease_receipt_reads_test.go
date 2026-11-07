package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

func TestLeaseReceiptCountsAndExactReadsVerifyBoundedRawProofs(t *testing.T) {
	now := onViewsNow
	m := projectionLease(0, now, now.Add(time.Hour))
	count := msg.Ack{ID: m.ID, By: "reader/a", At: now, Via: msg.ViaHookCount}
	full := count
	full.At = now.Add(time.Minute)
	full.Via = msg.ViaInbox
	records := []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}, {Origin: "peer", Seq: 2, Ack: &count}, {Origin: "other", Seq: 1, Ack: &full}}
	s := onStateStore(t, LeaseRetentionOn, records, &now)
	if n := s.Readers(m.ID); n != 1 {
		t.Fatal("receipt count", n)
	}
	state, found, err := s.history.exactReceipt(m.ID, count.By)
	if err != nil || !found || state.CountOnly || state.Receipt.Via != msg.ViaInbox {
		t.Fatal(state, found, err)
	}
	before := s.history.db.Stats().TxStats
	for i := 0; i < 100; i++ {
		if n := s.Readers(m.ID); n != 1 {
			t.Fatal(n)
		}
	}
	if after := s.history.db.Stats().TxStats; after.GetWrite() != before.GetWrite() {
		t.Fatal("count-only public reads wrote metadata")
	}
	if err := s.history.db.Update(func(tx *bolt.Tx) error {
		key := []byte(m.ID + "\x00" + count.By)
		var v receiptSummary
		if err := json.Unmarshal(tx.Bucket(historyReceipts).Get(key), &v); err != nil {
			return err
		}
		v.FullProof = historyKey("peer", 1)
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return tx.Bucket(historyReceipts).Put(key, b)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.history.exactReceipt(m.ID, count.By); err == nil {
		t.Fatal("non-receipt raw record accepted as full-reader proof")
	}
	_ = s.Readers(m.ID)
	if s.HealthChecked() == nil {
		t.Fatal("corrupted receipt projection appeared healthy")
	}
}

func TestLeaseRetentionOnColdSettleRepeatsAreIdempotent(t *testing.T) {
	now := onViewsNow
	m := projectionLease(0, now.Add(-48*time.Hour), now.Add(-47*time.Hour))
	var expected map[string]time.Time
	for _, mode := range []LeaseRetentionMode{LeaseRetentionOff, LeaseRetentionObserve, LeaseRetentionOn} {
		s := onStateStore(t, mode, []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}}, &now)
		if err := s.Settle(m.ID, "0docker-builder/human"); err != nil {
			t.Fatal(err)
		}
		vv := s.VV()
		if err := s.Settle(m.ID, "0docker-builder/human"); err != nil || !reflect.DeepEqual(vv, s.VV()) {
			t.Fatal("repeated cold settle appended new history", mode, err)
		}
		got := s.Settled(m.ID)
		if mode == LeaseRetentionOff {
			expected = got
		} else if !reflect.DeepEqual(got, expected) {
			t.Fatal("cold settle projection differs", mode, got, expected)
		}
	}
}

func TestLeaseIteratorCancelledWindowPreparationLeavesStoreHealthy(t *testing.T) {
	now := onViewsNow
	m := projectionLease(0, now, now.Add(time.Hour))
	s := onStateStore(t, LeaseRetentionOn, []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}}, &now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now = now.Add(2 * time.Hour)
	if err := s.history.refreshLiveWindowContext(ctx, now); err == nil {
		t.Fatal("cancelled preparation ignored deadline")
	}
	if _, err := s.NewLeaseIterator(ctx); err == nil {
		t.Fatal("cancelled public preparation succeeded")
	}
	if err := s.HealthChecked(); err != nil {
		t.Fatal("request cancellation made sound authority unhealthy", err)
	}
}

func TestLeaseRetentionOnNoticeClockRollbackDoesNotReclampArrival(t *testing.T) {
	var expected []msg.Message
	for _, mode := range []LeaseRetentionMode{LeaseRetentionOff, LeaseRetentionObserve, LeaseRetentionOn} {
		s, ms, clock := onViewsFixture(t, mode)
		root := ms[0]
		if _, err := s.Retract(root.ID, root.From, true); err != nil {
			t.Fatal(err)
		}
		*clock = onViewsNow.Add(-time.Hour)
		got := s.Inbox("person/reader", false)
		if len(got) != 1 || !got[0].At.Equal(onViewsNow) {
			t.Fatal("clock rollback changed an already known notice arrival", mode, got)
		}
		if mode == LeaseRetentionOff {
			expected = got
		} else if !reflect.DeepEqual(got, expected) {
			t.Fatal("notice rollback parity", mode, got, expected)
		}
	}
}

func TestLeaseIteratorExpiryIntegrityFailureLatchesBeforePinClose(t *testing.T) {
	now := onViewsNow
	m := projectionLease(0, now, now.Add(time.Hour))
	s := onStateStore(t, LeaseRetentionOn, []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}}, &now)
	raw, err := os.ReadFile(filepath.Join(s.Dir(), "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	offset := bytes.Index(raw, []byte("deploy:prod"))
	if offset < 0 {
		t.Fatal("fixture payload missing")
	}
	if _, err := s.history.raw.WriteAt([]byte("X"), int64(offset)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if it, err := s.NewLeaseIterator(context.Background()); err == nil {
		_ = it.Close()
		t.Fatal("corrupt expiry transition succeeded")
	}
	if err := s.HealthChecked(); err == nil {
		t.Fatal("closing the failed request pin hid a real integrity error")
	}
}
