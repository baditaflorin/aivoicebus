package store

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

var leaseHistoryTime = time.Date(2031, 6, 1, 12, 0, 0, 0, time.UTC)

func leaseHistoryObserve() Option {
	return WithLeaseRetention(LeaseRetentionOptions{
		Mode: LeaseRetentionObserve, Keep: 30 * 24 * time.Hour,
		Hosts: []string{"0docker-builder"}, MaxHotRecords: 4096,
		MaxHotBytes: 8 << 20, CacheBytes: 8 << 20,
	})
}

func leaseHistoryOpen(t *testing.T, dir string, now *time.Time, options ...Option) *Store {
	t.Helper()
	options = append([]Option{WithClock(func() time.Time { return *now })}, options...)
	s, err := Open(dir, "local", options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func leaseHistoryFixture(t *testing.T) []msg.Record {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sequence := map[string]uint64{}
	var records []msg.Record
	add := func(origin string, m *msg.Message, ack *msg.Ack) {
		sequence[origin]++
		r := msg.Record{Origin: origin, Seq: sequence[origin], Msg: m, Ack: ack}
		if err := r.ValidateReplicated(); err != nil {
			t.Fatalf("invalid synthetic record: %v", err)
		}
		records = append(records, r)
	}
	for i := 0; i < 12; i++ {
		origin := []string{"peer-z", "peer-a"}[i%2]
		id := fmt.Sprintf("%010x", i+1)
		by := fmt.Sprintf("0docker-builder/fleet-runner.%032x", i+1)
		exp := leaseHistoryTime.Add(-45 * 24 * time.Hour)
		m := &msg.Message{ID: id, From: by, To: by, Thread: id,
			Kind: msg.KindLease, Body: msg.LeaseBody("deploy:prod:go_example", fmt.Sprintf("synthetic run %d", i)),
			At: exp.Add(-time.Hour), Exp: &exp}
		m.Sign(private, nil)
		add(origin, m, nil)
		if i%3 == 0 {
			add(origin, nil, &msg.Ack{ID: msg.RetractPrefix + id, By: by, At: exp.Add(time.Minute)})
		}
	}
	question := &msg.Message{ID: "000000f001", From: "mac-a/codex.fixture", To: "mac-b/claude.fixture",
		Thread: "000000f001", Kind: msg.KindQuestion, Body: "ordinary question\n" + strings.Repeat("words ", 400), At: leaseHistoryTime.Add(-time.Hour)}
	question.Sign(private, nil)
	add("peer-a", question, nil)
	answer := &msg.Message{ID: "000000f002", From: "mac-b/claude.fixture", To: question.From,
		Thread: question.ID, Re: question.ID, Kind: msg.KindAnswer, Body: "ordinary answer", At: leaseHistoryTime.Add(-30 * time.Minute)}
	answer.Sign(private, nil)
	add("peer-z", answer, nil)
	add("peer-a", nil, &msg.Ack{ID: question.ID, By: answer.From, At: leaseHistoryTime.Add(-20 * time.Minute), Via: msg.ViaCLI, Chars: len(question.Body)})
	add("peer-z", nil, &msg.Ack{ID: msg.RetractPrefix + question.ID, By: question.From, At: leaseHistoryTime.Add(-10 * time.Minute)})
	task := &msg.Message{ID: "000000f003", From: question.From, To: "all", Thread: "000000f003",
		Kind: msg.KindTask, Body: "ordinary task remains in history", At: leaseHistoryTime.Add(-time.Hour)}
	task.Sign(private, nil)
	add("peer-a", task, nil)
	claim := &msg.Message{ID: "000000f004", From: answer.From, To: question.From, Thread: task.ID,
		Re: task.ID, Kind: msg.KindClaim, Body: "ordinary claim", At: leaseHistoryTime.Add(-30 * time.Minute)}
	claim.Sign(private, nil)
	add("peer-z", claim, nil)
	return records
}

func leaseHistoryBytes(t *testing.T, records []msg.Record) []byte {
	t.Helper()
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	return out.Bytes()
}

func leaseHistoryWrite(t *testing.T, dir string, records []msg.Record) []byte {
	t.Helper()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := leaseHistoryBytes(t, records)
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func leaseHistorySorted(records []msg.Record) []msg.Record {
	out := append([]msg.Record(nil), records...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Origin == out[j].Origin {
			return out[i].Seq < out[j].Seq
		}
		return out[i].Origin < out[j].Origin
	})
	return out
}

func leaseHistoryFeed(t *testing.T, s *Store, maxBytes int) []msg.Record {
	t.Helper()
	vv := map[string]uint64{}
	var out []msg.Record
	for range 1000 {
		page := s.SinceBytes(vv, 3, maxBytes)
		if len(page) == 0 {
			return out
		}
		for _, record := range page {
			if record.Seq != vv[record.Origin]+1 {
				t.Fatalf("sync page skipped/replayed a sequence: vv=%v record=%s/%d", vv, record.Origin, record.Seq)
			}
			vv[record.Origin] = record.Seq
		}
		out = append(out, page...)
	}
	t.Fatal("sync pages never finished")
	return nil
}

func leaseHistoryCheckRecords(t *testing.T, s *Store, records []msg.Record) {
	t.Helper()
	want := leaseHistorySorted(records)
	vv := map[string]uint64{}
	for _, record := range records {
		vv[record.Origin] = max(vv[record.Origin], record.Seq)
	}
	if got := s.VV(); !reflect.DeepEqual(got, vv) {
		t.Fatalf("version vector changed: got%v want%v", got, vv)
	}
	for _, pageBytes := range []int{0, 1, 512, 2048} {
		got := leaseHistoryFeed(t, s, pageBytes)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("records changed during byte-bounded sync/export (limit%d)", pageBytes)
		}
		if !bytes.Equal(leaseHistoryBytes(t, got), leaseHistoryBytes(t, want)) {
			t.Fatal("export-equivalent JSON lines changed")
		}
		for _, record := range got {
			if record.Msg != nil && record.Msg.Sig != "" {
				if signed, valid := record.Msg.VerifySig(); !signed || !valid {
					t.Fatalf("replication changed signed content for%s/%d", record.Origin, record.Seq)
				}
			}
		}
	}
}

func TestLeaseHistoryObservePreservesRawVectorsFeedsAndOrdinaryViews(t *testing.T) {
	records := leaseHistoryFixture(t)
	now := leaseHistoryTime
	legacyDir, observedDir := t.TempDir(), t.TempDir()
	wantRaw := leaseHistoryWrite(t, legacyDir, records)
	leaseHistoryWrite(t, observedDir, records)
	legacy := leaseHistoryOpen(t, legacyDir, &now)
	observed := leaseHistoryOpen(t, observedDir, &now, leaseHistoryObserve())
	if err := observed.SweepLeaseRetention(); err != nil {
		t.Fatal(err)
	}
	leaseHistoryCheckRecords(t, legacy, records)
	leaseHistoryCheckRecords(t, observed, records)
	if !reflect.DeepEqual(legacy.Messages(), observed.Messages()) || !reflect.DeepEqual(legacy.Who(), observed.Who()) ||
		!reflect.DeepEqual(legacy.Stats(), observed.Stats()) || !reflect.DeepEqual(legacy.Roster(), observed.Roster()) {
		t.Fatal("observe mode changed the ordinary history/board/statistics projection")
	}
	for _, id := range []string{"0000000001", "000000f001", "000000f003"} {
		if !reflect.DeepEqual(legacy.Thread(id), observed.Thread(id)) {
			t.Fatalf("observe mode changed thread%s", id)
		}
	}
	if !reflect.DeepEqual(legacy.Inbox("mac-b/claude.fixture", true), observed.Inbox("mac-b/claude.fixture", true)) {
		t.Fatal("ordinary inbox changed")
	}
	status := observed.LeaseRetentionStats()
	if !status.IndexHealthy || status.IndexedRecords != uint64(len(records)) || status.ColdRecords != 0 || status.CacheBytes > 8<<20 {
		t.Fatalf("observe index is unhealthy, missing records, evicting or over budget: %+v", status)
	}
	for _, dir := range []string{legacyDir, observedDir} {
		if got, err := os.ReadFile(filepath.Join(dir, "log.jsonl")); err != nil || !bytes.Equal(got, wantRaw) {
			t.Fatalf("ordinary raw audit bytes changed: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(legacyDir, "lease-history.db")); !os.IsNotExist(err) {
		t.Fatal("default-off store created a retention sidecar")
	}
	if err := observed.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := leaseHistoryOpen(t, observedDir, &now, leaseHistoryObserve())
	leaseHistoryCheckRecords(t, reopened, records)
	if got, _ := os.ReadFile(filepath.Join(observedDir, "log.jsonl")); !bytes.Equal(got, wantRaw) {
		t.Fatal("reopen rewrote ordinary audit data")
	}
}

func TestLeaseHistoryObserveReadFailureNeverLooksLikeEmptyHistory(t *testing.T) {
	records := leaseHistoryFixture(t)
	now := leaseHistoryTime
	dir := t.TempDir()
	before := leaseHistoryWrite(t, dir, records)
	s := leaseHistoryOpen(t, dir, &now, leaseHistoryObserve())
	if err := os.Truncate(filepath.Join(dir, "log.jsonl"), 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.SinceBytesChecked(map[string]uint64{}, 3, 512)
	if err == nil || len(got) != 0 {
		t.Fatalf("failed cold read appeared as valid/partial history: records%d err%v", len(got), err)
	}
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := leaseHistoryOpen(t, dir, &now, leaseHistoryObserve())
	leaseHistoryCheckRecords(t, reopened, records)
}

func TestLeaseHistoryObserveRebuildsMissingCorruptAndStaleIndexes(t *testing.T) {
	for _, damage := range []string{"missing", "corrupt", "truncated", "raw-append-before-index-commit"} {
		t.Run(damage, func(t *testing.T) {
			records := leaseHistoryFixture(t)
			now := leaseHistoryTime
			dir := t.TempDir()
			leaseHistoryWrite(t, dir, records)
			s := leaseHistoryOpen(t, dir, &now, leaseHistoryObserve())
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			index := filepath.Join(dir, "lease-history.db")
			switch damage {
			case "missing":
				if err := os.Remove(index); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(index, []byte("not a valid derived index\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				if err := os.Truncate(index, 7); err != nil {
					t.Fatal(err)
				}
			case "raw-append-before-index-commit":
				records = append(records, msg.Record{Origin: "local", Seq: 1, Msg: &msg.Message{
					ID: "000000ff01", From: "mac-a/codex.fixture", To: "all", Thread: "000000ff01", Body: "durable raw append after old index", At: now}})
				leaseHistoryWrite(t, dir, records)
			}
			before, _ := os.ReadFile(filepath.Join(dir, "log.jsonl"))
			reopened := leaseHistoryOpen(t, dir, &now, leaseHistoryObserve())
			leaseHistoryCheckRecords(t, reopened, records)
			if !reopened.LeaseRetentionStats().IndexHealthy {
				t.Fatal("derived index did not recover from raw authority")
			}
			if after, _ := os.ReadFile(filepath.Join(dir, "log.jsonl")); !bytes.Equal(after, before) {
				t.Fatal("rebuilding derived state changed authoritative raw bytes")
			}
		})
	}
}

func TestLeaseHistoryObserveTornTailDoesNotInventCommittedSequence(t *testing.T) {
	records := leaseHistoryFixture(t)
	now := leaseHistoryTime
	dir := t.TempDir()
	before := leaseHistoryWrite(t, dir, records)
	s := leaseHistoryOpen(t, dir, &now, leaseHistoryObserve())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	torn := []byte(`{"origin":"local","seq":1,"msg":{"id":"cut`)
	file, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(torn)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("write torn fixture: %v %v", writeErr, closeErr)
	}
	reopened := leaseHistoryOpen(t, dir, &now, leaseHistoryObserve())
	leaseHistoryCheckRecords(t, reopened, records)
	if got, _ := os.ReadFile(filepath.Join(dir, "log.jsonl")); !bytes.Equal(got, before) {
		t.Fatal("uncommitted tail became authoritative or damaged preceding records")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "log.jsonl.torn")); !bytes.Equal(got, torn) {
		t.Fatal("the interrupted append was not preserved separately")
	}
}

func TestLeaseHistoryObserveTracksTestLaneOffsetGeneration(t *testing.T) {
	now := leaseHistoryTime
	end := now.Add(time.Minute)
	testID := msg.NewTestID()
	testMessage := &msg.Message{ID: testID, From: "mac-a/codex.fixture", To: "all", Thread: testID,
		Body: strings.Repeat("large disposable test body ", 700), At: now, Exp: &end}
	records := []msg.Record{{Origin: "peer-a", Seq: 1, Msg: testMessage}}
	lease := leaseHistoryFixture(t)[0]
	lease.Origin, lease.Seq = "peer-a", 2
	records = append(records, lease)
	dir := t.TempDir()
	before := leaseHistoryWrite(t, dir, records)
	s := leaseHistoryOpen(t, dir, &now, leaseHistoryObserve())
	leaseHistoryCheckRecords(t, s, records)
	now = now.Add(10 * time.Minute)
	s.Stats() // existing test-lane purge atomically rewrites the earlier variable-length row
	after, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if err != nil || len(after) >= len(before) {
		t.Fatalf("fixture did not force an earlier-row offset change: %v", err)
	}
	got := s.SinceBytes(map[string]uint64{"peer-a": 1}, 1, 1)
	if len(got) != 1 || !reflect.DeepEqual(got[0], lease) {
		t.Fatal("live rewrite served a stale offset or changed the ordinary lease")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := leaseHistoryOpen(t, dir, &now, leaseHistoryObserve())
	got = reopened.SinceBytes(map[string]uint64{"peer-a": 1}, 1, 1)
	if len(got) != 1 || !reflect.DeepEqual(got[0], lease) || reopened.VV()["peer-a"] != 2 {
		t.Fatal("reopen lost the matching raw/index generation")
	}
	if gotRaw, _ := os.ReadFile(filepath.Join(dir, "log.jsonl")); !bytes.Equal(gotRaw, after) {
		t.Fatal("index reopen rewrote the already-purged raw generation")
	}
}

func TestLeaseHistoryObserveMixedPeerCatchupAndDuplicateIDsStayCompatible(t *testing.T) {
	records := leaseHistoryFixture(t)
	now := leaseHistoryTime
	sourceDir, observedDir := t.TempDir(), t.TempDir()
	originalRaw := leaseHistoryWrite(t, sourceDir, records)
	if err := os.Chmod(observedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := leaseHistoryOpen(t, sourceDir, &now)
	observed := leaseHistoryOpen(t, observedDir, &now, leaseHistoryObserve())
	for range 1000 {
		page := legacy.SinceBytes(observed.VV(), 3, 512)
		if len(page) == 0 {
			break
		}
		if n, err := observed.Merge(page); err != nil || n != len(page) {
			t.Fatalf("new node rejected an old peer's page: %d/%d %v", n, len(page), err)
		}
	}
	leaseHistoryCheckRecords(t, observed, records)
	oldCatchup := leaseHistoryOpen(t, t.TempDir(), &now)
	for range 1000 {
		page := observed.SinceBytes(oldCatchup.VV(), 2, 256)
		if len(page) == 0 {
			break
		}
		if n, err := oldCatchup.Merge(page); err != nil || n != len(page) {
			t.Fatalf("old peer rejected indexed history: %d/%d %v", n, len(page), err)
		}
	}
	leaseHistoryCheckRecords(t, oldCatchup, records)
	if !reflect.DeepEqual(oldCatchup.Messages(), observed.Messages()) {
		t.Fatal("mixed peer changed ordinary board projection")
	}
	beforeVV := observed.VV()
	if n, err := observed.Merge(legacy.Since(map[string]uint64{}, 1000)); err != nil || n != 0 || !reflect.DeepEqual(observed.VV(), beforeVV) {
		t.Fatalf("old peer replay invented new sequences: n%d err%v", n, err)
	}
	original := *records[0].Msg // retracted old lease, with its original sender/signature/expiry
	for _, s := range []*Store{legacy, observed} {
		before := s.VV()
		got, err := s.Send(original)
		if err != nil || got.ID != original.ID || got.Retracted == nil || got.Body == original.Body || !reflect.DeepEqual(s.VV(), before) {
			t.Fatalf("ID retry resurrected an expired/retracted message or rewrote history: %+v %v", got, err)
		}
		other := original
		other.Body += " different content"
		if _, err := s.Send(other); !errors.Is(err, ErrIDTaken) || !reflect.DeepEqual(s.VV(), before) {
			t.Fatalf("existing ID accepted changed content: %v", err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(sourceDir, "log.jsonl")); !bytes.Equal(got, originalRaw) {
		t.Fatal("idempotent/rejected sends changed legacy raw audit data")
	}
	// A legitimate copy under another origin occupies its sequence without a
	// duplicate visible message. A conflicting copy also keeps the first arrival,
	// just as old peers do; every wire record must still remain available.
	duplicate := msg.Record{Origin: "copy-peer", Seq: 1, Msg: &original}
	conflict := msg.Record{Origin: "conflict-peer", Seq: 1, Msg: &msg.Message{
		ID: original.ID, From: "other/codex.fixture", To: "all", Thread: original.ID,
		Body: "another sender drew the same identifier", At: now}}
	for _, s := range []*Store{legacy, observed} {
		for _, record := range []msg.Record{duplicate, conflict} {
			if n, err := s.Merge([]msg.Record{record}); err != nil || n != 1 {
				t.Fatalf("duplicate ID dropped its new origin sequence: %d %v", n, err)
			}
		}
	}
	withDuplicates := append(append([]msg.Record(nil), records...), duplicate, conflict)
	leaseHistoryCheckRecords(t, observed, withDuplicates)
	if !reflect.DeepEqual(legacy.Thread(original.ID), observed.Thread(original.ID)) {
		t.Fatal("duplicate origin or conflicting content replaced the first held message")
	}
	if err := observed.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := leaseHistoryOpen(t, observedDir, &now, leaseHistoryObserve())
	leaseHistoryCheckRecords(t, reopened, withDuplicates)
	if !reflect.DeepEqual(legacy.Thread(original.ID), reopened.Thread(original.ID)) {
		t.Fatal("index rebuild selected the latest collision instead of first arrival")
	}
}

func TestLeaseHistoryObserveKeepsExistingQuarantineAndRefusalSemantics(t *testing.T) {
	now := leaseHistoryTime
	good := &msg.Message{ID: "000000ab01", From: "mac-a/codex.fixture", To: "all", Thread: "000000ab01", Body: "valid history", At: now}
	bad := *good
	bad.ID, bad.Thread, bad.Kind, bad.Body = "000000ab02", "000000ab02", "not-a-wire-kind", "invalid content must not leak into sync"
	after := *good
	after.ID, after.Thread, after.Body = "000000ab03", "000000ab03", "valid history after quarantined sequence"
	records := []msg.Record{
		{Origin: "peer-a", Seq: 1, Msg: good},
		{Origin: "peer-a", Seq: 2, Msg: &bad},
		{Origin: "peer-a", Seq: 3, Msg: &after},
		{Origin: "unusable\norigin", Seq: 1, Msg: good},
	}
	legacyDir, observedDir := t.TempDir(), t.TempDir()
	raw := leaseHistoryWrite(t, legacyDir, records)
	leaseHistoryWrite(t, observedDir, records)
	legacy := leaseHistoryOpen(t, legacyDir, &now)
	observed := leaseHistoryOpen(t, observedDir, &now, leaseHistoryObserve())
	if !reflect.DeepEqual(legacy.VV(), observed.VV()) || observed.VV()["peer-a"] != 3 {
		t.Fatal("invalid records changed the legacy sequence/refusal contract")
	}
	if !reflect.DeepEqual(legacy.Since(map[string]uint64{}, 20), observed.Since(map[string]uint64{}, 20)) {
		t.Fatal("indexed sync did not retain the legacy inert quarantine placeholder")
	}
	if !reflect.DeepEqual(legacy.Stats(), observed.Stats()) || observed.Stats().Quarantined != 1 || observed.Stats().Refused != 1 {
		t.Fatalf("index reconstruction changed quarantine/refusal counts: old%+v new%+v", legacy.Stats(), observed.Stats())
	}
	if bytes.Contains(leaseHistoryBytes(t, observed.Since(map[string]uint64{}, 20)), []byte(bad.Body)) {
		t.Fatal("read-through served quarantined content")
	}
	if got, _ := os.ReadFile(filepath.Join(observedDir, "log.jsonl")); !bytes.Equal(got, raw) {
		t.Fatal("quarantine/index projection rewrote raw audit evidence")
	}
}
