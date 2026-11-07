package store

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
)

var onViewsNow = time.Date(2031, 6, 1, 12, 0, 0, 0, time.UTC)

func onViewsFixture(t *testing.T, mode LeaseRetentionMode) (*Store, []msg.Message, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old, active, recent := onViewsNow.Add(-45*24*time.Hour), onViewsNow.Add(time.Hour), onViewsNow.Add(-24*time.Hour)
	lease := func(id, nonce, key string, exp time.Time) msg.Message {
		owner := "0docker-builder/fleet-runner." + strings.Repeat(nonce, 32)
		return msg.Message{ID: id, Thread: id, From: owner, To: owner, At: exp.Add(-time.Hour), Exp: &exp, Kind: msg.KindLease, Body: msg.LeaseBody(key, "synthetic scoped lease")}
	}
	ms := []msg.Message{
		lease("000000aa01", "a", "deploy:prod:cold-fixture", old),
		lease("000000aa02", "b", "deploy:prod:active-fixture", active),
		lease("000000aa03", "c", "deploy:prod:recent-fixture", recent),
		lease("000000aa04", "d", "deploy:prod", old), // operator-wide, never scoped away
		{ID: "000000aa05", Thread: "000000aa05", From: "0docker-builder/fleet-runner." + strings.Repeat("e", 32), To: "person/reader", At: old, Body: "ordinary history from a nonce identity"},
	}
	ms = append(ms, msg.Message{ID: "000000aa06", Thread: ms[0].ID, Re: ms[0].ID, From: "person/reader", To: ms[0].From, At: onViewsNow.Add(-time.Hour), Body: "ordinary reply to cold managed root"})
	var raw bytes.Buffer
	for i := range ms {
		r := msg.Record{Origin: "fixture-peer", Seq: uint64(i + 1), Msg: &ms[i]}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	ack := msg.Ack{ID: ms[0].ID, By: "person/reader", At: onViewsNow.Add(-time.Hour), Via: msg.ViaInbox}
	b, err := json.Marshal(msg.Record{Origin: "fixture-peer", Seq: uint64(len(ms) + 1), Ack: &ack})
	if err != nil {
		t.Fatal(err)
	}
	raw.Write(b)
	raw.WriteByte('\n')
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := onViewsNow
	s, err := Open(dir, "views-fixture", WithClock(func() time.Time { return clock }), WithLeaseRetention(LeaseRetentionOptions{
		Mode: mode, Hosts: []string{"0docker-builder"}, Keep: 30 * 24 * time.Hour, MaxHotRecords: 4096, MaxHotBytes: 8 << 20, CacheBytes: 8 << 20,
	}))
	if err != nil {
		t.Fatalf("open %s: %v", mode, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, ms, &clock
}

func onViewsLookup(t *testing.T, s *Store, id string) msg.Message {
	t.Helper()
	s.mu.Lock()
	m, ok := s.byIDLocked(id)
	s.mu.Unlock()
	if !ok {
		t.Fatal("cold original disappeared from ByID", id)
	}
	return m
}

func onViewsAssertNotPinned(t *testing.T, s *Store, mode LeaseRetentionMode, id string) {
	t.Helper()
	if mode != LeaseRetentionOn {
		return
	}
	s.mu.Lock()
	_, resident := s.idx[id]
	s.mu.Unlock()
	if resident {
		t.Fatal("cold root was pinned in the ordinary Store payload index")
	}
}

func TestLeaseRetentionOnViewsColdByIDDedupAndOrdinaryAmendment(t *testing.T) {
	type view struct {
		Thread   []msg.Message
		Readers  int
		Receipt  msg.Receipts
		Arrived  []msg.Message
		VV       map[string]uint64
		Raw      []msg.Record
		RawBytes [32]byte
	}
	var golden view
	for _, mode := range []LeaseRetentionMode{LeaseRetentionOff, LeaseRetentionObserve, LeaseRetentionOn} {
		t.Run(string(mode), func(t *testing.T) {
			s, ms, _ := onViewsFixture(t, mode)
			root := ms[0]
			if got := onViewsLookup(t, s, root.ID); !reflect.DeepEqual(got, root) {
				t.Fatal("ByID no longer returns the exact historical original", got)
			}
			before := s.VV()
			duplicate, err := s.Send(root)
			if err != nil || duplicate.ID != root.ID || !reflect.DeepEqual(s.VV(), before) {
				t.Fatal("cold exact-ID duplicate appended a second record", err)
			}
			reply := ms[len(ms)-1]
			amendment, err := s.Send(msg.Message{ID: "000000aa10", From: reply.From, To: reply.To, Re: reply.ID, Body: "corrected ordinary reply"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Amend(amendment.ID, reply.From); err != nil {
				t.Fatal("ordinary amendment lost its cold-root thread", err)
			}
			rc, exists := s.Receipts(root.ID)
			if !exists || s.Readers(root.ID) != 1 || rc.Replies != 1 {
				t.Fatal("ordinary receipt/reply semantics changed", rc)
			}
			raw, err := os.ReadFile(filepath.Join(s.Dir(), "log.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			got := view{Thread: s.Thread(root.ID), Readers: s.Readers(root.ID), Receipt: rc,
				Arrived: s.ArrivedSince(onViewsNow.Add(-100 * 24 * time.Hour)), VV: s.VV(), Raw: s.Since(nil, 1000), RawBytes: sha256.Sum256(raw)}
			if len(got.Thread) != 3 || got.Thread[0].ID != root.ID || got.Thread[2].Amends == nil {
				t.Fatal("cold root plus ordinary reply/amendment was not a complete thread", got.Thread)
			}
			if mode == LeaseRetentionOff {
				golden = got
			} else if !reflect.DeepEqual(got, golden) {
				t.Fatalf("historical read/dedup/thread/receipt/raw parity changed: got%+v want%+v", got, golden)
			}
			onViewsAssertNotPinned(t, s, mode, root.ID)
		})
	}
}

func TestLeaseRetentionOnViewsScopedRetractionNoticesAndTruth(t *testing.T) {
	type view struct {
		Thread  []msg.Message
		Notices []msg.Message
		Receipt msg.Receipts
		Stats   Stats
		Raw     []msg.Record
	}
	var golden view
	for _, mode := range []LeaseRetentionMode{LeaseRetentionOff, LeaseRetentionObserve, LeaseRetentionOn} {
		t.Run(string(mode), func(t *testing.T) {
			s, ms, _ := onViewsFixture(t, mode)
			root := ms[0]
			result, err := s.Retract(root.ID, root.From, true)
			if err != nil || !result.OK || result.ReadBy != 1 {
				t.Fatal("cold managed root could not be retracted truthfully", result, err)
			}
			original := onViewsLookup(t, s, root.ID)
			if original.Body != root.Body {
				t.Fatal("ByID/raw truth was overwritten with a tombstone")
			}
			thread := s.Thread(root.ID)
			if len(thread) != 2 || thread[0].Retracted == nil || !thread[0].Retracted.Leak || thread[1].Body != ms[len(ms)-1].Body {
				t.Fatal("retraction lost cold truth or the ordinary reply", thread)
			}
			notices := s.Inbox("person/reader", false)
			if len(notices) != 1 || notices[0].From != msg.NoticeFrom || notices[0].Re != root.ID {
				t.Fatal("reader lost its scoped retraction notice", notices)
			}
			rc, _ := s.Receipts(root.ID)
			got := view{Thread: thread, Notices: notices, Receipt: rc, Stats: s.Stats(), Raw: s.Since(nil, 1000)}
			if mode == LeaseRetentionOff {
				golden = got
			} else if !reflect.DeepEqual(got, golden) {
				t.Fatalf("retraction notice/receipt/raw parity changed: got%+v want%+v", got, golden)
			}
			if err := s.Ack(notices[0].ID, "person/reader"); err != nil || len(s.Inbox("person/reader", false)) != 0 {
				t.Fatal("acknowledged notice reappeared", err)
			}
			onViewsAssertNotPinned(t, s, mode, root.ID)
		})
	}
}

func TestLeaseRetentionOnViewsTransientRosterHasExactDynamicScope(t *testing.T) {
	for _, mode := range []LeaseRetentionMode{LeaseRetentionOff, LeaseRetentionObserve, LeaseRetentionOn} {
		t.Run(string(mode), func(t *testing.T) {
			s, ms, clock := onViewsFixture(t, mode)
			for _, now := range []time.Time{onViewsNow, onViewsNow.Add(31 * 24 * time.Hour), onViewsNow.Add(-100 * 24 * time.Hour), onViewsNow} {
				*clock = now
				if err := s.SweepLeaseRetention(); err != nil {
					t.Fatal(err)
				}
				who := s.Who()
				roster := map[string]bool{}
				for _, r := range s.Roster() {
					roster[r.Me] = true
				}
				for i, m := range ms[:5] {
					want := true
					if mode == LeaseRetentionOn && i < 3 && !m.Exp.After(now.Add(-30*24*time.Hour)) {
						want = false
					}
					_, inWho := who[m.From]
					if inWho != want || roster[m.From] != want {
						t.Fatalf("transient identity scope changed at%s for%s: Who%t Roster%t want%t", now, m.From, inWho, roster[m.From], want)
					}
				}
				if got := onViewsLookup(t, s, ms[0].ID); got.Body != ms[0].Body || len(s.Thread(ms[0].ID)) != 2 {
					t.Fatal("transient roster aging destroyed full historical ByID/Thread")
				}
			}
		})
	}
}
