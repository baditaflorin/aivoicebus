package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestLeaseHistoryHasOnlyOneWriterAndCloseReleasesIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := Open(dir, "n1", WithLeaseRetention(LeaseRetentionOptions{Mode: LeaseRetentionObserve}))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.Send(msg.Message{From: "builder/agent", To: "reader/agent", Body: "ordinary history"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(dir, "n1", WithLeaseRetention(LeaseRetentionOptions{Mode: LeaseRetentionObserve})); err == nil || other != nil {
		t.Fatal("second indexed writer was admitted")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if !bytes.Equal(before, after) {
		t.Fatal("refused writer changed raw history")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir, "n1", WithLeaseRetention(LeaseRetentionOptions{Mode: LeaseRetentionObserve}))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.VV()["n1"] != 1 {
		t.Fatal("reopen lost the committed sequence")
	}
}

func TestLeaseHistoryPostAppendIndexFailureNeedsRecovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, "n", WithLeaseRetention(LeaseRetentionOptions{Mode: LeaseRetentionObserve}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.history.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(msg.Message{From: "writer/agent", To: "reader/agent", Body: "durable uncertain append"}); err == nil {
		t.Fatal("closed index acknowledged a write")
	}
	path := filepath.Join(dir, "log.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(before, []byte{'\n'}) != 1 {
		t.Fatal("expected one raw record awaiting recovery")
	}
	if _, err := s.Send(msg.Message{From: "writer/agent", To: "reader/agent", Body: "must not reuse the sequence"}); err == nil {
		t.Fatal("uncertain store kept accepting writes")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("uncertainty appended a duplicate sequence")
	}
	if records, err := s.SinceBytesChecked(nil, 100, 0); err == nil || records != nil {
		t.Fatal("uncertain history returned a successful feed")
	}
	_ = s.Close() // DB is deliberately already closed; writer lock must still release.
	reopened, err := Open(dir, "n", WithLeaseRetention(LeaseRetentionOptions{Mode: LeaseRetentionObserve}))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.VV()["n"] != 1 || len(reopened.Messages()) != 1 {
		t.Fatal("recovery did not apply the authoritative raw append exactly once")
	}
}

func TestLeaseHistoryOverridesVerifyTheCompleteRawExtent(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "quarantine", true: "oversized-tail"}[oversized], func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			r := msg.Record{Origin: "peer", Seq: 1, Ack: &msg.Ack{}}
			if oversized {
				r = msg.Record{Origin: "peer", Seq: 1, Msg: &msg.Message{ID: "0123456789", From: "a/agent", To: "b/agent", Body: strings.Repeat("x", maxLine+1024), At: time.Now().UTC()}}
			}
			data, _ := json.Marshal(r)
			data = append(data, '\n')
			path := filepath.Join(dir, "log.jsonl")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(dir, "n", WithLeaseRetention(LeaseRetentionOptions{Mode: LeaseRetentionObserve}))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got, err := s.SinceBytesChecked(nil, 10, 0)
			if err != nil || len(got) != 1 || got[0].Ack == nil || got[0].Ack.ID != QuarantineID {
				t.Fatalf("control is not a quarantine override: %v", err)
			}
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			// Damage the discarded tail for an oversized line, not just its header.
			offset := int64(len(data) - 4)
			if _, err = f.WriteAt([]byte{'!'}, offset); err != nil {
				t.Fatal(err)
			}
			f.Close()
			if got, err := s.SinceBytesChecked(nil, 10, 0); err == nil || got != nil {
				t.Fatal("override hid changed raw authority")
			}
		})
	}
}

func TestLeaseHistoryTestLaneOverridesAndSwapsVerifyRawAuthority(t *testing.T) {
	for _, pending := range []bool{true, false} {
		t.Run(map[bool]string{true: "pending-override", false: "rewritten-generation"}[pending], func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			clock := time.Now().UTC()
			s, err := Open(dir, "n", WithClock(func() time.Time { return clock }), WithLeaseRetention(LeaseRetentionOptions{Mode: LeaseRetentionObserve}))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := s.Send(msg.Message{ID: msg.NewTestID(), From: "writer/agent", To: "reader/agent", Body: strings.Repeat("test-body ", 1024), TTL: 1}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "log.jsonl")
			before, _ := os.ReadFile(path)
			if pending {
				s.lastRewrite = clock
			}
			clock = clock.Add(2 * time.Minute)
			_ = s.Messages()
			data, _ := os.ReadFile(path)
			if pending && !bytes.Equal(before, data) {
				t.Fatal("fixture unexpectedly rewrote raw log")
			}
			if !pending && len(data) >= len(before) {
				t.Fatal("fixture did not rewrite the test lane")
			}
			got, err := s.SinceBytesChecked(nil, 10, 0)
			if err != nil || len(got) != 1 || got[0].Msg == nil || !msg.TestPurged(*got[0].Msg) {
				t.Fatalf("purged control failed: %v", err)
			}
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.WriteAt([]byte{'!'}, int64(len(data)/2)); err != nil {
				t.Fatal(err)
			}
			f.Close()
			if got, err := s.SinceBytesChecked(nil, 10, 0); err == nil || got != nil {
				t.Fatal("test-lane projection hid raw corruption")
			}
		})
	}
}

func TestLeaseHistoryRefusesAReplacedRawGeneration(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, "n", WithLeaseRetention(LeaseRetentionOptions{Mode: LeaseRetentionObserve}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Send(msg.Message{From: "writer/agent", To: "reader/agent", Body: "ordinary"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "log.jsonl")
	data, _ := os.ReadFile(path)
	if err := os.Rename(path, path+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SinceBytesChecked(nil, 10, 0); err == nil || got != nil {
		t.Fatal("same bytes from a different raw generation were accepted")
	}
}

func TestLeaseHistoryCleansOnlyOwnedCrashRebuildFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".lease-history-rebuild-123456")
	other := filepath.Join(dir, ".lease-history-rebuild-operator-backup")
	for _, path := range []string{stale, other} {
		if err := os.WriteFile(path, []byte("derived fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(dir, "n", WithLeaseRetention(LeaseRetentionOptions{Mode: LeaseRetentionObserve}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatal("owned crash file was not removed")
	}
	if b, err := os.ReadFile(other); err != nil || string(b) != "derived fixture" {
		t.Fatal("unrelated file changed")
	}
}

func TestLeaseHistoryRefusesAliasedCrashFilesAndSidecars(t *testing.T) {
	for _, kind := range []string{"symlink-temp", "hardlink-temp", "hardlink-sidecar"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "original")
			if err := os.WriteFile(target, []byte("preserve this file"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, ".lease-history-rebuild-123456")
			if kind == "hardlink-sidecar" {
				path = filepath.Join(dir, "lease-history.db")
			}
			var err error
			if kind == "symlink-temp" {
				err = os.Symlink(target, path)
			} else {
				err = os.Link(target, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if s, err := Open(dir, "n", WithLeaseRetention(LeaseRetentionOptions{Mode: LeaseRetentionObserve})); err == nil || s != nil {
				t.Fatal("aliased derived path accepted")
			}
			if b, err := os.ReadFile(target); err != nil || string(b) != "preserve this file" {
				t.Fatal("aliased target changed")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("unsafe path was removed")
			}
		})
	}
}
