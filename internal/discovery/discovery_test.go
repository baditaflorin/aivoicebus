package discovery

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// testMesh is n nodes that serve whatever signed node list is in dir.
type testMesh struct {
	t     *testing.T
	urls  []string
	dir   string
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
	fetch atomic.Int64 // requests for nodes.json
}

func newTestMesh(t *testing.T, n int) *testMesh {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	m := &testMesh{t: t, dir: t.TempDir(), priv: priv, pub: pub}
	handlers := make([]http.Handler, n)
	for i := 0; i < n; i++ {
		i := i
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, release.NodesName) {
				m.fetch.Add(1)
			}
			handlers[i].ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		m.urls = append(m.urls, srv.URL)
	}
	for i := range handlers {
		s, _ := store.Open(t.TempDir(), "n")
		handlers[i] = server.NewHandler(s, "tok", server.WithReleaseDir(m.dir))
	}
	t.Setenv("HOME", t.TempDir())
	return m
}

func (m *testMesh) publish(serial int64, which ...int) {
	m.t.Helper()
	var entries []release.NodeEntry
	for _, i := range which {
		entries = append(entries, release.NodeEntry{Name: "n" + string(rune('0'+i)), URL: m.urls[i]})
	}
	if _, err := release.PackNodes(release.PackNodesOptions{Dir: m.dir, Serial: serial, Nodes: entries, Key: m.priv}); err != nil {
		m.t.Fatal(err)
	}
}

func (m *testMesh) client(urls ...string) *client.Client {
	return &client.Client{URLs: urls, Token: "tok", Timeout: 2 * time.Second}
}

func (m *testMesh) publishFor(serial int64, validFor time.Duration, now time.Time, which ...int) {
	m.t.Helper()
	var entries []release.NodeEntry
	for _, i := range which {
		entries = append(entries, release.NodeEntry{Name: "n" + string(rune('0'+i)), URL: m.urls[i]})
	}
	if _, err := release.PackNodes(release.PackNodesOptions{Dir: m.dir, Serial: serial, Nodes: entries, Key: m.priv, Now: now, ValidFor: validFor}); err != nil {
		m.t.Fatal(err)
	}
}

func TestAnExpiredListIsRefusedAndAStaleSavedOneStopsBeingUsed(t *testing.T) {
	m := newTestMesh(t, 2)
	t.Setenv("AIVOICEBUS_URL", m.urls[0])
	keys := []ed25519.PublicKey{m.pub}
	now := time.Now()

	// a genuinely signed list that has run out is not accepted, even on a machine with none saved
	m.publishFor(5, time.Hour, now.Add(-48*time.Hour), 0, 1)
	if _, err := Refresh(m.client(m.urls[0]), keys, now); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("a replayed old list must be refused: %v", err)
	}
	if got := config.URLs(); len(got) != 1 {
		t.Errorf("nothing was learned: %v", got)
	}

	// a list that is valid is learned, and stops being used once it runs out
	m.publishFor(6, 30*24*time.Hour, now, 0, 1)
	if _, err := Refresh(m.client(m.urls[0]), keys, now); err != nil {
		t.Fatal(err)
	}
	if got := config.URLs(); len(got) != 2 {
		t.Fatalf("learned: %v", got)
	}
	if d := config.LoadDiscovered(); d.ValidUntil.IsZero() || d.Expired(now) || !d.Expired(now.Add(31*24*time.Hour)) {
		t.Errorf("the saved list remembers its signer's expiry: %+v", d)
	}
	if Stale(now.Add(time.Hour)) {
		t.Error("fresh and valid")
	}
	d := config.LoadDiscovered()
	d.ValidUntil = now.Add(-time.Minute) // the signer's time has run out
	config.SaveDiscovered(d)
	if got := config.URLs(); len(got) != 1 {
		t.Errorf("an expired saved list must not keep adding addresses the token is sent to: %v", got)
	}
	if !Stale(now.Add(7 * time.Hour)) {
		t.Error("an expired saved list is due for a refresh regardless of its age")
	}

	// the same serial renews the fetch time but never extends the signer's expiry
	m.publishFor(6, 30*24*time.Hour, now, 0, 1)
	if _, err := Refresh(m.client(m.urls[0]), keys, now.Add(8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if d := config.LoadDiscovered(); !d.ValidUntil.Equal(now.Add(30 * 24 * time.Hour).Truncate(time.Second)) {
		t.Errorf("expiry %v", d.ValidUntil)
	}
}

func TestAMachineConfiguredWithOneNodeLearnsTheOthers(t *testing.T) {
	m := newTestMesh(t, 3)
	m.publish(10, 0, 1, 2)
	t.Setenv("AIVOICEBUS_URL", m.urls[0])
	if got := config.URLs(); len(got) != 1 {
		t.Fatalf("before discovery: %v", got)
	}
	if !Stale(time.Now()) {
		t.Fatal("a machine that never learned the list is stale")
	}
	res, err := Refresh(m.client(m.urls[0]), []ed25519.PublicKey{m.pub}, time.Now())
	if err != nil || !res.Changed || res.Serial != 10 || len(res.Added) != 3 {
		t.Fatalf("%v %+v", err, res)
	}
	if got := config.URLs(); !slices.Equal(got, m.urls) {
		t.Errorf("the configured node first, then the learned ones: %v, want %v", got, m.urls)
	}
	if Stale(time.Now()) {
		t.Error("just refreshed")
	}
	if !Stale(time.Now().Add(25 * time.Hour)) {
		t.Error("a day later it is due again")
	}
}

func TestALearnedListNeverReachesAMachineConfiguredForAnotherMesh(t *testing.T) {
	m := newTestMesh(t, 2)
	m.publish(10, 0, 1)
	t.Setenv("AIVOICEBUS_URL", m.urls[0])
	if _, err := Refresh(m.client(m.urls[0]), []ed25519.PublicKey{m.pub}, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:1") // a dev node, not in the list
	if got := config.URLs(); len(got) != 1 || got[0] != "http://127.0.0.1:1" {
		t.Errorf("a client pointed at another mesh must not get this mesh's nodes appended: %v", got)
	}
}

func TestRefusesAListThatDoesNotVerifyIsOlderOrMissing(t *testing.T) {
	m := newTestMesh(t, 2)
	m.publish(10, 0, 1)
	t.Setenv("AIVOICEBUS_URL", m.urls[0])
	keys := []ed25519.PublicKey{m.pub}
	if _, err := Refresh(m.client(m.urls[0]), keys, time.Now()); err != nil {
		t.Fatal(err)
	}
	saved := config.LoadDiscovered()

	// an older signed list, replayed by a node: refused, nothing changes
	m.publish(9, 0)
	if _, err := Refresh(m.client(m.urls[0]), keys, time.Now()); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Errorf("an older serial: %v", err)
	}
	// a list signed by someone else
	_, rogue, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := release.PackNodes(release.PackNodesOptions{Dir: m.dir, Serial: 99, Key: rogue, Nodes: []release.NodeEntry{{Name: "evil", URL: "http://203.0.113.9:7777"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(m.client(m.urls[0]), keys, time.Now()); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("a rogue signature: %v", err)
	}
	got := config.LoadDiscovered()
	if got.Serial != saved.Serial || !slices.Equal(got.Nodes, saved.Nodes) {
		t.Errorf("nothing that failed may change the saved list: %+v vs %+v", got, saved)
	}
	for _, u := range config.URLs() {
		if strings.Contains(u, "203.0.113.9") {
			t.Error("the token must never be sent to an address that was not signed")
		}
	}
	// the same list again only renews the fetch time
	m.publish(10, 0, 1)
	before := config.LoadDiscovered().FetchedAt
	res, err := Refresh(m.client(m.urls[0]), keys, time.Now().Add(time.Hour))
	if err != nil || res.Changed || !config.LoadDiscovered().FetchedAt.After(before) {
		t.Errorf("the same serial: %v %+v", err, res)
	}
	// a mesh with no list at all
	m2 := newTestMesh(t, 1)
	if _, err := Refresh(m2.client(m2.urls[0]), []ed25519.PublicKey{m2.pub}, time.Now()); err == nil {
		t.Error("no list is an error, not a crash")
	}
}

func TestAFailedAttemptIsNotRepeatedAtEverySessionStart(t *testing.T) {
	m := newTestMesh(t, 1) // serves no list
	t.Setenv("AIVOICEBUS_URL", m.urls[0])
	now := time.Now()
	if !Stale(now) {
		t.Fatal("due")
	}
	Refresh(m.client(m.urls[0]), []ed25519.PublicKey{m.pub}, now)
	if Stale(now.Add(time.Hour)) {
		t.Error("a failure should wait before the next attempt")
	}
	if !Stale(now.Add(7 * time.Hour)) {
		t.Error("and then try again")
	}
}

func TestANodeRemovedFromTheListDropsOutButAConfiguredOneStays(t *testing.T) {
	m := newTestMesh(t, 3)
	keys := []ed25519.PublicKey{m.pub}
	t.Setenv("AIVOICEBUS_URL", m.urls[0]+","+m.urls[2])
	m.publish(10, 0, 1, 2)
	if _, err := Refresh(m.client(m.urls[0]), keys, time.Now()); err != nil {
		t.Fatal(err)
	}
	m.publish(11, 0, 1) // node 2 retired from the list
	res, err := Refresh(m.client(m.urls[0]), keys, time.Now())
	if err != nil || !res.Changed || !slices.Equal(res.Removed, []string{m.urls[2]}) {
		t.Fatalf("%v %+v", err, res)
	}
	if got := config.URLs(); !slices.Equal(got, []string{m.urls[0], m.urls[2], m.urls[1]}) {
		t.Errorf("what the user configured is never removed: %v", got)
	}
}

func TestADiscoveredNodeCarriesTheMachineWhenItsConfiguredNodeIsDown(t *testing.T) {
	m := newTestMesh(t, 2)
	m.publish(10, 0, 1)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	// the machine knows one live node and one that died; the list teaches it the rest
	t.Setenv("AIVOICEBUS_URL", m.urls[0])
	if _, err := Refresh(m.client(m.urls[0]), []ed25519.PublicKey{m.pub}, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIVOICEBUS_URL", deadURL+","+m.urls[0]) // its first node is gone
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	c, err := client.FromEnv()
	if err != nil || len(c.URLs) != 3 { // dead, the live one it kept, and the one it learned
		t.Fatalf("%v %v", err, c.URLs)
	}
	if _, err := c.Inbox("x/y", false); err != nil {
		t.Errorf("reads fail over to the nodes it learned: %v", err)
	}
}

func TestRefreshIfDueLearnsWhenStaleAndLeavesAFreshListAlone(t *testing.T) {
	m := newTestMesh(t, 2)
	m.publish(4, 0, 1)
	t.Setenv("AIVOICEBUS_URL", m.urls[0])
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	now := time.Now()
	RefreshIfDue([]ed25519.PublicKey{m.pub}, now, time.Second)
	if d := config.LoadDiscovered(); d.Serial != 4 || len(d.Nodes) != 2 {
		t.Fatalf("a machine that never learned the list learns it: %+v", d)
	}
	before := m.fetch.Load()
	RefreshIfDue([]ed25519.PublicKey{m.pub}, now.Add(time.Hour), time.Second)
	if m.fetch.Load() != before {
		t.Error("a fresh list must not be fetched again")
	}
	RefreshIfDue([]ed25519.PublicKey{m.pub}, now.Add(Every+time.Minute), time.Second)
	if m.fetch.Load() == before {
		t.Error("a day-old list is due for a refresh")
	}
}

// hungMesh is nodes that accept a request and never answer it (for a bounded time, so a test
// that fails does not hang): what a half-dead node does to a session start.
func hungMesh(t *testing.T, n int) []string {
	t.Helper()
	var urls []string
	for i := 0; i < n; i++ {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
		t.Cleanup(srv.Close)
		urls = append(urls, srv.URL)
	}
	return urls
}

func TestTheBudgetIsOneDeadlineForTheWholeRefreshNotPerRequest(t *testing.T) {
	urls := hungMesh(t, 4)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", strings.Join(urls, ","))
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	start := time.Now()
	RefreshIfDue(nil, time.Now(), 200*time.Millisecond) // four nodes x two files, each hanging
	if d := time.Since(start); d > 700*time.Millisecond {
		t.Errorf("the refresh must end at its budget across all nodes, took %s", d)
	}
}

func TestTheStartBudgetIsShortEnoughNotToBeNoticed(t *testing.T) {
	urls := hungMesh(t, 2)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", strings.Join(urls, ","))
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	start := time.Now()
	RefreshIfDue(nil, time.Now(), StartBudget)
	if d := time.Since(start); d > time.Second {
		t.Errorf("a session start with hung nodes took %s", d)
	}
}

func TestAnUnwritableConfigDirMeansNoAttemptRatherThanOneAtEveryStart(t *testing.T) {
	m := newTestMesh(t, 1)
	m.publish(4, 0)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, ".config")
	if err := os.WriteFile(cfg, []byte("x"), 0o600); err != nil { // a file where the directory must be
		t.Fatal(err)
	}
	t.Setenv("AIVOICEBUS_URL", m.urls[0])
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	RefreshIfDue([]ed25519.PublicKey{m.pub}, time.Now(), time.Second)
	if m.fetch.Load() != 0 {
		t.Errorf("an attempt that cannot be remembered must not be made: %d fetches", m.fetch.Load())
	}
}

func TestAClientDeadlineBoundsARequestThatTheTimeoutWouldAllow(t *testing.T) {
	urls := hungMesh(t, 1)
	c := &client.Client{URLs: urls, Token: "tok", Timeout: 5 * time.Second, Deadline: time.Now().Add(150 * time.Millisecond)}
	start := time.Now()
	if _, err := c.FetchRelease("nodes.json", 1<<10); err == nil {
		t.Fatal("a hung node cannot answer")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the deadline did not cut the request: %s", d)
	}
}
