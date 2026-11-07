package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

func TestLeaseHistoryGenerationPinDoesNotBlockGrowthOrRenewal(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	m := projectionLease(0, now, now.Add(time.Hour))
	s, _ := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}}, now)
	p, err := s.history.pin(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if !s.history.minimumSelectionTime(now.Add(time.Hour)).Equal(now) {
		t.Fatal("paused selection was not pinned")
	}
	done := make(chan error, 1)
	go func() {
		// A long-lived bbolt read transaction would block this mmap growth.
		err := s.history.db.Update(func(tx *bolt.Tx) error {
			b, e := tx.CreateBucket([]byte("fixture-growth"))
			if e != nil {
				return e
			}
			return b.Put([]byte("large"), bytes.Repeat([]byte("x"), 4<<20))
		})
		if err == nil {
			renewal := projectionLease(1, now, now.Add(2*time.Hour))
			_, err = s.Send(renewal)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("paused snapshot blocked mmap growth or renewal")
	}
	if _, err := p.read("peer", 1); err != nil {
		t.Fatal("original generation stopped being readable", err)
	}
	if len(s.Leases()) != 2 {
		t.Fatal("renewal was not visible while snapshot stayed open")
	}
}

func TestLeaseHistoryGenerationPinSurvivesTestLaneOffsetSwap(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	exp := now.Add(time.Minute)
	testID := msg.NewTestID()
	lane := msg.Message{ID: testID, Thread: testID, From: "fixture/a", To: "fixture/b", At: now, Exp: &exp, Body: strings.Repeat("owned fixture ", 40)}
	m := projectionLease(0, now, now.Add(time.Hour))
	s, _ := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Msg: &lane}, {Origin: "peer", Seq: 2, Msg: &m}}, now)
	old := s.history
	p, err := old.pin(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	before, err := p.read("peer", 2)
	if err != nil || before.Msg == nil {
		t.Fatal("control", err)
	}
	done := make(chan struct{})
	go func() { s.SetClock(func() time.Time { return now.Add(6 * time.Minute) }); s.Stats(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rewrite waited for a paused generation pin")
	}
	if s.history == old || s.historyErr != nil {
		t.Fatal("canonical generation was not replaced", s.historyErr)
	}
	select {
	case <-old.life.done:
		t.Fatal("old raw/index handles closed while pinned")
	default:
	}
	after, err := p.read("peer", 2)
	if err != nil || after.Msg == nil || after.Msg.Body != before.Msg.Body {
		t.Fatal("old offset no longer reads the canonical hold", err)
	}
	current, err := s.history.read("peer", 2)
	if err != nil || current.Msg == nil || current.Msg.ID != m.ID {
		t.Fatal("new offsets failed", err)
	}
	oldFD, err := old.raw.Stat()
	if err != nil {
		t.Fatal(err)
	}
	newFD, err := os.Stat(filepath.Join(s.Dir(), "log.jsonl"))
	if err != nil || os.SameFile(oldFD, newFD) {
		t.Fatal("fixture did not actually swap raw generations", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.life.done:
	case <-time.After(time.Second):
		t.Fatal("last pin did not close old generation")
	}
	if err := p.Close(); err != nil {
		t.Fatal("pin close is not idempotent", err)
	}
}

func TestLeaseHistoryGenerationPinsReleaseOnCancelAndStoreClose(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	m := projectionLease(0, now, now.Add(time.Hour))
	s, _ := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}}, now)
	ctx, cancel := context.WithCancel(context.Background())
	p, err := s.history.pin(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.history.life.mu.Lock()
		n := s.history.life.refs
		s.history.life.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := p.read("peer", 1); err == nil {
		t.Fatal("cancelled generation still readable")
	}
	old := s.history
	other, err := old.pin(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.life.done:
	default:
		t.Fatal("Close returned with generation handles still open")
	}
	if _, err := other.read("peer", 1); err == nil {
		t.Fatal("Store.Close did not cancel the open stream")
	}
	reopened, err := Open(s.Dir(), "fixture", WithLeaseRetention(LeaseRetentionOptions{Mode: LeaseRetentionObserve}))
	if err != nil {
		t.Fatal("writer lock was not released", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
