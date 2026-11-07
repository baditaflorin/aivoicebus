package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

var retentionStreamNow = time.Date(2031, 6, 1, 12, 0, 0, 0, time.UTC)

func retentionStreamRecords(count int, withLane bool) []msg.Record {
	var records []msg.Record
	add := func(m msg.Message) {
		records = append(records, msg.Record{Origin: "synthetic-peer", Seq: uint64(len(records) + 1), Msg: &m})
	}
	if withLane {
		id := msg.NewTestID()
		exp := retentionStreamNow.Add(time.Minute)
		add(msg.Message{ID: id, Thread: id, From: "fixture/a", To: "fixture/b", At: retentionStreamNow, Exp: &exp, Body: strings.Repeat("synthetic test lane ", 40)})
	}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%010x", i+1)
		owner := fmt.Sprintf("0docker-builder/fleet-runner.%032x", i)
		exp := retentionStreamNow.Add(2 * time.Hour)
		add(msg.Message{ID: id, Thread: id, From: owner, To: owner, Kind: msg.KindLease,
			Body: msg.LeaseBody(fmt.Sprintf("deploy:prod:fixture-%d", i), "synthetic active claim"), At: retentionStreamNow.Add(-time.Hour), Exp: &exp})
	}
	// Old managed payload is cold-eligible; the ordinary conversation is not.
	oldID := "eeee000001"
	owner := "0docker-builder/fleet-runner." + strings.Repeat("e", 32)
	expired := retentionStreamNow.Add(-45 * 24 * time.Hour)
	add(msg.Message{ID: oldID, Thread: oldID, From: owner, To: owner, Kind: msg.KindLease,
		Body: msg.LeaseBody("deploy:prod:old-fixture", "old synthetic claim"), At: expired.Add(-time.Hour), Exp: &expired})
	add(msg.Message{ID: "ffff000001", Thread: "ffff000001", From: "person/session", To: "person/other", At: retentionStreamNow, Body: "ordinary conversation survives"})
	return records
}

func retentionStreamStore(t *testing.T, mode store.LeaseRetentionMode, records []msg.Record) (*store.Store, string, [32]byte) {
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
	path := filepath.Join(dir, "log.jsonl")
	if err := os.WriteFile(path, raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(dir, "stream-fixture", store.WithClock(func() time.Time { return retentionStreamNow }),
		store.WithLeaseRetention(store.LeaseRetentionOptions{Mode: mode, Hosts: []string{"0docker-builder"},
			Keep: 30 * 24 * time.Hour, MaxHotRecords: 4096, MaxHotBytes: 8 << 20, CacheBytes: 8 << 20}))
	if err != nil {
		t.Fatalf("open retention mode %s: %v", mode, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path, sha256.Sum256(raw.Bytes())
}

func retentionStreamClient(t *testing.T, handler http.Handler) *client.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &client.Client{URLs: []string{srv.URL}, Token: "synthetic-stream-token", Timeout: 5 * time.Second, NoSign: true}
}

func TestLeaseRetentionOnHTTPListsBeyondCacheBudgetAndRenews(t *testing.T) {
	const count = 5003
	records := retentionStreamRecords(count, false)
	off, _, _ := retentionStreamStore(t, store.LeaseRetentionOff, records)
	on, rawPath, before := retentionStreamStore(t, store.LeaseRetentionOn, records)
	oldClient := retentionStreamClient(t, server.NewHandler(off, "synthetic-stream-token"))
	newClient := retentionStreamClient(t, server.NewHandler(on, "synthetic-stream-token"))
	compare := func() []msg.Lease {
		t.Helper()
		want, err := oldClient.Leases()
		if err != nil {
			t.Fatal(err)
		}
		got, err := newClient.Leases()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != count || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(msg.Holders(got), msg.Holders(want)) {
			t.Fatalf("complete HTTP lease/holder parity lost: got%d want%d", len(got), len(want))
		}
		return got
	}
	initial := compare()
	if got := on.Thread("ffff000001"); len(got) != 1 || got[0].Body != "ordinary conversation survives" {
		t.Fatal("ordinary conversation changed", got)
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil || sha256.Sum256(raw) != before {
		t.Fatal("opening/querying On changed authoritative raw history", err)
	}
	previous := initial[count-1]
	for _, s := range []*store.Store{off, on} {
		out, err := s.Send(msg.Message{ID: "dddd000001", From: previous.By, To: previous.By, Kind: msg.KindLease,
			Body: msg.LeaseBody(previous.Key, "synthetic replacement of "+previous.ID), TTL: 3 * 60 * 60})
		if err != nil || out.Exp == nil || !out.Exp.After(previous.Until) {
			t.Fatal("valid active claim could not renew past its previous expiry", err)
		}
		if err := s.Ack(msg.RetractPrefix+previous.ID, previous.By); err != nil {
			t.Fatal(err)
		}
	}
	compare()
}

// The wrapper observes actual object bytes, not just an opening '['. It also
// exposes Unwrap so ResponseController can flush/set deadlines on the real
// transport; the test must not manufacture a streaming failure itself.
type retentionStreamWriter struct {
	http.ResponseWriter
	onObject func()
	once     sync.Once
	mu       sync.Mutex
	body     bytes.Buffer
}

func (w *retentionStreamWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *retentionStreamWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.mu.Lock()
	w.body.Write(p[:n])
	w.mu.Unlock()
	if bytes.Contains(p[:n], []byte(`"key"`)) {
		w.once.Do(w.onObject)
	}
	return n, err
}

func (w *retentionStreamWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.body.Bytes()...)
}

func TestLeaseRetentionOnHTTPMidstreamReadFailureCannotSucceedPartially(t *testing.T) {
	s, rawPath, _ := retentionStreamStore(t, store.LeaseRetentionOn, retentionStreamRecords(600, false))
	handler := server.NewHandler(s, "synthetic-stream-token")
	var capture *retentionStreamWriter
	done := make(chan struct{})
	fault := make(chan error, 1)
	c := retentionStreamClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		capture = &retentionStreamWriter{ResponseWriter: w, onObject: func() { fault <- os.Truncate(rawPath, 0) }}
		handler.ServeHTTP(capture, r)
	}))
	_, err := c.Leases()
	if err == nil {
		t.Fatal("cold I/O failure after an emitted object became a valid successful lease set")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("failed stream did not terminate")
	}
	select {
	case e := <-fault:
		if e != nil {
			t.Fatal(e)
		}
	default:
		t.Fatal("response did not emit an actual lease object before fault")
	}
	if capture == nil || json.Valid(capture.bytes()) || !bytes.Contains(capture.bytes(), []byte(`"key"`)) {
		t.Fatal("I/O failure was hidden behind a valid array, or no actual object was emitted")
	}
}

func retentionBlockedStream(t *testing.T, s *store.Store) (*client.Client, <-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	handler := server.NewHandler(s, "synthetic-stream-token")
	c := retentionStreamClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := &retentionStreamWriter{ResponseWriter: w, onObject: func() { close(entered); <-release }}
		handler.ServeHTTP(writer, r)
	}))
	// Registered after server cleanup so a failing test unblocks the request
	// before httptest.Server.Close waits for its handler.
	t.Cleanup(unblock)
	return c, entered, unblock
}

func TestLeaseRetentionOnSlowHTTPWriterDoesNotPinRenewalOrTestLaneRewrite(t *testing.T) {
	const count = 600
	s, _, _ := retentionStreamStore(t, store.LeaseRetentionOn, retentionStreamRecords(count, true))
	c, entered, release := retentionBlockedStream(t, s)
	finished := make(chan error, 1)
	go func() {
		got, err := c.Leases()
		if err == nil && len(got) != count {
			err = fmt.Errorf("paused snapshot lost live claims: got%d want%d", len(got), count)
		}
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no actual object reached slow writer")
	}
	progress := make(chan error, 1)
	go func() {
		owner := "0docker-builder/fleet-runner." + strings.Repeat("0", 32)
		_, err := s.Send(msg.Message{ID: "dddd000002", From: owner, To: owner, Kind: msg.KindLease,
			Body: msg.LeaseBody("deploy:prod:fixture-0", "synthetic concurrent renewal"), TTL: 4 * 60 * 60})
		if err == nil {
			s.SetClock(func() time.Time { return retentionStreamNow.Add(6 * time.Minute) })
			s.Stats() // expires test lane and performs the atomic raw generation rewrite
		}
		progress <- err
	}()
	select {
	case err := <-progress:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow network writer pinned a renewal or raw rewrite")
	}
	release()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal("old snapshot did not survive append/rewrite", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not finish after writer resumed")
	}
}

func TestLeaseRetentionOnCloseIsNotPinnedBySlowHTTPWriter(t *testing.T) {
	s, _, _ := retentionStreamStore(t, store.LeaseRetentionOn, retentionStreamRecords(600, false))
	c, entered, release := retentionBlockedStream(t, s)
	finished := make(chan error, 1)
	go func() { _, err := c.Leases(); finished <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no actual object reached slow writer")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow HTTP writer pinned Store.Close")
	}
	release()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("store closure produced a valid partial lease response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closed store did not terminate the streaming request")
	}
}
