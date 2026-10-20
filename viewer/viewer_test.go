package viewer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

const (
	fullTok = "full-token-0123456789-0123456789"
	obsTok  = "observer-token-0123456789-012345"
	key     = "0123456789abcdef0123456789abcdef"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func TestSessionCardsComeFromTheRosterAndTheMessages(t *testing.T) {
	roster := []msg.RosterEntry{
		{Me: "laptop-a/claude.a1b2", Seen: t0, Title: "Fix the login", Labels: msg.Labels{"tool": "claude", "machine": "laptop-a-m1-mbp", "repo": "dash", "branch": "fix/login", "model": "claude-opus-5-5"}},
		{Me: "build/codex.q1w2", Seen: t0.Add(-time.Hour)},
	}
	ms := []msg.Message{
		{ID: "a000000001", Thread: "a000000001", From: "laptop-a/claude.a1b2", To: "build/codex.q1w2", At: t0.Add(-time.Minute), Body: "q"},
		{ID: "a000000002", Thread: "a000000001", From: "build/codex.q1w2", FromTitle: "Review the parser", FromModel: "gpt-5.1-codex", To: "laptop-a/claude.a1b2", At: t0.Add(-30 * time.Second), Body: "a"},
		{ID: "a000000003", Thread: "a000000003", From: "gone/gemini.z7y6", FromTitle: "Old work", To: "all", At: t0.Add(-48 * time.Hour), Body: "hi"},
	}
	cards := Sessions(roster, ms)
	if len(cards) != 3 {
		t.Fatalf("%+v", cards)
	}
	by := map[string]Session{}
	for _, c := range cards {
		by[c.Me] = c
	}
	a := by["laptop-a/claude.a1b2"]
	if a.Title != "Fix the login" || a.Tool != "claude" || a.Machine != "laptop-a-m1-mbp" || a.Repo != "dash" || a.Branch != "fix/login" || a.Sent != 1 || a.Received != 1 || a.Threads != 1 {
		t.Errorf("%+v", a)
	}
	if want := []string{"repo:dash", "branch:fix/login", "tool:claude", "model:claude-opus-5-5"}; !slices.Equal(a.Caps, want) {
		t.Errorf("caps %v, want %v", a.Caps, want)
	}
	if a.Model != "claude-opus-5-5" || by["build/codex.q1w2"].Model != "gpt-5.1-codex" {
		t.Errorf("models from the label, else the latest message: %q %q", a.Model, by["build/codex.q1w2"].Model)
	}
	if b := by["build/codex.q1w2"]; b.Title != "Review the parser" || b.Tool != "codex" || b.Machine != "build" || !b.LastSeen.Equal(t0.Add(-30*time.Second)) {
		t.Errorf("the roster forgot its title and saw it earlier than its last message: %+v", b)
	}
	if g := by["gone/gemini.z7y6"]; g.Title != "Old work" || g.Sent != 1 {
		t.Errorf("a session known only from what it sent: %+v", g)
	}
	if cards[0].Me != "laptop-a/claude.a1b2" {
		t.Errorf("newest first: %+v", cards[0])
	}
}

func TestConversationsGroupByThreadNameTheParticipantsAndFilterBySession(t *testing.T) {
	ms := []msg.Message{
		{ID: "a000000001", Thread: "a000000001", From: "h/claude.a1b2", FromTitle: "Fix the login", To: "b/codex.q1w2", At: t0, Body: "Does the parser accept it?\nmore"},
		{ID: "a000000002", Thread: "a000000001", From: "b/codex.q1w2", To: "h/claude.a1b2", At: t0.Add(time.Minute), Body: "yes"},
		{ID: "a000000003", Thread: "a000000003", From: "c/claude.k9x3", To: "all", At: t0.Add(2 * time.Minute), Body: "release notes", Retracted: &msg.Retraction{By: "c/claude.k9x3"}},
	}
	all := Conversations(ms, map[string]string{"b/codex.q1w2": "Review the parser"}, "")
	if len(all) != 2 || all[0].Thread != "a000000003" {
		t.Fatalf("newest first: %+v", all)
	}
	c := all[1]
	if c.Subject != "Does the parser accept it?" || c.Messages != 2 || len(c.Participants) != 2 || c.LastFrom != "b/codex.q1w2" {
		t.Errorf("%+v", c)
	}
	names := map[string]string{}
	for _, p := range c.Participants {
		names[p.Me] = p.Title
	}
	if names["h/claude.a1b2"] != "Fix the login" || names["b/codex.q1w2"] != "Review the parser" {
		t.Errorf("titles from the messages, else the roster: %v", names)
	}
	if b := all[0]; len(b.Audience) != 1 || b.Audience[0] != "all" || b.Withdrawn != 1 || b.Subject != "" {
		t.Errorf("a broadcast that was withdrawn: %+v", b)
	}
	only := Conversations(ms, nil, "b/codex.q1w2")
	if len(only) != 1 || only[0].Thread != "a000000001" {
		t.Errorf("filtered: %+v", only)
	}
}

// mesh is one real node holding a little traffic, with a full and an observer token.
func mesh(t *testing.T) (*store.Store, string) {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, fullTok, server.WithObserverToken(obsTok)))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func follow(t *testing.T, url string) (*Viewer, *httptest.Server) {
	t.Helper()
	v, err := Open(Options{Nodes: []string{url}, Token: obsTok, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Pull(); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(v.Handler(HTTPOptions{Key: key}))
	t.Cleanup(hs.Close)
	return v, hs
}

type os_discard struct{}

func (os_discard) Write(p []byte) (int, error) { return len(p), nil }

func getJSON(t *testing.T, hs *httptest.Server, path string, into any) int {
	t.Helper()
	req, _ := http.NewRequest("GET", hs.URL+path, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: key})
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil && resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func TestTheViewerFollowsANodeWithTheObserverTokenAndAppliesItsRules(t *testing.T) {
	s, url := mesh(t)
	q, _ := s.Send(msg.Message{From: "h/claude.a1b2", FromTitle: "Fix the login", To: "b/codex.q1w2", Body: "question"})
	s.Send(msg.Message{From: "b/codex.q1w2", To: "h/claude.a1b2", Re: q.ID, Thread: q.ID, Body: "answer"})
	w, _ := s.Send(msg.Message{From: "h/claude.a1b2", FromTitle: "Fix the login", To: "b/codex.q1w2", Body: "the secret plan"})
	s.Retract(w.ID, "h/claude.a1b2", false)
	s.InboxFor("b/codex.q1w2", false, msg.Labels{"repo": "parser"})
	s.NoteTitle("b/codex.q1w2", "Review the parser")
	s.Ack(q.ID, "b/codex.q1w2")

	v, hs := follow(t, url)
	var sessions []Session
	if code := getJSON(t, hs, "/api/sessions", &sessions); code != 200 || len(sessions) != 2 {
		t.Fatalf("%d %+v", code, sessions)
	}
	var convs []Conversation
	getJSON(t, hs, "/api/conversations?session=b/codex.q1w2", &convs)
	if len(convs) != 2 {
		t.Fatalf("%+v", convs)
	}
	var thread []ThreadMessage
	getJSON(t, hs, "/api/thread?id="+q.ID, &thread)
	if len(thread) != 2 || thread[0].FromTitle != "Fix the login" || thread[0].ReadBy != 1 {
		t.Errorf("%+v", thread)
	}
	var gone []ThreadMessage
	getJSON(t, hs, "/api/thread?id="+w.ID, &gone)
	if len(gone) != 1 || gone[0].Retracted == nil || strings.Contains(gone[0].Body, "secret plan") || gone[0].FromTitle != "" {
		t.Errorf("a withdrawn message must show as its tombstone, without its words or title: %+v", gone)
	}
	// news arrives with the next pull, and the revision says so
	var st struct{ Rev int64 }
	getJSON(t, hs, "/api/status", &st)
	s.Send(msg.Message{From: "b/codex.q1w2", To: "all", Body: "later"})
	v.Pull()
	var st2 struct{ Rev int64 }
	getJSON(t, hs, "/api/status", &st2)
	if st2.Rev <= st.Rev {
		t.Errorf("rev %d then %d", st.Rev, st2.Rev)
	}
}

func TestTheObserverTokenCannotWriteSoTheViewerCannotEither(t *testing.T) {
	_, url := mesh(t)
	c := &client.Client{URLs: []string{url}, Token: obsTok, Warn: os_discard{}}
	if _, err := c.Send(msg.Message{From: "h/a", To: "all", Body: "x"}); err == nil {
		t.Error("an observer token sent a message")
	}
	if info, err := c.TokenInfo(); err != nil || !info.Observer {
		t.Errorf("%+v %v", info, err)
	}
}

func TestTheGuardKeepsThePageToItsKeyItsHostAndReadOnly(t *testing.T) {
	v, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(v.Handler(HTTPOptions{Key: key, Hosts: []string{"viewer.test:1"}}))
	defer hs.Close()
	do := func(method, path, host string, cookie bool) *http.Response {
		req, _ := http.NewRequest(method, hs.URL+path, nil)
		req.Host = host
		if cookie {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: key})
		}
		c := hs.Client()
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := do("GET", "/api/status", "evil.example:1", true); r.StatusCode != 403 {
		t.Errorf("another host (DNS rebinding): %d", r.StatusCode)
	}
	if r := do("GET", "/api/status", "viewer.test:1", false); r.StatusCode != 403 {
		t.Errorf("no cookie: %d", r.StatusCode)
	}
	if r := do("GET", "/?key=wrong", "viewer.test:1", false); r.StatusCode != 403 {
		t.Errorf("wrong key: %d", r.StatusCode)
	}
	r := do("GET", "/?key="+key, "viewer.test:1", false)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "./" || !strings.Contains(r.Header.Get("Set-Cookie"), "HttpOnly") || !strings.Contains(r.Header.Get("Set-Cookie"), "SameSite=Strict") {
		t.Errorf("the key sets a strict cookie and leaves the address bar: %d %v", r.StatusCode, r.Header)
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if r := do(m, "/api/status", "viewer.test:1", true); r.StatusCode != 405 {
			t.Errorf("%s: %d", m, r.StatusCode)
		}
	}
	r = do("GET", "/", "viewer.test:1", true)
	if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Security-Policy"), "script-src 'self'") || r.Header.Get("X-Frame-Options") != "DENY" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("%d %v", r.StatusCode, r.Header)
	}
	for _, p := range []string{"/viewer.js", "/lib.js", "/viewer.css"} {
		if r := do("GET", p, "viewer.test:1", true); r.StatusCode != 200 {
			t.Errorf("%s: %d", p, r.StatusCode)
		}
	}
	if r := do("GET", "/api/thread?id=../../x", "viewer.test:1", true); r.StatusCode != 400 {
		t.Errorf("a bad id: %d", r.StatusCode)
	}
}

// Everything the page shows was written by agents: it must never turn data into markup.
func TestThePageScriptNeverWritesMarkup(t *testing.T) {
	scripts, _ := filepath.Glob("ui/*.js")
	if len(scripts) < 2 { // viewer.js and lib.js
		t.Fatalf("scripts found: %v", scripts)
	}
	for _, f := range scripts {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(string(b), "")
		for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "setAttribute(\"on", "srcdoc",
			"createContextualFragment", "DOMParser", "setHTML", "location.href =", "location.assign", "location.replace", "setTimeout(\"", "setInterval(\""} {
			if strings.Contains(src, banned) {
				t.Errorf("%s uses %s", f, banned)
			}
		}
	}
	html, _ := os.ReadFile("ui/index.html")
	if strings.Contains(string(html), "<script>") || strings.Contains(string(html), " on") && regexp.MustCompile(`\son[a-z]+=`).Match(html) {
		t.Error("index.html has inline script or handlers (the CSP forbids them)")
	}
}

func TestTheDemoBoardHasSessionsConversationsAndAWithdrawnMessage(t *testing.T) {
	v, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Demo(time.Now()); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(v.Handler(HTTPOptions{Key: key}))
	defer hs.Close()
	var sessions []Session
	var convs []Conversation
	getJSON(t, hs, "/api/sessions", &sessions)
	getJSON(t, hs, "/api/conversations", &convs)
	withdrawn := 0
	for _, c := range convs {
		withdrawn += c.Withdrawn
	}
	if len(sessions) != 4 || len(convs) < 4 || withdrawn != 1 || sessions[0].Title == "" {
		t.Errorf("%d sessions, %d conversations, %d withdrawn: %+v", len(sessions), len(convs), withdrawn, sessions)
	}
}

// A task's thread says who took it, and every message says who read it.
func TestTheThreadViewSaysWhoTookATaskAndWhoReadIt(t *testing.T) {
	v, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Demo(time.Now()); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(v.Handler(HTTPOptions{Key: key}))
	defer hs.Close()
	var convs []Conversation
	getJSON(t, hs, "/api/conversations", &convs)
	found := false
	for _, c := range convs {
		var thread []ThreadMessage
		getJSON(t, hs, "/api/thread?id="+c.Thread, &thread)
		for _, m := range thread {
			if m.Kind != msg.KindTask {
				continue
			}
			found = true
			if m.TakenBy != "build-1/codex.q1w2" || m.ReadBy != 1 || len(m.Readers) != 1 || m.Readers[0] != "build-1/codex.q1w2" {
				t.Errorf("task in the thread view: %+v", m)
			}
		}
	}
	if !found {
		t.Error("the demo board has no task")
	}
}
