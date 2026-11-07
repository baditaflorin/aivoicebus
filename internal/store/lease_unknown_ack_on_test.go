package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

var unknownAckOnNow = time.Date(2031, 6, 1, 12, 0, 0, 0, time.UTC)

type unknownAckOnView struct {
	Stats         store.Stats
	Readers       int
	Receipt       msg.Receipts
	ReceiptExists bool
	Thread        []msg.Message
	Leases        []msg.Lease
	VV            map[string]uint64
	RawFeed       []msg.Record
	RawFileSHA256 [32]byte
}

func unknownAckOnSnapshot(t *testing.T, s *store.Store, id string) unknownAckOnView {
	t.Helper()
	result := unknownAckOnView{Stats: s.Stats(), Readers: s.Readers(id), Thread: s.Thread(id), VV: s.VV(), RawFeed: s.Since(nil, 1000)}
	result.Receipt, result.ReceiptExists = s.Receipts(id)
	var err error
	result.Leases, err = s.LeasesChecked()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir(), "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	result.RawFileSHA256 = sha256.Sum256(raw)
	return result
}

func unknownAckOnOpen(t *testing.T, dir string, mode store.LeaseRetentionMode, clock *time.Time) *store.Store {
	t.Helper()
	s, err := store.Open(dir, "unknown-ack-fixture", store.WithClock(func() time.Time { return *clock }),
		store.WithLeaseRetention(store.LeaseRetentionOptions{Mode: mode, Keep: 30 * 24 * time.Hour,
			Hosts: []string{"0docker-builder"}, MaxHotRecords: 4096, MaxHotBytes: 8 << 20, CacheBytes: 8 << 20}))
	if err != nil {
		t.Fatalf("open %s: %v", mode, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestLeaseRetentionOnUnknownAckPublicParity(t *testing.T) {
	id := "0000000a01"
	owner := "0docker-builder/fleet-runner." + strings.Repeat("a", 32)
	ordinary := msg.Message{ID: id, Thread: id, From: "sender/session", To: "reader/session", At: unknownAckOnNow, Body: "ordinary synthetic history must survive"}
	expired := unknownAckOnNow.Add(-45 * 24 * time.Hour)
	managed := msg.Message{ID: id, Thread: id, From: owner, To: owner, At: expired.Add(-time.Hour), Exp: &expired,
		Kind: msg.KindLease, Body: msg.LeaseBody("deploy:prod:unknown-ack-fixture", "expired synthetic managed history")}
	liveUntil := unknownAckOnNow.Add(2 * time.Hour)
	liveManaged := managed
	liveManaged.At, liveManaged.Exp = unknownAckOnNow, &liveUntil
	count := msg.Ack{ID: id, By: "reader/session", At: unknownAckOnNow, Via: msg.ViaHookCount}
	full := count
	full.At, full.Via = unknownAckOnNow.Add(time.Hour), msg.ViaInbox
	claim := func(by string, leak bool, at time.Time) msg.Ack {
		prefix := msg.RetractPrefix
		if leak {
			prefix = msg.RetractLeakPrefix
		}
		return msg.Ack{ID: prefix + id, By: by, At: at}
	}
	cases := []struct {
		name          string
		acks          []msg.Ack
		target        *msg.Message
		wantRetracted bool
		wantLeak      bool
		wantReaders   int
	}{
		{name: "unknown-forever", acks: []msg.Ack{count, full, claim("sender/session", false, unknownAckOnNow.Add(2*time.Hour))}, wantReaders: 1},
		{name: "ordinary-later", acks: []msg.Ack{count, full, claim("sender/session", false, unknownAckOnNow.Add(2*time.Hour))}, target: &ordinary, wantReaders: 1, wantRetracted: true},
		{name: "own-claim-before-managed-target", acks: []msg.Ack{claim(owner, false, unknownAckOnNow.Add(time.Hour))}, target: &managed, wantRetracted: true},
		{name: "unauthorized-unknown-before-managed", acks: []msg.Ack{claim("wrong-machine/session", false, unknownAckOnNow.Add(time.Hour))}, target: &managed},
		{name: "unauthorized-unknown-before-ordinary", acks: []msg.Ack{claim("wrong-machine/session", true, unknownAckOnNow.Add(time.Hour))}, target: &ordinary},
		{name: "stronger-leak-after-normal-claim", acks: []msg.Ack{claim(owner, false, unknownAckOnNow.Add(time.Hour)), claim(owner, true, unknownAckOnNow.Add(2*time.Hour)), claim(owner, false, unknownAckOnNow)}, target: &managed, wantRetracted: true, wantLeak: true},
		{name: "duplicate-origin-receipt-upgrade", acks: []msg.Ack{count, full}, target: &ordinary, wantReaders: 1},
		{name: "future-receipt-active-target-clock-rollback", acks: []msg.Ack{{ID: id, By: "reader/session", At: unknownAckOnNow.Add(24 * time.Hour), Via: msg.ViaInbox}}, target: &liveManaged, wantReaders: 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var golden []unknownAckOnView
			for _, mode := range []store.LeaseRetentionMode{store.LeaseRetentionOff, store.LeaseRetentionObserve, store.LeaseRetentionOn} {
				t.Run(string(mode), func(t *testing.T) {
					dir := t.TempDir()
					if err := os.Chmod(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					var records []msg.Record
					for i := range test.acks {
						a := test.acks[i]
						records = append(records, msg.Record{Origin: "fixture-a", Seq: uint64(i + 1), Ack: &a})
						if i == 0 {
							// Distinct origin/sequence, identical logical ACK. It must
							// stay in the raw feed without inflating public counts.
							records = append(records, msg.Record{Origin: "fixture-duplicate", Seq: 1, Ack: &a})
						}
					}
					var raw bytes.Buffer
					for _, record := range records {
						if err := record.ValidateReplicated(); err != nil {
							t.Fatal("invalid synthetic record", err)
						}
						b, err := json.Marshal(record)
						if err != nil {
							t.Fatal(err)
						}
						raw.Write(b)
						raw.WriteByte('\n')
					}
					if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), raw.Bytes(), 0o600); err != nil {
						t.Fatal(err)
					}
					clock := unknownAckOnNow
					s := unknownAckOnOpen(t, dir, mode, &clock)
					var views []unknownAckOnView
					capture := func() {
						t.Helper()
						views = append(views, unknownAckOnSnapshot(t, s, id))
					}
					captureClocks := func() {
						t.Helper()
						for _, at := range []time.Time{unknownAckOnNow, unknownAckOnNow.Add(90 * 24 * time.Hour), unknownAckOnNow.Add(-24 * time.Hour)} {
							clock = at
							if err := s.SweepLeaseRetention(); err != nil {
								t.Fatal(err)
							}
							capture()
							if s.Readers(id) != test.wantReaders {
								t.Fatalf("unknown/read receipt lost across aging: got%d want%d", s.Readers(id), test.wantReaders)
							}
						}
					}
					captureClocks() // unknown targets may remain unknown indefinitely
					if _, exists := s.Receipts(id); exists {
						t.Fatal("unknown target became a message")
					}
					if test.target != nil {
						n, err := s.Merge([]msg.Record{{Origin: "fixture-target", Seq: 1, Msg: test.target}})
						if err != nil || n != 1 {
							t.Fatal("late target did not merge", n, err)
						}
						captureClocks()
						thread := s.Thread(id)
						if len(thread) != 1 || (thread[0].Retracted != nil) != test.wantRetracted {
							t.Fatal("late target retraction authority changed", thread)
						}
						if test.wantLeak && !thread[0].Retracted.Leak {
							t.Fatal("later/duplicate normal claim downgraded stronger leak state")
						}
					}
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
					s = unknownAckOnOpen(t, dir, mode, &clock)
					capture() // raw replay/sidecar rebuild must keep the same contract
					if mode == store.LeaseRetentionOff {
						golden = views
					} else if !reflect.DeepEqual(views, golden) {
						for i := range views {
							if i >= len(golden) || !reflect.DeepEqual(views[i], golden[i]) {
								t.Fatalf("public ACK/receipt/thread/VV/raw parity differs at stage%d: got%+v want%+v", i, views[i], golden[i])
							}
						}
					}
				})
			}
		})
	}
}
