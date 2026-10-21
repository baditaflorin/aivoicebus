package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
	"github.com/baditaflorin/aivoicebus/internal/worker"
)

// withoutLane makes a node look like one before the test lane: /v1/health lists no testlane (the node keeps
// working as it does; a client only decides by what it advertises).
func withoutLane(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		var h msg.Health
		_ = json.Unmarshal(rec.Body.Bytes(), &h)
		var kept []string
		for _, f := range h.Features {
			if f != msg.FeatureTestLane {
				kept = append(kept, f)
			}
		}
		h.Features = kept
		_ = json.NewEncoder(w).Encode(h)
	})
}

// selftestNodeOld is selftestNode behind a node that does not advertise the test lane, with wrap (nil: none) outside it.
func selftestNodeOld(t *testing.T, wrap func(http.Handler) http.Handler) *store.Store {
	t.Helper()
	return selftestNode(t, func(next http.Handler) http.Handler {
		h := withoutLane(next)
		if wrap != nil {
			h = wrap(h)
		}
		return h
	})
}

// recorder wraps a node so a test can read every request line and body it saw.
func recorder() (func(http.Handler) http.Handler, func() []string) {
	var mu sync.Mutex
	var seen []string
	return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(raw))
				mu.Lock()
				seen = append(seen, r.URL.RequestURI()+" "+string(raw))
				mu.Unlock()
				next.ServeHTTP(w, r)
			})
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), seen...)
		}
}

func TestSelftestUsesTheTestLaneWhereTheNodesHaveItAndLeavesNothingBehind(t *testing.T) {
	wrap, seen := recorder()
	s := selftestNode(t, wrap)
	code, out, e := run(t, "", "selftest", "--json")
	var v selftestJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 0 || !v.OK {
		t.Fatalf("%d %v\n%s%s", code, err, out, e)
	}
	for _, name := range []string{"health", "send", "visible", "ack", "hidden"} {
		if h, ok := hopOf(v, name); !ok || !h.OK {
			t.Errorf("hop %s: %+v", name, h)
		}
	}
	for _, name := range []string{"retract", "verify"} {
		if _, ok := hopOf(v, name); ok {
			t.Errorf("a test probe is purged by the node, so %s is not a step", name)
		}
	}
	if h, _ := hopOf(v, "hidden"); !strings.Contains(h.Detail, "does not show it") {
		t.Errorf("the hidden hop says what it proves: %q", h.Detail)
	}
	var sends, laneReads, plainReads int
	for _, l := range seen() {
		switch {
		case strings.HasPrefix(l, "/v1/retract"):
			t.Errorf("nothing is retracted: %s", l)
		case strings.Contains(l, "claude.st1"):
			t.Errorf("the session's own identity went to the node: %s", l)
		case strings.HasPrefix(l, "/v1/send"):
			sends++
			var m msg.Message
			_ = json.Unmarshal([]byte(l[strings.Index(l, " ")+1:]), &m)
			if !msg.IsTestID(m.ID) || m.TTL < 1 || m.TTL > 600 || m.From != "imac/"+msg.ProbeSenderAgent || m.To != "imac/"+msg.ProbeReaderAgent || m.Prio != msg.PrioFYI {
				t.Errorf("the probe is a short test note between the two probe identities: %s", l)
			}
		case strings.HasPrefix(l, "/v1/inbox") && strings.Contains(l, "lane=test"):
			laneReads++
			if !strings.Contains(l, "test%3Don") || !strings.Contains(l, "me=imac/"+msg.ProbeReaderAgent) {
				t.Errorf("the lane read is the reader's and declares the opt-in: %s", l)
			}
		case strings.HasPrefix(l, "/v1/inbox"):
			plainReads++
			if !strings.Contains(l, "all=1") {
				t.Errorf("the plain read lists everything, so a hidden message cannot be a read-state mistake: %s", l)
			}
		}
	}
	if sends != 1 || laneReads == 0 || plainReads != 1 {
		t.Errorf("one probe, read through the lane, then looked for with one plain read: sends %d lane reads %d plain reads %d", sends, laneReads, plainReads)
	}
	if st := s.Stats(); st.Messages != 0 || st.Acks != 0 || st.Retracted != 0 {
		t.Errorf("the lane is not in the statistics: %+v", st)
	}
	if got := s.Messages(); len(got) != 0 {
		t.Errorf("nor on the board: %v", ids(got))
	}
	if _, who, _ := run(t, "", "who"); strings.Contains(who, "probe") || strings.Contains(who, "join-reader") {
		t.Errorf("a probe is not a live session:\n%s", who)
	}
}

func ids(ms []msg.Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestSelftestFailsWhenANodeShowsTheLaneToAReadThatDidNotAskForIt(t *testing.T) {
	// a node that serves test messages to every read, as one that does not hide the lane would
	selftestNode(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (r.URL.Path == "/v1/inbox" || r.URL.Path == "/v1/thread") && !strings.Contains(r.URL.RawQuery, "lane=test") {
				r.URL.RawQuery += "&lane=test"
			}
			next.ServeHTTP(w, r)
		})
	})
	code, out, e := run(t, "", "selftest")
	if code != 1 || !strings.Contains(e, "does not hide the lane") || !strings.Contains(out, "hidden") || !strings.Contains(out, "FAIL") {
		t.Errorf("a node that does not hide the lane fails the selftest: %d\n%s%s", code, out, e)
	}
}

func TestSelftestFlagsGoTogetherOrNotAtAll(t *testing.T) {
	selftestNode(t, nil)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"--write needs --worker":         {[]string{"--write"}, "go with --worker"},
		"--brake needs --worker":         {[]string{"--brake", "5"}, "go with --worker"},
		"--brake is bounded":             {[]string{"--worker", "imac", "--brake", "2"}, "3 to 50"},
		"worker and file":                {[]string{"--worker", "imac", "--file", "1k", "--via", "node"}, "two scenarios"},
		"--file needs --via":             {[]string{"--file", "1k"}, "--via pipe or --via node"},
		"--via needs --file":             {[]string{"--via", "node"}, "goes with --file"},
		"--via is pipe or node":          {[]string{"--file", "1k", "--via", "carrier"}, "--via pipe or --via node"},
		"--file is a size":               {[]string{"--file", "huge", "--via", "node"}, "size from 1 byte"},
		"--file is not over the maximum": {[]string{"--file", "8m", "--via", "node"}, "size from 1 byte"},
		"a stray word":                   {[]string{"stray"}, "usage: selftest"},
	} {
		code, out, e := run(t, "", append([]string{"selftest", "--force"}, tc.args...)...)
		if code != 2 || !strings.Contains(e, tc.want) || out != "" {
			t.Errorf("%s: %d %q %q (want %q)", name, code, out, e, tc.want)
		}
	}
}

// ── the worker scenarios ─────────────────────────────────────────────────────

// testLog keeps what the worker says, and shows it only if the test failed: a goroutine of the worker may still
// say something as the test ends, which a t.Log from it would turn into a panic.
type testLog struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func newTestLog(t *testing.T) testLog {
	l := testLog{&sync.Mutex{}, &bytes.Buffer{}}
	t.Cleanup(func() { // runs after the worker is stopped (cleanups run last in, first out)
		l.mu.Lock()
		defer l.mu.Unlock()
		if t.Failed() {
			t.Log("what the worker said:\n" + l.buf.String())
		}
	})
	return l
}

func (l testLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(b)
}

// testWorker runs a worker for imac/worker against the node the CLI points at, until the test ends. optIn
// makes it read the lane and declare the opt-in. Its command, if it ever ran, would leave mark.
func testWorker(t *testing.T, optIn bool, allow string) (mark string) {
	t.Helper()
	mark = filepath.Join(t.TempDir(), "ran")
	t.Setenv("AIVOICEBUS_ME", "imac/worker") // the worker of this process: its claims and answers go out as the worker (the host is imac either way)
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	c.Title = "aivoicebus worker"
	if optIn {
		c.Labels = msg.Labels{msg.TestLabel: msg.TestOn, "tool": "worker"}
		c.Lane = true
	}
	al, _ := worker.ParseAllow(allow)
	w := &worker.Worker{C: c, Me: "imac/worker", Allow: al, Argv: []string{"sh", "-c", "touch " + mark}, Env: os.Environ(), Out: newTestLog(t),
		Limits: worker.Limits{Timeout: time.Minute, MaxOutput: 4000}, MaxAge: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Loop(ctx, time.Second) }()
	t.Cleanup(func() { cancel(); <-done })
	// let it declare itself, as a started worker does with its first read
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if roster, err := c.Roster(); err == nil {
			for _, e := range roster {
				if e.Me == "imac/worker" {
					return mark
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the worker never showed on the roster")
	return mark
}

func selftestRun(t *testing.T, args ...string) (selftestJSON, int, string) {
	t.Helper()
	code, out, e := run(t, "", append([]string{"selftest", "--json", "--force"}, args...)...)
	var v selftestJSON
	_ = json.Unmarshal([]byte(out), &v)
	return v, code, e
}

func TestSelftestWorkerAnswersATestTaskThroughTheRealGatesAndRunsNothing(t *testing.T) {
	wrap, seen := recorder()
	s := selftestNode(t, wrap)
	mark := testWorker(t, true, "imac\n")
	v, code, e := selftestRun(t, "--worker", "imac", "--write", "--timeout", "30s")
	if code != 0 || !v.OK {
		t.Fatalf("%d %s\n%+v", code, e, v)
	}
	for _, name := range []string{"health", "find", "send", "claim", "answer", "hidden", "status", "write"} {
		if h, ok := hopOf(v, name); !ok || !h.OK {
			t.Errorf("hop %s: %+v", name, h)
		}
	}
	if h, _ := hopOf(v, "answer"); !strings.Contains(h.Detail, "17*23 = 391") {
		t.Errorf("the worker computed it itself: %q", h.Detail)
	}
	if h, _ := hopOf(v, "write"); !strings.Contains(h.Detail, "write mode is off") {
		t.Errorf("this worker is read-only and says so: %q", h.Detail)
	}
	if h, _ := hopOf(v, "status"); !strings.Contains(h.Detail, "read-only") || !strings.Contains(h.Detail, "test lane on") {
		t.Errorf("status: %q", h.Detail)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("the worker ran its command for a test task: the lane must never run anything")
	}
	tasks := 0
	for _, l := range seen() {
		if strings.HasPrefix(l, "/v1/send") {
			var m msg.Message
			_ = json.Unmarshal([]byte(l[strings.Index(l, " ")+1:]), &m)
			if !msg.IsTestID(m.ID) {
				t.Errorf("every message of the scenario is a test message: %s", l)
			}
			if m.From != "imac/worker" && (m.From != "imac/"+msg.ProbeSenderAgent || m.Sig == "") {
				t.Errorf("sent as the probe identity, signed, or by the worker: %s", l)
			}
			if m.Kind == msg.KindTask {
				tasks++
				if m.From != "imac/"+msg.ProbeSenderAgent || m.Sig == "" {
					t.Errorf("a test task comes from the probe identity and is signed, so the worker's allowlist and signature checks are the real ones: %s", l)
				}
			}
		}
	}
	if tasks != 3 {
		t.Errorf("a calculation, the status and the write step: %d test tasks", tasks)
	}
	if st := s.Stats(); st.Messages != 0 || st.Acks != 0 {
		t.Errorf("the lane is not in the statistics: %+v", st)
	}
}

func TestSelftestWorkerSaysWhyWhenThereIsNoWorkerToTalkTo(t *testing.T) {
	selftestNode(t, nil)
	if _, code, e := selftestRun(t, "--worker", "imac"); code != 1 || !strings.Contains(e, "no worker seen on imac") {
		t.Errorf("no worker: %d %q", code, e)
	}
	// a worker that did not opt in
	testWorker(t, false, "imac\n")
	v, code, e := selftestRun(t, "--worker", "imac")
	if code != 1 || !strings.Contains(e, "has not opted in") || !strings.Contains(e, "--test-lane") {
		t.Errorf("not opted in: %d %q", code, e)
	}
	if h, _ := hopOf(v, "find"); h.OK {
		t.Errorf("find fails: %+v", h)
	}
}

func TestSelftestWorkerSaysWhyWhenTheWorkerDoesNotTakeTheTask(t *testing.T) {
	s := selftestNode(t, nil)
	mark := testWorker(t, true, "somewhere-else\n") // the allowlist does not name this machine
	v, code, e := selftestRun(t, "--worker", "imac", "--timeout", "3s")
	if code != 1 || !strings.Contains(e, "worker allow add imac") {
		t.Errorf("the answer names the likely cause: %d %q", code, e)
	}
	if h, _ := hopOf(v, "answer"); h.OK {
		t.Errorf("%+v", h)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("a worker that does not allow the sender ran a task")
	}
	if s.Stats().Messages != 0 {
		t.Errorf("%+v", s.Stats())
	}
}

func TestSelftestWorkerCancelStopsALongSleep(t *testing.T) {
	selftestNode(t, nil)
	testWorker(t, true, "imac\n")
	start := time.Now()
	v, code, e := selftestRun(t, "--worker", "imac", "--cancel", "--timeout", "30s")
	if code != 0 || !v.OK {
		t.Fatalf("%d %s\n%+v", code, e, v)
	}
	if h, ok := hopOf(v, "cancel"); !ok || !h.OK || !strings.Contains(h.Detail, "stopped the sleep") {
		t.Errorf("cancel: %+v", h)
	}
	if time.Since(start) > 60*time.Second {
		t.Errorf("a 120 s sleep was cancelled, but the scenario took %s", time.Since(start))
	}
}

func TestSelftestWorkerBrakeChecksTheTurnLimit(t *testing.T) {
	selftestNode(t, nil)
	testWorker(t, true, "imac\n")
	v, code, e := selftestRun(t, "--worker", "imac", "--brake", "5", "--timeout", "30s")
	if code != 0 || !v.OK {
		t.Fatalf("%d %s\n%+v", code, e, v)
	}
	if h, ok := hopOf(v, "brake"); !ok || !h.OK || !strings.Contains(h.Detail, "message 6 was refused (turn limit)") {
		t.Errorf("brake: %+v", h)
	}
}

func TestSelftestWorkerPauseHoldsATaskAndThePauseEnds(t *testing.T) {
	s := selftestNode(t, nil)
	testWorker(t, true, "imac\n")
	// the pause lasts a minute at least; the test lifts it early the way its sender could, by retracting it
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
			}
			for _, r := range s.Since(map[string]uint64{}, 1000) {
				if r.Msg != nil && strings.HasPrefix(r.Msg.Body, "[aivoicebus pause]") && r.Msg.IsTest() {
					time.Sleep(23 * time.Second) // long enough for the scenario's 20 s check that the task waited
					_, _ = s.Retract(r.Msg.ID, "imac/"+msg.ProbeSenderAgent, false)
					return
				}
			}
		}
	}()
	v, code, e := selftestRun(t, "--worker", "imac", "--pause", "--timeout", "40s")
	if code != 0 || !v.OK {
		t.Fatalf("%d %s\n%+v", code, e, v)
	}
	if h, ok := hopOf(v, "pause"); !ok || !h.OK || !strings.Contains(h.Detail, "not taken") {
		t.Errorf("pause: %+v", h)
	}
	if h, ok := hopOf(v, "resume"); !ok || !h.OK {
		t.Errorf("resume: %+v", h)
	}
}

func TestSelftestWorkerIsRefusedOnAMeshWithANodeBeforeTheLane(t *testing.T) {
	s := selftestNodeOld(t, nil)
	testWorker(t, true, "imac\n")
	_, code, e := selftestRun(t, "--worker", "imac")
	if code != 1 || !strings.Contains(e, "predates the test lane") {
		t.Errorf("%d %q", code, e)
	}
	if s.Stats().Messages != 0 {
		t.Errorf("nothing was sent: %+v", s.Stats())
	}
}

// ── the file scenario ────────────────────────────────────────────────────────

func TestSelftestFileSendsDeterministicBytesThroughTheNodeAndChecksTheirChecksum(t *testing.T) {
	s := selftestNode(t, nil)
	v, code, e := selftestRun(t, "--file", "300k", "--via", "node", "--timeout", "20s")
	if code != 0 || !v.OK {
		t.Fatalf("%d %s\n%+v", code, e, v)
	}
	for _, name := range []string{"health", "put", "send", "visible", "get", "hidden"} {
		if h, ok := hopOf(v, name); !ok || !h.OK {
			t.Errorf("hop %s: %+v", name, h)
		}
	}
	if h, _ := hopOf(v, "put"); !strings.Contains(h.Detail, "300.0 KB") || !strings.Contains(h.Detail, "sha256") {
		t.Errorf("put names the size and the checksum: %q", h.Detail)
	}
	if h, _ := hopOf(v, "get"); !strings.Contains(h.Detail, "matches") {
		t.Errorf("get compares the checksum: %q", h.Detail)
	}
	if st := s.Stats(); st.Messages != 0 {
		t.Errorf("the carrier message is in the lane: %+v", st)
	}
}

func TestSelftestFileViaThePipeIsRefusedWhenThePipeIsOff(t *testing.T) {
	selftestNode(t, nil)
	t.Setenv("AIVOICEBUS_PIPE_URL", "off")
	v, code, e := selftestRun(t, "--file", "10k", "--via", "pipe")
	if code != 1 || !strings.Contains(e, "pipe is turned off") {
		t.Errorf("%d %q", code, e)
	}
	if h, _ := hopOf(v, "put"); h.OK {
		t.Errorf("%+v", h)
	}
}

func TestTestBytesAreTheSameEveryTimeAndOfTheSizeAsked(t *testing.T) {
	a, b := testBytes(300000), testBytes(300000)
	if len(a) != 300000 || !bytes.Equal(a, b) || bytes.Equal(testBytes(32)[:16], testBytes(32)[16:]) {
		t.Error("deterministic, of the size asked, and not a repeating block")
	}
	if n, err := parseSize("300k"); err != nil || n != 300*1024 {
		t.Errorf("300k: %d %v", n, err)
	}
	if n, err := parseSize("1m"); err != nil || n != 1<<20 {
		t.Errorf("1m: %d %v", n, err)
	}
	if _, err := parseSize("0"); err == nil {
		t.Error("zero is not a size")
	}
}
