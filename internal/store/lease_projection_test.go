package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

func projectionLease(i int, at, until time.Time) msg.Message {
	id := fmt.Sprintf("%010x", i+1)
	identity := fmt.Sprintf("0docker-builder/fleet-runner.%032x", i)
	return msg.Message{ID: id, Thread: id, From: identity, To: identity, Kind: msg.KindLease,
		Body: fmt.Sprintf("deploy:prod:fixture-%d\nnote", i), At: at, Exp: &until}
}

func projectionFixture(t *testing.T, records []msg.Record, now time.Time) (*Store, []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, "fixture", WithClock(func() time.Time { return now }), WithLeaseRetention(LeaseRetentionOptions{
		Mode: LeaseRetentionObserve, Hosts: []string{"0docker-builder"}, MaxHotRecords: 2, MaxHotBytes: 64, CacheBytes: 64,
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, raw.Bytes()
}

func TestInternalLeaseEligibilityIsNarrow(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	base := projectionLease(0, now, now.Add(time.Hour))
	options := LeaseRetentionOptions{Hosts: []string{"0docker-builder"}}
	if !internalLeaseEligible(base, options) {
		t.Fatal("canonical nonce lease must qualify")
	}
	legacy := base
	legacy.From = "0docker-builder/fleet-runner"
	legacy.To = legacy.From
	if !internalLeaseEligible(legacy, options) {
		t.Fatal("legacy owner history must qualify")
	}
	cases := map[string]func(*msg.Message){
		"unapproved host": func(m *msg.Message) { m.From = "other/fleet-runner"; m.To = m.From },
		"prefix host":     func(m *msg.Message) { m.From = "0docker-builder-extra/fleet-runner"; m.To = m.From },
		"human":           func(m *msg.Message) { m.From = "0docker-builder/operator"; m.To = m.From },
		"short nonce":     func(m *msg.Message) { m.From = "0docker-builder/fleet-runner.abcdef"; m.To = m.From },
		"nonhex nonce": func(m *msg.Message) {
			m.From = "0docker-builder/fleet-runner.zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
			m.To = m.From
		},
		"ordinary message":     func(m *msg.Message) { m.Kind = "" },
		"other recipient":      func(m *msg.Message) { m.To = "person/agent" },
		"reply":                func(m *msg.Message) { m.Re = "1234567890" },
		"other thread":         func(m *msg.Message) { m.Thread = "1234567890" },
		"staging":              func(m *msg.Message) { m.Body = "deploy:staging:fixture" },
		"lookalike namespace":  func(m *msg.Message) { m.Body = "deploy:production:fixture" },
		"global operator hold": func(m *msg.Message) { m.Body = "deploy:prod" },
		"empty service":        func(m *msg.Message) { m.Body = "deploy:prod:" },
		"invalid resource":     func(m *msg.Message) { m.Body = "deploy:prod:with spaces" },
		"no expiration":        func(m *msg.Message) { m.Exp = nil },
		"invalid lifetime":     func(m *msg.Message) { m.Exp = &m.At },
		"unconverted ttl":      func(m *msg.Message) { m.TTL = 60 },
		"thread limit":         func(m *msg.Message) { m.MaxTurns = 2 },
		"thread deadline":      func(m *msg.Message) { m.Deadline = 60 },
		"thread budget":        func(m *msg.Message) { m.BudgetChars = 1024 },
		"task depth":           func(m *msg.Message) { m.Depth = 1 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m := base
			change(&m)
			if internalLeaseEligible(m, options) {
				t.Fatal("out-of-scope message was selected")
			}
		})
	}
	if internalLeaseEligible(base, LeaseRetentionOptions{}) {
		t.Fatal("empty allowlist selected history")
	}
}

func TestLeaseProjectionPagesDoNotCapLogicalPopulation(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	const count = 5003
	records := make([]msg.Record, 0, count)
	for i := 0; i < count; i++ {
		m := projectionLease(i, now.Add(-time.Hour), now.Add(time.Duration(i+1)*time.Second))
		records = append(records, msg.Record{Origin: "peer", Seq: uint64(i + 1), Msg: &m})
	}
	s, raw := projectionFixture(t, records, now)
	var cursor []byte
	total := 0
	for {
		page, next, err := s.history.internalLeasePage(now, cursor, 127)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > 127 {
			t.Fatal("resident page exceeded its bound")
		}
		for _, ref := range page {
			m, err := s.history.readInternalLease(ref)
			if err != nil {
				t.Fatal(err)
			}
			if m.ID != records[total].Msg.ID {
				t.Fatalf("lost or repeated record at %d: %s", total, m.ID)
			}
			total++
		}
		if next == nil {
			break
		}
		cursor = next
	}
	if total != count {
		t.Fatalf("selected %d of %d valid active claims", total, count)
	}
	got, err := os.ReadFile(filepath.Join(s.Dir(), "log.jsonl"))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("projection changed raw history", err)
	}
	if len(s.Leases()) != count {
		t.Fatal("observe altered live lease visibility")
	}
	// A backwards clock makes previously expired claims visible again. Their
	// references were not discarded by an earlier query or sweep.
	page, _, err := s.history.internalLeasePage(now.Add(2*time.Hour), nil, 20)
	if err != nil || len(page) != 0 {
		t.Fatal("expected no future active claims", err)
	}
	page, _, err = s.history.internalLeasePage(now, nil, 20)
	if err != nil || len(page) != 20 {
		t.Fatal("clock rollback lost retained claims", err)
	}
}

func TestLeaseProjectionPreservesFirstIDAndFutureTargetRelations(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	lease := projectionLease(0, now, now.Add(time.Hour))
	ordinary := lease
	ordinary.Kind = ""
	ordinary.Body = "ordinary audit history"
	ack := msg.Ack{ID: msg.RetractPrefix + lease.ID, By: lease.From, At: now}
	s, _ := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Ack: &ack}, {Origin: "peer", Seq: 2, Msg: &lease}, {Origin: "other", Seq: 1, Msg: &ordinary}}, now)
	got, ok, err := s.history.messageByID(lease.ID)
	if err != nil || !ok || got.Body != lease.Body {
		t.Fatal("first-arrival ID changed", err)
	}
	if _, ok, err := s.history.internalLease(lease.ID); err != nil || !ok {
		t.Fatal("first internal message not indexed", err)
	}
	err = s.history.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(historyAckTargets).Cursor()
		prefix := []byte(lease.ID + "\x00")
		key, value := cursor.Seek(prefix)
		if !bytes.HasPrefix(key, prefix) || !bytes.Equal(value, historyKey("peer", 1)) {
			t.Fatal("ack before message lost its on-disk target relation")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Msg: &ordinary}, {Origin: "other", Seq: 1, Msg: &lease}}, now)
	if _, ok, err := s2.history.internalLease(lease.ID); err != nil || ok {
		t.Fatal("later colliding internal message displaced ordinary history", err)
	}
}

func TestLeaseProjectionExpiryBeyondUnixNanoAndColdReadErrors(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	m := projectionLease(0, now, time.Date(4000, 1, 1, 0, 0, 0, 0, time.UTC))
	s, _ := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}}, now)
	page, _, err := s.history.internalLeasePage(time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), nil, 10)
	if err != nil || len(page) != 1 {
		t.Fatal("far-future active hold was lost to integer time overflow", err)
	}
	if err := os.Truncate(filepath.Join(s.Dir(), "log.jsonl"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.readInternalLease(page[0]); err == nil {
		t.Fatal("cold I/O failure was accepted")
	}
	if _, _, err := s.history.messageByID(m.ID); err == nil {
		t.Fatal("ID read hid cold I/O failure")
	}
}

func TestLeaseProjectionIndexErrorsNeverReturnPartialPages(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	first := projectionLease(0, now, now.Add(time.Hour))
	second := projectionLease(1, now, now.Add(2*time.Hour))
	s, _ := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Msg: &first}, {Origin: "peer", Seq: 2, Msg: &second}}, now)
	page, _, err := s.history.internalLeasePage(now, nil, 10)
	if err != nil || len(page) != 2 {
		t.Fatal("healthy control", err)
	}
	wrongOrder := page[0]
	wrongOrder.Offset++
	if _, err := s.history.readInternalLease(wrongOrder); err == nil {
		t.Fatal("altered ordering was accepted")
	}
	key, err := leaseExpiryKey(*second.Exp, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.history.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(historyLeaseExpiry).Put(key, []byte("missing")) }); err != nil {
		t.Fatal(err)
	}
	page, next, err := s.history.internalLeasePage(now, nil, 10)
	if err == nil || page != nil || next != nil {
		t.Fatal("partial page escaped an index integrity failure", err)
	}
	if err := s.history.db.Close(); err != nil {
		t.Fatal(err)
	}
	if page, next, err := s.history.internalLeasePage(now, nil, 10); err == nil || page != nil || next != nil {
		t.Fatal("closed index became an empty success", err)
	}
}
