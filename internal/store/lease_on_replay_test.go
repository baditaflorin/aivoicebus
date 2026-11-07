package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func onReplayOpen(t *testing.T, dir string, now *time.Time, mode LeaseRetentionMode) *Store {
	t.Helper()
	return leaseHistoryOpen(t, dir, now, WithLeaseRetention(LeaseRetentionOptions{Mode: mode,
		Hosts: []string{"0docker-builder"}, Keep: 30 * 24 * time.Hour,
		MaxHotRecords: 4096, MaxHotBytes: 8 << 20, CacheBytes: 8 << 20}))
}

func TestLeaseRetentionOnReplayRebuildAndDurableRawTailParity(t *testing.T) {
	fixture := leaseHistoryFixture(t)
	for _, damage := range []string{"reopen", "missing", "corrupt", "truncated", "raw-append-before-index-commit", "torn-raw-tail"} {
		t.Run(damage, func(t *testing.T) {
			for _, mode := range []LeaseRetentionMode{LeaseRetentionOff, LeaseRetentionObserve, LeaseRetentionOn} {
				t.Run(string(mode), func(t *testing.T) {
					records := append([]msg.Record(nil), fixture...)
					now := leaseHistoryTime
					dir := t.TempDir()
					leaseHistoryWrite(t, dir, records)
					s := onReplayOpen(t, dir, &now, mode)
					leaseHistoryCheckRecords(t, s, records)
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
					index := filepath.Join(dir, "lease-history.db")
					if mode != LeaseRetentionOff {
						var err error
						switch damage {
						case "missing":
							err = os.Remove(index)
						case "corrupt":
							err = os.WriteFile(index, []byte("invalid disposable index"), 0o600)
						case "truncated":
							err = os.Truncate(index, 7)
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					if damage == "raw-append-before-index-commit" {
						until := now.Add(time.Hour)
						owner := "0docker-builder/fleet-runner." + strings.Repeat("b", 32)
						records = append(records, msg.Record{Origin: "late-peer", Seq: 1, Msg: &msg.Message{
							ID: "000000b001", Thread: "000000b001", From: owner, To: owner, Kind: msg.KindLease,
							Body: msg.LeaseBody("deploy:prod:raw-tail-fixture", "durable raw before sidecar"), At: now, Exp: &until}})
						leaseHistoryWrite(t, dir, records)
					}
					before, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
					if err != nil {
						t.Fatal(err)
					}
					var torn []byte
					if damage == "torn-raw-tail" {
						torn = []byte(`{"origin":"interrupted","seq":1,"msg":{"id":"cut`)
						f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
						if err != nil {
							t.Fatal(err)
						}
						_, writeErr := f.Write(torn)
						closeErr := f.Close()
						if writeErr != nil || closeErr != nil {
							t.Fatal(writeErr, closeErr)
						}
					}
					reopened := onReplayOpen(t, dir, &now, mode)
					leaseHistoryCheckRecords(t, reopened, records)
					if mode != LeaseRetentionOff && !reopened.LeaseRetentionStats().IndexHealthy {
						t.Fatal("sidecar did not rebuild from raw authority")
					}
					if after, err := os.ReadFile(filepath.Join(dir, "log.jsonl")); err != nil || !bytes.Equal(after, before) {
						t.Fatal("rebuild/reopen changed complete authoritative raw records", err)
					}
					if torn != nil {
						if b, err := os.ReadFile(filepath.Join(dir, "log.jsonl.torn")); err != nil || !bytes.Equal(b, torn) {
							t.Fatal("incomplete raw tail was lost rather than preserved", err)
						}
					}
				})
			}
		})
	}
}

func TestLeaseRetentionOnReplayMixedOriginsAndColdIDCollision(t *testing.T) {
	fixture := leaseHistoryFixture(t)
	for _, mode := range []LeaseRetentionMode{LeaseRetentionOff, LeaseRetentionObserve, LeaseRetentionOn} {
		t.Run(string(mode), func(t *testing.T) {
			now := leaseHistoryTime
			dir := t.TempDir()
			leaseHistoryWrite(t, dir, fixture)
			s := onReplayOpen(t, dir, &now, mode)
			original := *fixture[0].Msg
			beforeVV, beforeMessages := s.VV(), s.Stats().Messages
			_, err := s.Send(msg.Message{ID: original.ID, From: "other/session", To: "all", Body: "different owner collision"})
			if !errors.Is(err, ErrIDTaken) || !reflect.DeepEqual(s.VV(), beforeVV) {
				t.Fatal("cold original ID became available for another sender", err)
			}
			records := append([]msg.Record(nil), fixture...)
			records = append(records, msg.Record{Origin: "duplicate-peer", Seq: 1, Msg: &original})
			conflict := msg.Message{ID: original.ID, Thread: original.ID, From: "other/session", To: "all", Body: "replicated conflicting ID", At: now}
			records = append(records, msg.Record{Origin: "conflicting-peer", Seq: 1, Msg: &conflict})
			if n, err := s.Merge(records[len(fixture):]); err != nil || n != 2 {
				t.Fatal("mixed peer raw records were not preserved", n, err)
			}
			leaseHistoryCheckRecords(t, s, records)
			s.mu.Lock()
			got, exists := s.byIDLocked(original.ID)
			s.mu.Unlock()
			if !exists || !reflect.DeepEqual(got, original) || s.Stats().Messages != beforeMessages {
				t.Fatal("a mixed-origin duplicate or collision replaced/counts the cold original", got)
			}
		})
	}
}

func TestLeaseRetentionOnReplayTestLaneOffsetGenerationMatchesLegacy(t *testing.T) {
	now := leaseHistoryTime
	exp := now.Add(time.Minute)
	testID := msg.NewTestID()
	lane := msg.Message{ID: testID, Thread: testID, From: "fixture/a", To: "all", At: now, Exp: &exp, Body: strings.Repeat("owned synthetic lane ", 500)}
	managed := leaseHistoryFixture(t)[0]
	managed.Origin, managed.Seq = "peer", 2
	records := []msg.Record{{Origin: "peer", Seq: 1, Msg: &lane}, managed}
	var golden []msg.Record
	var goldenRaw []byte
	for _, mode := range []LeaseRetentionMode{LeaseRetentionOff, LeaseRetentionObserve, LeaseRetentionOn} {
		t.Run(string(mode), func(t *testing.T) {
			clock := now
			dir := t.TempDir()
			before := leaseHistoryWrite(t, dir, records)
			s := onReplayOpen(t, dir, &clock, mode)
			clock = now.Add(6 * time.Minute)
			s.Stats() // atomically replace the earlier variable-sized lane row
			feed := leaseHistoryFeed(t, s, 1)
			raw, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
			if err != nil || len(raw) >= len(before) {
				t.Fatal("fixture did not force an offset-changing raw rewrite", err)
			}
			if len(feed) != 2 || !reflect.DeepEqual(feed[1], managed) || !msg.TestPurged(*feed[0].Msg) {
				t.Fatal("new raw offsets lost or rewrote ordinary/managed history", feed)
			}
			if mode == LeaseRetentionOff {
				golden, goldenRaw = feed, raw
			} else if !reflect.DeepEqual(feed, golden) || !bytes.Equal(raw, goldenRaw) {
				t.Fatal("test-lane generation semantics differ from legacy")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := onReplayOpen(t, dir, &clock, mode)
			if got := leaseHistoryFeed(t, reopened, 1); !reflect.DeepEqual(got, golden) {
				t.Fatal("reopen reused offsets from the previous raw generation")
			}
		})
	}
}

func TestLeaseRetentionOnReplayActiveRawIOFailsClosedAcrossCheckedViews(t *testing.T) {
	now := leaseHistoryTime
	exp := now.Add(time.Hour)
	owner := "0docker-builder/fleet-runner." + strings.Repeat("c", 32)
	m := msg.Message{ID: "000000b002", Thread: "000000b002", From: owner, To: owner, Kind: msg.KindLease,
		Body: msg.LeaseBody("deploy:prod:active-io-fixture", "synthetic active claim"), At: now, Exp: &exp}
	dir := t.TempDir()
	leaseHistoryWrite(t, dir, []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}})
	s := onReplayOpen(t, dir, &now, LeaseRetentionOn)
	if live, err := s.LeasesChecked(); err != nil || len(live) != 1 {
		t.Fatal("healthy active control failed", live, err)
	}
	if err := os.Truncate(filepath.Join(dir, "log.jsonl"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LeasesChecked(); err == nil {
		t.Fatal("active raw corruption became a successful lease answer")
	}
	if s.HealthChecked() == nil {
		t.Fatal("active raw failure did not latch store health")
	}
	if _, err := s.SinceBytesChecked(nil, 10, 1024); err == nil {
		t.Fatal("latched failure became a successful sync answer")
	}
}
