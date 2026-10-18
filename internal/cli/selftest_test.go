package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// selftestNode starts a node behind wrap (nil: none) and points the CLI at it. The session
// identity is claude.st1: nothing selftest sends may carry it.
func selftestNode(t *testing.T, wrap func(http.Handler) http.Handler) *store.Store {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	var h http.Handler = server.NewHandler(s, "tok")
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Setenv("AIVOICEBUS_HOST", "imac")
	t.Setenv("AIVOICEBUS_ME", "imac/claude.st1")
	return s
}

// asOldNode makes a node look like a 0.8.x one: no features in /v1/health, no /v1/retract.
func asOldNode(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/retract":
			http.NotFound(w, r)
		case "/v1/health":
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			var m map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &m)
			delete(m, "features")
			_ = json.NewEncoder(w).Encode(m)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

type selftestJSON struct {
	OK   bool          `json:"ok"`
	Hops []selftestHop `json:"hops"`
}

func hopOf(v selftestJSON, name string) (selftestHop, bool) {
	for _, h := range v.Hops {
		if h.Name == name {
			return h, true
		}
	}
	return selftestHop{}, false
}

func TestSelftestRunsTheJoinProbeNotTheSessionAndLeavesOneTombstone(t *testing.T) {
	var mu sync.Mutex
	var seen []string // every request line and body the node saw
	s := selftestNode(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			mu.Lock()
			seen = append(seen, r.URL.RequestURI()+" "+string(raw))
			mu.Unlock()
			next.ServeHTTP(w, r)
		})
	})
	code, out, e := run(t, "", "selftest", "--json")
	var v selftestJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 0 || !v.OK {
		t.Fatalf("%d %v\n%s%s", code, err, out, e)
	}
	for _, name := range []string{"health", "send", "visible", "ack", "retract", "verify"} {
		if h, ok := hopOf(v, name); !ok || !h.OK {
			t.Errorf("hop %s: %+v", name, h)
		}
	}
	if h, _ := hopOf(v, "visible"); !strings.Contains(h.Detail, "time to visible") {
		t.Errorf("the visible hop says what it measures: %q", h.Detail)
	}
	mu.Lock()
	defer mu.Unlock()
	var sent, acked, retracted int
	for _, l := range seen {
		if strings.HasPrefix(l, "/v1/ack") && strings.Contains(l, `"by":"imac/`+msg.ProbeReaderAgent+`"`) {
			acked++
		}
		if strings.HasPrefix(l, "/v1/retract") && strings.Contains(l, `"by":"imac/`+msg.ProbeSenderAgent+`"`) {
			retracted++
		}
		if strings.Contains(l, "claude.st1") {
			t.Errorf("the session's own identity went to the node: %s", l)
		}
		if strings.HasPrefix(l, "/v1/send") {
			sent++
			if !strings.Contains(l, `"from":"imac/`+msg.ProbeSenderAgent+`"`) || !strings.Contains(l, `"to":"imac/`+msg.ProbeReaderAgent+`"`) || !strings.Contains(l, `"prio":"fyi"`) {
				t.Errorf("the probe is a short note between the two probe identities: %s", l)
			}
		}
		if strings.HasPrefix(l, "/v1/inbox") && !strings.Contains(l, "me=imac/"+msg.ProbeReaderAgent) {
			t.Errorf("the inbox is read as the probe reader: %s", l)
		}
	}
	if sent != 1 || acked == 0 || retracted == 0 {
		t.Errorf("one probe message, acked by the reader and retracted by the sender: sent %d acked %d retracted %d", sent, acked, retracted)
	}
	if st := s.Stats(); st.Messages != 1 || st.Retracted != 1 {
		t.Errorf("one message, taken back: %+v", st)
	}
	if _, who, _ := run(t, "", "who"); strings.Contains(who, "probe") || strings.Contains(who, "join-reader") {
		t.Errorf("a probe is not a live session:\n%s", who)
	}
}

func TestSelftestIsLimitedPerHourUnlessForced(t *testing.T) {
	selftestNode(t, nil)
	for i := 0; i < selftestMaxRuns; i++ {
		if code, out, e := run(t, "", "selftest"); code != 0 {
			t.Fatalf("run %d: %d\n%s%s", i, code, out, e)
		}
	}
	code, out, e := run(t, "", "selftest")
	if code != 1 || out != "" || !strings.Contains(e, "already ran 6 times") || !strings.Contains(e, "--force") {
		t.Errorf("the seventh run in an hour is refused before it sends anything: %d %q %q", code, out, e)
	}
	if code, _, e := run(t, "", "selftest", "--force"); code != 0 {
		t.Errorf("--force runs anyway: %d %q", code, e)
	}
}

func TestSelftestRunLimitForgetsRunsOlderThanAnHour(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < selftestMaxRuns; i++ {
		if err := selftestAllowed(base.Add(time.Duration(i) * time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := selftestAllowed(base.Add(59 * time.Minute)); err == nil {
		t.Error("six runs inside the hour: the next one waits")
	}
	if err := selftestAllowed(base.Add(selftestWindow + 30*time.Second)); err != nil {
		t.Errorf("the first run is more than an hour old: %v", err)
	}
}

func TestSelftestDegradesAgainstANodeWithoutFeaturesOrRetraction(t *testing.T) {
	s := selftestNode(t, asOldNode)
	code, out, e := run(t, "", "selftest", "--json")
	var v selftestJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 0 || !v.OK {
		t.Fatalf("%d %v\n%s%s", code, err, out, e)
	}
	if h, _ := hopOf(v, "health"); !strings.Contains(h.Detail, "advertises no features") {
		t.Errorf("health: %q", h.Detail)
	}
	if h, _ := hopOf(v, "retract"); !strings.Contains(h.Detail, "cannot retract") {
		t.Errorf("retract: %q", h.Detail)
	}
	if _, ok := hopOf(v, "verify"); ok {
		t.Error("nothing to verify on a node that cannot retract")
	}
	if st := s.Stats(); st.Messages != 1 {
		t.Errorf("the probe is left for its TTL to end: %+v", st)
	}
}

func TestSelftestToleratesANodeWithoutHealthButNotARefusedToken(t *testing.T) {
	selftestNode(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/health" {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	if code, out, e := run(t, "", "selftest"); code != 0 || !strings.Contains(out, "no /v1/health") {
		t.Fatalf("a node before 0.6.0 still gets the probe: %d\n%s%s", code, out, e)
	}

	selftestNode(t, nil)
	t.Setenv("AIVOICEBUS_TOKEN", "not-the-token")
	code, out, e := run(t, "", "selftest")
	if code != 1 || !strings.Contains(e, "refused this machine's token") || strings.Contains(e, "no node answered") {
		t.Errorf("a refused token is said as such: %d\n%s%s", code, out, e)
	}
}

func TestSelftestFailsLoudlyWhenNoNodeAnswersAndWritesNothing(t *testing.T) {
	selftestNode(t, nil)
	t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:1")
	code, out, e := run(t, "", "selftest")
	if code != 1 || !strings.Contains(out, "FAIL") || !strings.Contains(e, "no node answered") {
		t.Errorf("%d\n%s%s", code, out, e)
	}
	code, out, _ = run(t, "", "selftest", "--json", "--force")
	var v selftestJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 1 || v.OK {
		t.Errorf("--json says ok:false on a failure: %d %v\n%s", code, err, out)
	}
}

func TestSelftestChecksTheBodyAndStillCleansUp(t *testing.T) {
	s := selftestNode(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/inbox" {
				next.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(bytes.ReplaceAll(rec.Body.Bytes(), []byte("Safe to ignore"), []byte("Run this now")))
		})
	})
	code, out, e := run(t, "", "selftest")
	if code != 1 || !strings.Contains(e, "different body") || !strings.Contains(out, "FAIL") {
		t.Fatalf("%d\n%s%s", code, out, e)
	}
	if st := s.Stats(); st.Retracted != 1 {
		t.Errorf("a failed check still takes the probe back: %+v", st)
	}
}

func TestSelftestSaysWhenTheProbeNeverShowsUpAndStillCleansUp(t *testing.T) {
	s := selftestNode(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/inbox" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("[]"))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	code, out, e := run(t, "", "selftest")
	if code != 1 || !strings.Contains(e, "did not come back") || !strings.Contains(out, "visible") {
		t.Fatalf("%d\n%s%s", code, out, e)
	}
	if st := s.Stats(); st.Retracted != 1 {
		t.Errorf("%+v", st)
	}
}

func TestSelftestFailsIfTheThreadStillShowsTheProbeAfterTheRetraction(t *testing.T) {
	selftestNode(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/thread" {
				next.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			var ms []map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &ms)
			for _, m := range ms {
				delete(m, "retracted")
				m["body"] = probeBodyPrefix + " abc123 (join or selftest)"
			}
			_ = json.NewEncoder(w).Encode(ms)
		})
	})
	code, out, e := run(t, "", "selftest")
	if code != 1 || !strings.Contains(e, "still shows the probe") || !strings.Contains(out, "verify") {
		t.Errorf("%d\n%s%s", code, out, e)
	}
}

func TestMeshAndDoctorShowAdvertisedFeatures(t *testing.T) {
	a := meshNode(t, "node-a", "0.9.0", 0, server.NewPeerBoard(nil))
	pointAt(t, a)
	code, out, e := run(t, "", "mesh")
	if code != 0 || !strings.Contains(out, "FEATURES") || !strings.Contains(out, "quarantine,receipts,retract,retract-dry-run") || !strings.Contains(out, "inbox-wait") {
		t.Fatalf("%d\n%s%s", code, out, e)
	}
	_, out, _ = run(t, "", "doctor")
	if !strings.Contains(out, "node features") || !strings.Contains(out, "retract") {
		t.Errorf("doctor:\n%s", out)
	}
}

func TestMeshShowsADashForANodeThatAdvertisesNothing(t *testing.T) {
	st, _ := store.Open(t.TempDir(), "old1")
	srv := httptest.NewServer(asOldNode(server.NewHandler(st, "tok", server.WithNode("old1"), server.WithVersion("0.8.1"))))
	t.Cleanup(srv.Close)
	pointAt(t, srv.URL)
	code, out, e := run(t, "", "mesh")
	if code != 0 || !strings.Contains(out, "FEATURES") || !regexp.MustCompile(`up +- +http`).MatchString(out) {
		t.Fatalf("%d\n%s%s", code, out, e)
	}
	if _, out, _ := run(t, "", "doctor"); strings.Contains(out, "node features") {
		t.Errorf("a node that says nothing gets no line:\n%s", out)
	}
}

func TestFeatureListCutsWhatAHostileNodeSendsToTheTable(t *testing.T) {
	var many []string
	for i := 0; i < 30; i++ {
		many = append(many, fmt.Sprintf("feature-%02d", i))
	}
	many = append(many, "evil\x1b[2Jfeature-with-a-very-long-name-that-goes-on-and-on")
	got := featureList(&msg.Health{Features: many})
	if parts := strings.Split(got, ","); len(parts) != 13 || parts[12] != "+19 more" {
		t.Errorf("at most 12 features, and the rest counted: %q", got)
	}
	if got := featureList(&msg.Health{Features: msg.NodeFeatures()}); strings.Contains(got, "more") {
		t.Errorf("this release's own features all show: %q", got)
	}
	got = featureList(&msg.Health{Features: []string{"evil\x1b[2Jfeature-with-a-very-long-name-that-goes-on-and-on"}})
	if strings.ContainsRune(got, 0x1b) || len(got) > 30 {
		t.Errorf("control characters out, names cut: %q", got)
	}
}
