package store

import (
	"context"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

func TestLeaseLiveWindowKeepsSnapshotCandidatesAndRestoresClockRollback(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	long := projectionLease(0, now, now.Add(time.Hour))
	short := projectionLease(1, now, now.Add(time.Minute))
	expired := projectionLease(2, now.Add(-time.Hour), now.Add(-time.Minute))
	s, _ := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Msg: &long}, {Origin: "peer", Seq: 2, Msg: &short}, {Origin: "peer", Seq: 3, Msg: &expired}}, now)
	st, err := s.history.raw.Stat()
	if err != nil {
		t.Fatal(err)
	}
	end := st.Size()
	page, _, total, err := s.history.liveLeasePage(nil, end, 20)
	if err != nil || len(page) != 2 || total != 2 || page[0].ID != long.ID || page[1].ID != short.ID {
		t.Fatal("live arrival order differs from raw arrival order", err)
	}
	p, err := s.history.pin(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if err := s.history.refreshLiveWindow(now.Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	page, _, total, err = s.history.liveLeasePage(nil, end, 20)
	if err != nil || len(page) != 2 || total != 2 {
		t.Fatal("paused snapshot lost candidates to advancing time", err)
	}
	if err := s.Ack(msg.RetractPrefix+long.ID, long.From); err != nil {
		t.Fatal(err)
	}
	page, _, _, err = s.history.liveLeasePage(nil, end, 20)
	if err != nil || len(page) != 2 {
		t.Fatal("a new retraction removed an older snapshot's candidate", err)
	}
	newer := projectionLease(3, now, now.Add(24*time.Hour))
	if _, err := s.Send(newer); err != nil {
		t.Fatal(err)
	}
	page, _, _, err = s.history.liveLeasePage(nil, end, 20)
	if err != nil || len(page) != 2 {
		t.Fatal("a claim after the raw-end watermark entered the older snapshot", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.history.refreshLiveWindow(now.Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	st, err = s.history.raw.Stat()
	if err != nil {
		t.Fatal(err)
	}
	page, _, total, err = s.history.liveLeasePage(nil, st.Size(), 20)
	if err != nil || len(page) != 1 || total != 1 || page[0].ID != newer.ID {
		t.Fatal("expired candidates were not retired from derived view", err)
	}
	if err := s.history.refreshLiveWindow(now); err != nil {
		t.Fatal(err)
	}
	page, _, total, err = s.history.liveLeasePage(nil, st.Size(), 20)
	if err != nil || len(page) != 3 || total != 3 {
		t.Fatal("clock rollback did not restore all now-live candidates", err)
	}
	if got, ok, err := s.history.messageByID(expired.ID); err != nil || !ok || got.ID != expired.ID {
		t.Fatal("aging removed full ID history", err)
	}
}

func TestLeaseLiveWindowNoOpReadsDoNotWriteOrSyncMetadata(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "active claim", true: "empty view"}[empty], func(t *testing.T) {
			var records []msg.Record
			if !empty {
				m := projectionLease(0, now, now.Add(time.Hour))
				records = []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}}
			}
			s, _ := projectionFixture(t, records, now)
			before := s.history.db.Stats().TxStats
			for i := 1; i <= 100; i++ {
				if err := s.history.refreshLiveWindow(now.Add(time.Duration(i) * time.Nanosecond)); err != nil {
					t.Fatal(err)
				}
			}
			after := s.history.db.Stats().TxStats
			if after.GetWrite() != before.GetWrite() {
				t.Fatalf("100 healthy read preparations performed %d bbolt writes", after.GetWrite()-before.GetWrite())
			}
			var floor time.Time
			if err := s.history.db.View(func(tx *bolt.Tx) error { var err error; floor, err = leaseWindowFloor(tx); return err }); err != nil || !floor.Equal(now) {
				t.Fatal("no-op reader persisted clock changes", err)
			}
		})
	}
}

func TestLeaseLiveWindowVerifiesRawBeforeDroppingAClaim(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	m := projectionLease(0, now, now.Add(time.Minute))
	s, _ := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}}, now)
	// A real read failure cannot be used as evidence that an old candidate may
	// be safely omitted, even while advancing the view's expiry threshold.
	if err := s.history.raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.history.refreshLiveWindow(now.Add(time.Hour)); err == nil {
		t.Fatal("raw failure silently removed a potential hold")
	}
	page, _, count, err := s.history.liveLeasePage(nil, 1<<60, 10)
	if err != nil || len(page) != 1 || count != 1 {
		t.Fatal("failed expiry check mutated the derived candidate set", err)
	}
}
