package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestLeaseHistoryHealthChecksAuthorityEvenWithNoLiveCandidates(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	m := projectionLease(0, now.Add(-60*24*time.Hour), now.Add(-45*24*time.Hour))
	s, raw := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}}, now)
	if err := s.HealthChecked(); err != nil {
		t.Fatal(err)
	}
	if page, _, _, err := s.history.liveLeasePage(nil, s.history.rawEnd, 10); err != nil || len(page) != 0 {
		t.Fatal("fixture should have no active candidates", err)
	}
	path := filepath.Join(s.Dir(), "log.jsonl")
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.HealthChecked(); err == nil {
		t.Fatal("missing cold history became a healthy empty candidate set")
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.HealthChecked(); err == nil {
		t.Fatal("uncertain state cleared without a verified reopen")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.Dir(), "fixture", WithLeaseRetention(LeaseRetentionOptions{Mode: LeaseRetentionObserve}))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.HealthChecked(); err != nil {
		t.Fatal("verified complete raw reopen failed", err)
	}
}

func TestLeaseHistoryHealthFollowsAcknowledgedAppends(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	s, _ := projectionFixture(t, nil, now)
	m := projectionLease(0, now, now.Add(time.Hour))
	if _, err := s.Send(m); err != nil {
		t.Fatal(err)
	}
	if err := s.HealthChecked(); err != nil {
		t.Fatal("a canonical acknowledged append looked like external change", err)
	}
	if err := s.history.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.HealthChecked(); err == nil {
		t.Fatal("a closed derived index was reported healthy")
	}
}
