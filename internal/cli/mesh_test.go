package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// meshNode starts a node that reports the given release and shifts its clock by skew.
func meshNode(t *testing.T, name, ver string, skew time.Duration, peers *server.PeerBoard) string {
	t.Helper()
	s, _ := store.Open(t.TempDir(), name)
	inner := server.NewHandler(s, "tok", server.WithNode(name), server.WithVersion(ver), server.WithPeerBoard(peers))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" || skew == 0 {
			inner.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		var h msg.Health
		_ = json.Unmarshal(rec.Body.Bytes(), &h)
		h.Now = h.Now.Add(skew)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func pointAt(t *testing.T, urls ...string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", strings.Join(urls, ","))
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
}

func TestMeshWarnsWhenThePublishedNodeListIsAboutToExpireOrHas(t *testing.T) {
	dir := t.TempDir()
	priv, _, err := release.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	s, _ := store.Open(t.TempDir(), "n")
	srv := httptest.NewServer(server.NewHandler(s, "tok", server.WithReleaseDir(dir)))
	defer srv.Close()
	c := &client.Client{URLs: []string{srv.URL}, Token: "tok"}
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	entries := []release.NodeEntry{{Name: "a", URL: "http://10.0.0.1:7777"}}
	pack := func(signedAt time.Time, validFor time.Duration) {
		if _, err := release.PackNodes(release.PackNodesOptions{Dir: dir, Serial: 1, Nodes: entries, Key: priv, Now: signedAt, ValidFor: validFor}); err != nil {
			t.Fatal(err)
		}
	}
	if note := nodeListNote(c, now); note != "" {
		t.Errorf("no list published is not a problem: %q", note)
	}
	pack(now, 90*24*time.Hour)
	if note := nodeListNote(c, now); note != "" {
		t.Errorf("a fresh list: %q", note)
	}
	pack(now.Add(-80*24*time.Hour), 90*24*time.Hour)
	if note := nodeListNote(c, now); !strings.Contains(note, "expires on 2026-10-24") {
		t.Errorf("ten days left: %q", note)
	}
	pack(now.Add(-100*24*time.Hour), 90*24*time.Hour)
	if note := nodeListNote(c, now); !strings.Contains(note, "expired on") || !strings.Contains(note, "no longer learn nodes") {
		t.Errorf("expired: %q", note)
	}
	pack(now.Add(-1000*24*time.Hour), -1)
	if note := nodeListNote(c, now); note != "" {
		t.Errorf("a list with no expiry never raises one: %q", note)
	}
}

func TestMeshSaysHealthyWhenEveryNodeAgrees(t *testing.T) {
	a := meshNode(t, "node-a", "0.6.0", 0, server.NewPeerBoard(nil))
	b := meshNode(t, "build", "0.6.0", 0, server.NewPeerBoard(nil))
	pointAt(t, a, b)
	code, out, e := run(t, "", "mesh")
	if code != 0 || !strings.Contains(out, "mesh healthy: 2 node(s)") || !strings.Contains(out, "node-a") || !strings.Contains(out, "0.6.0") {
		t.Fatalf("%d\n%s%s", code, out, e)
	}
}

func TestMeshReportsEveryKindOfProblemAndExitsNonZero(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	board := server.NewPeerBoard([]string{"http://peer-a:7777", "http://peer-b:7777"})
	board.Update("http://peer-a:7777", func(p *msg.PeerHealth) { p.OK, p.Lag = true, 120 })
	board.Update("http://peer-b:7777", func(p *msg.PeerHealth) { p.OK, p.Error = false, "connection refused" })
	a := meshNode(t, "node-a", "0.6.0", 0, board)
	b := meshNode(t, "build", "0.5.0", 0, server.NewPeerBoard(nil))
	c := meshNode(t, "node-b", "0.6.0", 0, server.NewPeerBoard(nil))
	d := meshNode(t, "node-c", "0.6.0", 30*time.Second, server.NewPeerBoard(nil)) // a clock 30s fast
	pointAt(t, a, b, c, d, downURL)

	code, out, _ := run(t, "", "mesh")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	for _, want := range []string{
		"node-c's clock is",
		"is unreachable",
		"is 120 records behind its peer http://peer-a:7777",
		"cannot reach its peer http://peer-b:7777: connection refused",
		"the nodes run different releases: 0.5.0 on build; 0.6.0 on node-a, node-b, node-c",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "node-a's clock") || strings.Contains(out, "build's clock") {
		t.Errorf("one wrong clock must not be blamed on the others:\n%s", out)
	}
}

func TestMeshJSONAndThresholds(t *testing.T) {
	a := meshNode(t, "node-a", "0.6.0", 0, server.NewPeerBoard(nil))
	d := meshNode(t, "build", "0.6.0", 5*time.Second, server.NewPeerBoard(nil))
	pointAt(t, a, d)
	code, out, _ := run(t, "", "mesh", "--json", "--max-skew", "10s")
	var v meshView
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 0 || len(v.Nodes) != 2 || len(v.Problems) != 0 || v.Nodes[0].Health == nil {
		t.Fatalf("a 5s skew under a 10s threshold is healthy: %d %v\n%s", code, err, out)
	}
	if code, out, _ := run(t, "", "mesh", "--json", "--max-skew", "1s"); code != 1 || !strings.Contains(out, "clock is") {
		t.Errorf("a 5s skew over a 1s threshold is a problem: %d\n%s", code, out)
	}
}

func TestAssessRelativeSkewAndGaps(t *testing.T) {
	mk := func(name string, skew int64, records int64) nodeReport {
		return nodeReport{Name: name, SkewMs: skew, Records: records, Health: &msg.Health{Node: name, Version: "1"}}
	}
	// the local clock is 100s off, but the nodes agree with each other
	got := assess([]nodeReport{mk("a", 100000, 500), mk("b", 100200, 500), mk("c", 99900, 500)}, meshThresholds{MaxSkew: 2 * time.Second, MaxLag: 50, MaxGap: 50})
	if len(got) != 0 {
		t.Errorf("skew is judged between the nodes, not against this machine: %v", got)
	}
	got = assess([]nodeReport{mk("a", 0, 500), mk("b", 0, 400)}, meshThresholds{MaxSkew: time.Second, MaxLag: 50, MaxGap: 50})
	if len(got) != 1 || !strings.Contains(got[0], "b holds 100 fewer records") {
		t.Errorf("%v", got)
	}
}

func TestMeshWarnsBeforeANodeOutgrowsItsMemory(t *testing.T) {
	mk := func(name string, records int64) nodeReport {
		return nodeReport{Name: name, Records: records, Health: &msg.Health{Version: "0.6.0", Log: map[string]uint64{name: uint64(records)}}}
	}
	th := meshThresholds{MaxSkew: time.Hour, MaxLag: 50, MaxGap: 1 << 40, MaxRecords: 1000}
	if got := assess([]nodeReport{mk("a", 900), mk("b", 900)}, th); len(got) != 0 {
		t.Errorf("below the limit: %v", got)
	}
	got := assess([]nodeReport{mk("a", 1500), mk("b", 900)}, th)
	if len(got) != 1 || !strings.Contains(got[0], "a holds 1500 records, more than 1000") || !strings.Contains(got[0], "docs/capacity.md") {
		t.Errorf("above the limit: %v", got)
	}
	th.MaxRecords = 0
	if got := assess([]nodeReport{mk("a", 1500)}, th); len(got) != 0 {
		t.Errorf("0 turns the alarm off: %v", got)
	}
}

func TestMeshNamesANodeThatIsLeaving(t *testing.T) {
	a := meshNode(t, "node-a", "0.6.0", 0, server.NewPeerBoard(nil))
	s, _ := store.Open(t.TempDir(), "build")
	srv := httptest.NewServer(server.NewHandler(s, "tok", server.WithNode("build"), server.WithVersion("0.6.0"), server.WithLeaving(true)))
	t.Cleanup(srv.Close)
	pointAt(t, a, srv.URL)
	code, out, e := run(t, "", "mesh")
	if code != 1 || !strings.Contains(out, "build is leaving the mesh: it takes no client writes") {
		t.Fatalf("%d\n%s%s", code, out, e)
	}
}
