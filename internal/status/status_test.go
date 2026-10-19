package status

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

const me = "here/claude.1"

// node starts a real node holding st, answering as name and running release v; latest, if set, is
// the release it announces on every answer.
func node(t *testing.T, name, v, latest string, opts ...server.Option) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir(), name)
	if err != nil {
		t.Fatal(err)
	}
	h := server.NewHandler(st, "tok", append([]server.Option{server.WithNode(name), server.WithVersion(v)}, opts...)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if latest != "" {
			w.Header().Set(msg.HeaderLatest, latest)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

// frozen is a node that takes the connection and never answers (a process under SIGSTOP, a hung
// disk), until the test ends.
func frozen(t *testing.T) *httptest.Server {
	t.Helper()
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	return srv
}

// fakeHealth is a node that answers only /v1/health, with h (its clock moved by skew).
func fakeHealth(t *testing.T, h msg.Health, skew time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		h.Now = time.Now().Add(skew)
		_ = json.NewEncoder(w).Encode(h)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func opts(urls ...string) Options {
	return Options{
		Client: &client.Client{URLs: urls, Token: "tok", NoSign: true},
		Me:     me,
		Why:    func() string { return "AIVOICEBUS_ME" },
		Here:   "0.10.4",
		Quiet:  func() config.Quiet { return config.Quiet{} },
		Held:   func() int { return 0 },
		Worker: func() *Worker { return &Worker{Service: "not installed"} },
	}
}

func fixFor(r Report, section string) []string {
	var out []string
	for _, p := range r.Problems {
		if p.Section == section {
			out = append(out, p.Fix)
		}
	}
	return out
}

func render(r Report) string {
	var b strings.Builder
	Render(&b, r)
	return b.String()
}

// Every section, gathered from a healthy two-node mesh that has something for each.
func TestEverySection(t *testing.T) {
	a, sa := node(t, "na", "0.10.4", "0.10.9")
	b, _ := node(t, "nb", "0.10.4", "")
	until := time.Now().Add(time.Hour)
	for _, m := range []msg.Message{
		{From: "far/codex.2", To: me, Body: "look at this", Prio: msg.PrioUrgent},
		{From: "far/codex.2", To: me, Body: "a question", Prio: msg.PrioAsk},
		{From: "far/codex.2", To: me, Body: "for your info", Prio: msg.PrioFYI},
		{From: "far/codex.2", To: "here", Body: "do this", Kind: msg.KindTask, Prio: msg.PrioAsk},
		{From: me, To: msg.HumanOf(me), Body: msg.AskBody("ok to deploy?", []string{"yes", "no"}, ""), Prio: msg.PrioUrgent},
		{From: "here/worker", To: msg.HumanOf(me), Body: "[approval needed] the worker would run x", Prio: msg.PrioUrgent},
		{From: "far/codex.2", To: "all", Body: msg.PauseBody(until, "drill"), Prio: msg.PrioUrgent},
	} {
		if _, err := sa.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	// A session on this machine in a wait, one on another machine too (not ours to list).
	c := &client.Client{URLs: []string{a.URL}, Token: "tok", NoSign: true}
	for _, who := range []string{"here/codex.w", "far/claude.w"} {
		if _, err := c.Inbox(who, false); err != nil {
			t.Fatal(err)
		}
		sa.NoteWaiting(who, time.Now().Add(time.Minute))
	}

	o := opts(a.URL, b.URL)
	o.Quiet = func() config.Quiet { return config.Quiet{From: 22 * 60, To: 7 * 60} }
	o.Held = func() int { return 2 }
	o.Worker = func() *Worker { return &Worker{Running: true, Service: "running", Detail: "pid 42"} }
	r := Gather(o)

	if len(r.Unknown) != 0 {
		t.Fatalf("unknown: %v", r.Unknown)
	}
	if r.Me.ID != me || r.Me.Why != "AIVOICEBUS_ME" {
		t.Errorf("me = %+v", r.Me)
	}
	if r.Token.Kind != "shared" || !slices.Contains(fixFor(r, "token"), "the operator: deploy/machine-token.sh add here --apply") {
		t.Errorf("token = %+v, fixes %v", r.Token, fixFor(r, "token"))
	}
	if r.Release.Here != "0.10.4" || r.Release.Latest != "0.10.9" || !r.Release.Update || r.Release.Nodes["0.10.4"] != 2 {
		t.Errorf("release = %+v", r.Release)
	}
	if !slices.Contains(fixFor(r, "release"), "aivoicebus update") {
		t.Errorf("no update fix: %v", r.Problems)
	}
	if n := r.Nodes; n.Configured != 2 || n.Up != 2 || len(n.Down) != 0 || n.List[0].Name != "na" || n.SpreadMs > 500 {
		t.Errorf("nodes = %+v", n)
	}
	// The pause is urgent mail too.
	if in := r.Inbox; in.Unread != 4 || in.Urgent != 2 || in.Ask != 1 || in.FYI != 1 {
		t.Errorf("inbox = %+v", in)
	}
	if r.Tasks.Waiting != 1 || !slices.Contains(fixFor(r, "tasks"), "aivoicebus queue") {
		t.Errorf("tasks = %+v", r.Tasks)
	}
	if len(r.Person.Questions) != 1 || len(r.Person.Approvals) != 1 {
		t.Errorf("person = %+v", r.Person)
	}
	if fs := fixFor(r, "person"); len(fs) != 2 || fs[0] != "the person, in a terminal: aivoicebus approve "+r.Person.Approvals[0] {
		t.Errorf("person fixes = %v", fs)
	}
	if r.Held.Count != 2 || !slices.Contains(fixFor(r, "held"), "the person, in a terminal: aivoicebus held") {
		t.Errorf("held = %+v", r.Held)
	}
	if p := r.Pause; !p.Active || p.By != "far/codex.2" || p.Reason != "drill" || !p.Until.Equal(until.Truncate(time.Second)) {
		t.Errorf("pause = %+v", p)
	}
	if fs := fixFor(r, "pause"); len(fs) != 1 || !strings.Contains(fs[0], "a person there, in a terminal: aivoicebus resume") {
		t.Errorf("pause fix = %v", fs)
	}
	if !r.Worker.Running {
		t.Errorf("worker = %+v", r.Worker)
	}
	if !slices.Equal(r.Standby.Listening, []string{"here/codex.w"}) {
		t.Errorf("standby = %+v", r.Standby)
	}
	if q := r.Quiet; q.Window != "22:00-07:00" {
		t.Errorf("quiet = %+v", q)
	}

	out := render(r)
	for _, want := range []string{"aivoicebus status (here/claude.1, ", "  me       here/claude.1 (AIVOICEBUS_ME)", "  worker   running (pid 42)",
		"  standby  1 listening here: here/codex.w", "  quiet    22:00-07:00 daily", "\"drill\""} {
		if !strings.Contains(out, want) {
			t.Errorf("screen lacks %q:\n%s", want, out)
		}
	}
	checkScreen(t, out)
}

// checkScreen holds every screen to its rules: short, and every problem line names its command.
func checkScreen(t *testing.T, out string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > 22 {
		t.Errorf("%d lines; the screen is meant to be short:\n%s", len(lines), out)
	}
	for _, l := range lines[1:] {
		if (strings.HasPrefix(l, "!") || strings.HasPrefix(l, "? ")) && !strings.Contains(l, " -> aivoicebus ") && !strings.Contains(l, " -> the ") && !strings.Contains(l, " -> a person ") && !strings.Contains(l, " -> sudo ") && !strings.Contains(l, "unknown: ") {
			t.Errorf("a problem line without its command: %q", l)
		}
	}
}

// A quiet mesh: nothing to act on, no line marked.
func TestNothingToDo(t *testing.T) {
	ts := server.NewTokenSet("tok")
	ts.Add("mtok", server.Principal{Name: "here", Host: "here"})
	a, _ := node(t, "na", "0.10.4", "0.10.4", server.WithTokenSet(ts))
	o := opts(a.URL)
	o.Client.Token = "mtok"
	r := Gather(o)
	if len(r.Problems) != 0 || len(r.Unknown) != 0 {
		t.Fatalf("problems %v, unknown %v", r.Problems, r.Unknown)
	}
	if r.Token.Kind != "machine" || r.Token.Host != "here" {
		t.Errorf("token = %+v", r.Token)
	}
	out := render(r)
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "!") || strings.HasPrefix(l, "?") {
			t.Errorf("marked line on a quiet mesh: %q", l)
		}
	}
	for _, want := range []string{"a machine token, bound to here", "0.10.4 here; nodes run 0.10.4 on 1; up to date", "1 of 1 up", "nothing unread", "pause    none", "quiet    off"} {
		if !strings.Contains(out, want) {
			t.Errorf("screen lacks %q:\n%s", want, out)
		}
	}
	checkScreen(t, out)
}

// The hedged-read check of the lab, for the whole screen: with the first node frozen, every part
// that needs one node still answers, well inside the budget.
func TestFrozenNodeCostsNoMoreThanTheBudget(t *testing.T) {
	f := frozen(t)
	good, _ := node(t, "good", "0.10.4", "")
	o := opts(f.URL, good.URL)
	o.Budget = 1500 * time.Millisecond
	t0 := time.Now()
	r := Gather(o)
	took := time.Since(t0)
	if took > 2*time.Second {
		t.Errorf("took %s with one node frozen; the budget is 1.5s", took)
	}
	if len(r.Unknown) != 0 {
		t.Errorf("unknown with one node of two up: %v", r.Unknown)
	}
	if n := r.Nodes; n == nil || n.Up != 1 || n.Configured != 2 || len(n.Down) != 1 || n.Down[0] != strings.TrimPrefix(f.URL, "http://") {
		t.Fatalf("nodes = %+v", r.Nodes)
	}
	if !slices.Contains(fixFor(r, "nodes"), "aivoicebus mesh") {
		t.Errorf("no mesh fix for the frozen node: %v", r.Problems)
	}
	checkScreen(t, render(r))
}

// With every node frozen nothing hangs: the parts that need a node are unknown, the rest is shown.
func TestEveryNodeFrozenIsUnknownNotAHang(t *testing.T) {
	o := opts(frozen(t).URL, frozen(t).URL)
	o.Budget = 600 * time.Millisecond
	t0 := time.Now()
	r := Gather(o)
	if took := time.Since(t0); took > 1100*time.Millisecond {
		t.Errorf("took %s; the budget is 600ms", took)
	}
	want := []string{"token", "inbox", "tasks", "person", "pause", "standby"}
	got := append([]string(nil), r.Unknown...)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("unknown = %v, want %v", r.Unknown, want)
	}
	if r.Quiet == nil || r.Worker == nil || r.Held == nil || r.Me.Why == "" || r.Release == nil {
		t.Errorf("a local part went missing: %+v", r)
	}
	if n := r.Nodes; n == nil || n.Up != 0 || len(n.Down) != 2 { // what did not answer in time is down
		t.Errorf("nodes = %+v", n)
	}
	if fs := fixFor(r, "unknown"); len(fs) != 1 || fs[0] != "aivoicebus doctor" {
		t.Errorf("unknown fix = %v", fs)
	}
	out := render(r)
	if !strings.Contains(out, "? token    unknown: no answer within 600ms") {
		t.Errorf("screen:\n%s", out)
	}
	checkScreen(t, out)
}

// A slow local probe (a home on a stalled disk) is bounded too.
func TestSlowLocalProbeIsBounded(t *testing.T) {
	a, _ := node(t, "na", "0.10.4", "")
	o := opts(a.URL)
	o.Budget = 400 * time.Millisecond
	block := make(chan struct{})
	defer close(block)
	o.Held = func() int { <-block; return 0 }
	t0 := time.Now()
	r := Gather(o)
	if took := time.Since(t0); took > 900*time.Millisecond {
		t.Errorf("took %s", took)
	}
	if !slices.Equal(r.Unknown, []string{"held"}) {
		t.Errorf("unknown = %v", r.Unknown)
	}
}

func TestNoNodeConfigured(t *testing.T) {
	o := opts()
	o.Client = nil
	r := Gather(o)
	if !r.NoNodes || !slices.Contains(fixFor(r, "nodes"), "aivoicebus join --url URL") {
		t.Errorf("no_nodes %v, problems %v", r.NoNodes, r.Problems)
	}
	if len(fixFor(r, "unknown")) != 0 {
		t.Errorf("the parts that need a node are not 'no answer': %v", r.Problems)
	}
	out := render(r)
	if !strings.Contains(out, "? inbox    unknown: no node is configured") {
		t.Errorf("screen:\n%s", out)
	}
	checkScreen(t, out)
}

func TestRefusedToken(t *testing.T) {
	a, _ := node(t, "na", "0.10.4", "")
	o := opts(a.URL)
	o.Client.Token = "wrong"
	r := Gather(o)
	if r.Token == nil || r.Token.Kind != "rejected" || !slices.Contains(fixFor(r, "token"), "aivoicebus doctor") {
		t.Errorf("token = %+v, problems %v", r.Token, r.Problems)
	}
	checkScreen(t, render(r))
}

// Clocks, links between nodes and mixed releases come from the nodes' own health.
func TestNodeHealthProblems(t *testing.T) {
	ok := fakeHealth(t, msg.Health{Node: "a", Version: "0.10.4", Peers: []msg.PeerHealth{{OK: true}, {OK: false}}}, 0)
	ok2 := fakeHealth(t, msg.Health{Node: "b", Version: "0.10.4"}, 0)
	late := fakeHealth(t, msg.Health{Node: "c", Version: "0.10.3"}, 5*time.Second)
	r := Gather(opts(ok.URL, ok2.URL, late.URL))
	n := r.Nodes
	if n.Up != 3 || n.Peers != 2 || n.PeersOK != 1 || n.SpreadMs < 4500 {
		t.Fatalf("nodes = %+v", n)
	}
	var texts []string
	for _, p := range r.Problems {
		texts = append(texts, p.Text)
	}
	all := strings.Join(texts, "\n")
	for _, want := range []string{"1 of 2 links between nodes are down", "a node's clock is 5.0s off the others", "the nodes run different releases: 0.10.3 on 1, 0.10.4 on 2"} {
		if !strings.Contains(all, want) {
			t.Errorf("problems lack %q:\n%s", want, all)
		}
	}
	// This machine against the nodes: all three ahead by 5s.
	r = Gather(opts(late.URL))
	if !strings.Contains(strings.Join(fixFor(r, "nodes"), ","), "sudo ") {
		t.Errorf("no clock fix: %v", r.Problems)
	}
	checkScreen(t, render(r))
}

// A pause from this machine is lifted here.
func TestOwnPause(t *testing.T) {
	a, sa := node(t, "na", "0.10.4", "")
	if _, err := sa.Send(msg.Message{From: "here/claude.9", To: "here", Body: msg.PauseBody(time.Now().Add(time.Hour), "stop"), Prio: msg.PrioUrgent}); err != nil {
		t.Fatal(err)
	}
	r := Gather(opts(a.URL))
	if fs := fixFor(r, "pause"); len(fs) != 1 || fs[0] != "a person here, in a terminal: aivoicebus resume" {
		t.Errorf("pause fix = %v", fs)
	}
	o := opts(a.URL)
	o.Worker = func() *Worker { return &Worker{Service: "stopped", Detail: "state waiting"} }
	r = Gather(o)
	if !slices.Contains(fixFor(r, "worker"), "aivoicebus worker status") {
		t.Errorf("stopped worker: %v", r.Problems)
	}
	checkScreen(t, render(r))
}

// The --json shape is what scripts read: every section by name, null when unknown.
func TestJSONShape(t *testing.T) {
	o := opts(frozen(t).URL)
	o.Budget = 300 * time.Millisecond
	b, err := json.Marshal(Gather(o))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := append(append([]string(nil), Sections...), "problems", "unknown", "no_nodes", "took_ms", "budget_ms")
	sort.Strings(want)
	if !slices.Equal(keys, want) {
		t.Errorf("keys = %v\nwant %v", keys, want)
	}
	if string(m["inbox"]) != "null" || string(m["quiet"]) == "null" {
		t.Errorf("inbox %s, quiet %s", m["inbox"], m["quiet"])
	}
	var ps []Problem
	if err := json.Unmarshal(m["problems"], &ps); err != nil || len(ps) == 0 || ps[len(ps)-1].Fix == "" {
		t.Errorf("problems = %s (%v)", m["problems"], err)
	}

	a, _ := node(t, "na", "0.10.4", "")
	b, _ = json.Marshal(Gather(opts(a.URL)))
	var full struct {
		Token   map[string]any   `json:"token"`
		Nodes   map[string]any   `json:"nodes"`
		Inbox   map[string]int   `json:"inbox"`
		Tasks   map[string]any   `json:"tasks"`
		Person  map[string]any   `json:"person"`
		Release map[string]any   `json:"release"`
		Pause   map[string]any   `json:"pause"`
		Unknown []string         `json:"unknown"`
		Prob    []map[string]any `json:"problems"`
	}
	if err := json.Unmarshal(b, &full); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]map[string]any{"token": full.Token, "nodes": full.Nodes, "tasks": full.Tasks, "person": full.Person, "release": full.Release, "pause": full.Pause} {
		if v == nil {
			t.Errorf("%s missing", k)
		}
	}
	for _, f := range []string{"configured", "up", "down", "peers_ok", "peers", "spread_ms", "local_ms", "list"} {
		if _, ok := full.Nodes[f]; !ok {
			t.Errorf("nodes.%s missing", f)
		}
	}
	for _, f := range []string{"unread", "urgent", "ask", "fyi"} {
		if _, ok := full.Inbox[f]; !ok {
			t.Errorf("inbox.%s missing", f)
		}
	}
	if full.Unknown == nil || full.Prob == nil {
		t.Errorf("unknown and problems are lists, never null: %s", b)
	}
}
