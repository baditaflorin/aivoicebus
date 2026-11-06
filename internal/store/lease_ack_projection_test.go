package store

import (
	"context"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

func TestLeaseAckProjectionKeepsSnapshotsAndCanonicalAuthority(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	m := projectionLease(0, now, now.Add(time.Hour))
	unauthorized := msg.Ack{ID: msg.RetractPrefix + m.ID, By: "other/fleet-runner", At: now.Add(-time.Hour)}
	s, _ := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}, {Origin: "peer", Seq: 2, Ack: &unauthorized}}, now)
	p, err := s.history.pin(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	oldVV := s.VV()
	if _, ok, err := s.history.foldedRetraction(m.ID, m.From, oldVV, p.read); err != nil || ok {
		t.Fatal("unauthorized claim removed a hold", err)
	}
	if err := s.Ack(msg.RetractPrefix+m.ID, m.From); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.history.foldedRetraction(m.ID, m.From, oldVV, p.read); err != nil || ok {
		t.Fatal("new withdrawal entered an older snapshot", err)
	}
	got, ok, err := s.history.foldedRetraction(m.ID, m.From, s.VV(), p.read)
	if err != nil || !ok || got.by != m.From || !got.at.Equal(now) {
		t.Fatal("new snapshot did not see the authorized withdrawal", err)
	}
	if err := s.Ack(msg.RetractLeakPrefix+m.ID, m.From); err != nil {
		t.Fatal(err)
	}
	got, ok, err = s.history.foldedRetraction(m.ID, m.From, s.VV(), p.read)
	if err != nil || !ok || !got.leak {
		t.Fatal("stronger leak flag was not retained", err)
	}
	st, err := s.history.raw.Stat()
	if err != nil {
		t.Fatal(err)
	}
	page, _, _, err := s.history.liveLeasePage(nil, st.Size(), 10)
	if err != nil || len(page) != 1 {
		t.Fatal("withdrawals erased candidate needed by an older snapshot", err)
	}
}

func TestLeaseAckProjectionSupportsClaimsBeforeMessagesAndFailsOnBadReferences(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	m := projectionLease(0, now, now.Add(time.Hour))
	claim := msg.Ack{ID: msg.RetractPrefix + m.ID, By: m.From, At: now}
	s, _ := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Ack: &claim}, {Origin: "peer", Seq: 2, Msg: &m}}, now)
	if _, ok, err := s.history.foldedRetraction(m.ID, m.From, s.VV(), s.history.read); err != nil || !ok {
		t.Fatal("claim preceding its message was lost", err)
	}
	// Pointing a relation at a perfectly readable message is still corruption,
	// not proof that a lease is free or withdrawn.
	if err := s.history.db.Update(func(tx *bolt.Tx) error {
		key := historyKey("peer", 2)
		return tx.Bucket(historyAckTargets).Put(append([]byte(m.ID+"\x00"), key...), key)
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.history.foldedRetraction(m.ID, m.From, s.VV(), s.history.read); err == nil || ok {
		t.Fatal("bad relation became a successful retraction result", err)
	}
}
