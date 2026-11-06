package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// The real node handles signing, sends and retractions; only lease reads fail.
type leaseFailureNode struct {
	store              *store.Store
	client             *client.Client
	sends              atomic.Int32
	checks             atomic.Int32
	fail               atomic.Bool
	lost               atomic.Bool
	empty              atomic.Bool
	stale              atomic.Bool
	watch              atomic.Bool
	gaps               atomic.Int32
	rejectRetract      atomic.Bool
	mu                 sync.Mutex
	created, retracted []string
	written            []msg.Message
}

func newLeaseFailureNode(t *testing.T, fail func(*leaseFailureNode) bool, timeout bool) *leaseFailureNode {
	t.Helper()
	n := &leaseFailureNode{}
	var err error
	n.store, err = store.Open(t.TempDir(), "failure-fixture")
	if err != nil {
		t.Fatal(err)
	}
	n.store.SetSendLimit(1 << 20)
	n.store.SetRetractLimit(1 << 20)
	inner := server.NewHandler(n.store, "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/leases" {
			n.checks.Add(1)
			failing := fail(n)
			if n.empty.Load() {
				_, _ = io.WriteString(w, "[]")
				return
			}
			if n.lost.Load() {
				_ = json.NewEncoder(w).Encode([]msg.Lease{{Key: leaseKey, By: "fixture/other", ID: "0123456789", At: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour)}})
				return
			}
			if n.stale.Load() && n.sends.Load() > 1 {
				n.mu.Lock()
				first := n.created[0]
				n.mu.Unlock()
				for _, l := range n.store.Leases() {
					if l.ID == first {
						_ = json.NewEncoder(w).Encode([]msg.Lease{l})
						return
					}
				}
				_, _ = io.WriteString(w, "[]")
				return
			}
			if failing {
				if timeout {
					<-r.Context().Done()
					return
				}
				http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		if r.URL.Path == "/v1/retract" {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			var req msg.RetractRequest
			_ = json.Unmarshal(b, &req)
			n.mu.Lock()
			n.retracted = append(n.retracted, req.ID)
			n.mu.Unlock()
			if n.rejectRetract.Load() {
				http.Error(w, "fixture retraction unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		if r.URL.Path == "/v1/retract" && n.watch.Load() && len(n.store.Leases()) == 0 {
			n.gaps.Add(1)
		}
		if r.URL.Path == "/v1/send" && rec.Code == http.StatusOK {
			var m msg.Message
			if err := json.Unmarshal(rec.Body.Bytes(), &m); err == nil && m.ID != "" {
				n.mu.Lock()
				n.created = append(n.created, m.ID)
				n.written = append(n.written, m)
				n.mu.Unlock()
				n.sends.Add(1)
			}
		}
		for k, values := range rec.Header() {
			w.Header()[k] = values
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_ME", "fixture/runner")
	t.Setenv("AIVOICEBUS_AGENT", "")
	n.client = &client.Client{URLs: []string{srv.URL}, Token: "tok", Timeout: 500 * time.Millisecond, Warn: io.Discard}
	return n
}

func (n *leaseFailureNode) assertOnlyOwnCleanup(t *testing.T) {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, id := range n.retracted {
		found := false
		for _, own := range n.created {
			found = found || id == own
		}
		if !found {
			t.Errorf("retracted a record this run did not create: %s", id)
		}
	}
	for _, own := range n.created {
		for _, live := range n.store.Leases() {
			if live.ID == own {
				t.Errorf("own claim %s was not cleaned up", own)
			}
		}
	}
}

func TestLeaseLookupFailureNeverStartsCommand(t *testing.T) {
	for _, action := range []string{"take", "run"} {
		for _, phase := range []string{"initial", "settlement"} {
			t.Run(action+"/"+phase, func(t *testing.T) {
				n := newLeaseFailureNode(t, func(n *leaseFailureNode) bool { return phase == "initial" || n.sends.Load() > 0 }, false)
				marker := filepath.Join(t.TempDir(), "ran")
				args := []string{"lease", action, leaseKey, "--settle", "0s"}
				if action == "run" {
					args = append(args, "--", "sh", "-c", `: > "$1"`, "lease-fixture", marker)
				}
				code, _, detail := run(t, "", args...)
				if code == 0 || exists(marker) {
					t.Fatalf("lookup failure allowed work: code=%d marker=%v %s", code, exists(marker), detail)
				}
				want := int32(0)
				if phase == "settlement" {
					want = 1
				}
				if n.sends.Load() != want {
					t.Fatalf("sent %d claims, want %d", n.sends.Load(), want)
				}
				n.assertOnlyOwnCleanup(t)
			})
		}
	}
}

func TestLeaseSettlementTimeoutNeverStartsCommand(t *testing.T) {
	n := newLeaseFailureNode(t, func(n *leaseFailureNode) bool { return n.sends.Load() > 0 }, true)
	marker := filepath.Join(t.TempDir(), "ran")
	err := leaseRun(n.client, nil, "fixture/runner", []string{leaseKey, "--settle", "0s", "--", "sh", "-c", `: > "$1"`, "lease-fixture", marker}, Streams{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard})
	if err == nil || exists(marker) || n.sends.Load() != 1 {
		t.Fatalf("timeout allowed work: %v marker=%v sends=%d", err, exists(marker), n.sends.Load())
	}
	n.assertOnlyOwnCleanup(t)
}

func TestLeaseRenewalFailureStopsCommandAndDescendants(t *testing.T) {
	for _, mode := range []string{"503", "timeout", "lost", "empty", "stale"} {
		t.Run(mode, func(t *testing.T) {
			n := newLeaseFailureNode(t, func(n *leaseFailureNode) bool { return n.fail.Load() }, mode == "timeout")
			old := leaseRenewEvery
			leaseRenewEvery = func(time.Duration) time.Duration { return 40 * time.Millisecond }
			t.Cleanup(func() { leaseRenewEvery = old })
			dir := t.TempDir()
			started, heartbeat := filepath.Join(dir, "started"), filepath.Join(dir, "heartbeat")
			done := make(chan error, 1)
			go func() {
				done <- leaseRun(n.client, nil, "fixture/runner", []string{leaseKey, "--settle", "0s", "--", "sh", "-c", `(while :; do echo x >> "$2"; sleep 0.02; done) & : > "$1"; wait`, "lease-fixture", started, heartbeat}, Streams{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard})
			}()
			waitFile(t, started)
			waitFile(t, heartbeat)
			switch mode {
			case "lost":
				n.lost.Store(true)
			case "empty":
				n.empty.Store(true)
			case "stale":
				n.stale.Store(true)
			default:
				n.fail.Store(true)
			}
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "stopped") {
					t.Fatalf("renewal did not fail closed: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("command kept running after ownership lookup failed")
			}
			before, err := os.ReadFile(heartbeat)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(120 * time.Millisecond)
			after, _ := os.ReadFile(heartbeat)
			if !bytes.Equal(before, after) {
				t.Fatal("descendant kept working after the lease was released")
			}
			n.assertOnlyOwnCleanup(t)
		})
	}
}

func TestLeaseRunReleasesOnlyItsOwnKnownClaim(t *testing.T) {
	n := newLeaseFailureNode(t, func(*leaseFailureNode) bool { return false }, false)
	dir := t.TempDir()
	started, stop := filepath.Join(dir, "started"), filepath.Join(dir, "stop")
	done := make(chan int, 1)
	go func() {
		code, _, _ := run(t, "", "lease", "run", leaseKey, "--settle", "0s", "--", "sh", "-c", `: > "$1"; while [ ! -e "$2" ]; do sleep 0.02; done`, "lease-fixture", started, stop)
		done <- code
	}()
	waitFile(t, started)
	other, err := n.store.Send(msg.Message{From: "fixture/runner", To: "fixture/runner", Kind: msg.KindLease, Body: msg.LeaseBody(leaseKey, "independent caller"), TTL: 600})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stop, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("healthy command exit=%d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command did not finish")
	}
	n.assertOnlyOwnCleanup(t)
	for _, l := range n.store.Leases() {
		if l.ID == other.ID {
			return
		}
	}
	t.Fatal("release withdrew another caller's same-identity claim")
}

func TestLeaseEmptySettlementNeverStartsCommand(t *testing.T) {
	n := newLeaseFailureNode(t, func(n *leaseFailureNode) bool {
		if n.sends.Load() > 0 {
			n.empty.Store(true)
		}
		return false
	}, false)
	marker := filepath.Join(t.TempDir(), "ran")
	code, _, detail := run(t, "", "lease", "run", leaseKey, "--settle", "0s", "--", "sh", "-c", `: > "$1"`, "lease-fixture", marker)
	if code == 0 || exists(marker) {
		t.Fatalf("empty read allowed work: code=%d %s", code, detail)
	}
	n.assertOnlyOwnCleanup(t)
}

func TestLeaseRenewalsNeverRetractTheOnlyLiveClaim(t *testing.T) {
	n := newLeaseFailureNode(t, func(*leaseFailureNode) bool { return false }, false)
	old := leaseRenewEvery
	leaseRenewEvery = func(time.Duration) time.Duration { return 40 * time.Millisecond }
	t.Cleanup(func() { leaseRenewEvery = old })
	dir := t.TempDir()
	started, stop := filepath.Join(dir, "started"), filepath.Join(dir, "stop")
	done := make(chan int, 1)
	go func() {
		code, _, _ := run(t, "", "lease", "run", leaseKey, "--note", "operator release", "--settle", "0s", "--", "sh", "-c", `: > "$1"; while [ ! -e "$2" ]; do sleep 0.02; done`, "fixture", started, stop)
		done <- code
	}()
	waitFile(t, started)
	n.watch.Store(true)
	deadline := time.Now().Add(3 * time.Second)
	for n.sends.Load() < 30 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	n.watch.Store(false)
	if err := os.WriteFile(stop, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("healthy renewals failed: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit")
	}
	if n.sends.Load() < 30 || n.gaps.Load() != 0 {
		t.Fatalf("renewal sends=%d gaps with no live claim=%d", n.sends.Load(), n.gaps.Load())
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	seen := map[string]bool{}
	for _, id := range n.created {
		if seen[id] {
			t.Fatalf("renewal was deduplicated to prior claim %s", id)
		}
		seen[id] = true
	}
	for i, m := range n.written {
		if m.Re != "" {
			t.Fatal("renewal used reply links that trip old-node pair brakes")
		}
		want := msg.LeaseBody(leaseKey, "operator release")
		if i > 0 {
			want = msg.LeaseBody(leaseKey, "operator release\nrenewal of "+n.written[i-1].ID)
		}
		if m.Body != want {
			t.Fatalf("renewal changed/accumulated operator note: %q", m.Body)
		}
	}
}

func TestLeaseRenewalDeadlineBoundsAllNodeIO(t *testing.T) {
	n := newLeaseFailureNode(t, func(*leaseFailureNode) bool { return true }, true)
	n.client.Timeout = 5 * time.Second
	end := time.Now().Add(120 * time.Millisecond)
	bounded := leaseClientUntil(n.client, end)
	if _, err := bounded.Leases(); err == nil {
		t.Fatal("blocked ownership read succeeded")
	}
	if time.Since(end) > 500*time.Millisecond {
		t.Fatal("read exceeded the ownership safety deadline")
	}
	n.client.Deadline = time.Now().Add(-time.Second)
	if got := leaseClientUntil(n.client, time.Now().Add(time.Hour)); !got.Deadline.Equal(n.client.Deadline) {
		t.Fatal("existing stricter deadline was widened")
	}
}

func TestLeaseRunReportsFailedFinalReleaseAndKeepsCommandExit(t *testing.T) {
	n := newLeaseFailureNode(t, func(*leaseFailureNode) bool { return false }, false)
	n.rejectRetract.Store(true)
	code, _, detail := run(t, "", "lease", "run", leaseKey, "--settle", "0s", "--", "sh", "-c", "exit 3")
	if code != 3 || !strings.Contains(detail, "not released") || !strings.Contains(detail, "ttl") {
		t.Fatalf("release failure hidden or child exit changed: code=%d %s", code, detail)
	}
	if len(n.store.Leases()) != 1 {
		t.Fatal("failed release was reported as successful")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.retracted) != 1 || len(n.created) != 1 || n.retracted[0] != n.created[0] {
		t.Fatal("release did not target only known claim")
	}
}

func TestLeaseRunRejectsNoteWithoutRenewalSpaceBeforeAnyRequest(t *testing.T) {
	n := newLeaseFailureNode(t, func(*leaseFailureNode) bool { return false }, false)
	code, _, detail := run(t, "", "lease", "run", leaseKey, "--note", strings.Repeat("x", msg.MaxBody), "--", "sh", "-c", "exit 0")
	if code != exitUsage || !strings.Contains(detail, "renewal provenance") || n.checks.Load() != 0 || n.sends.Load() != 0 {
		t.Fatalf("oversized note not refused before acquisition: %d %s", code, detail)
	}
}

func TestLeaseTakeAgainWritesDistinctExtendedClaim(t *testing.T) {
	n := newLeaseFailureNode(t, func(*leaseFailureNode) bool { return false }, false)
	for i := 0; i < 3; i++ {
		code, _, detail := run(t, "", "lease", "take", leaseKey, "--note", "operator release", "--settle", "0s")
		if code != 0 {
			t.Fatalf("take %d: %d %s", i, code, detail)
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.written) != 3 || n.written[0].ID == n.written[1].ID || n.written[1].ID == n.written[2].ID || !n.written[2].Exp.After(*n.written[1].Exp) {
		t.Fatal("taking the lease again did not extend it with a new claim")
	}
	if n.written[1].Body != msg.LeaseBody(leaseKey, "operator release\nrenewal of "+n.written[0].ID) {
		t.Fatal("manual renewal lost original note or bounded provenance")
	}
	if n.written[2].Body != msg.LeaseBody(leaseKey, "operator release\nrenewal of "+n.written[1].ID) {
		t.Fatal("manual renewal reused the aggregated earliest ID")
	}
}
