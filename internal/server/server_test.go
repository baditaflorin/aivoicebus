package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestAuth(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "t"))
	defer srv.Close()
	r, _ := srv.Client().Get(srv.URL + "/v1/who")
	if r.StatusCode != 401 {
		t.Fatalf("got %d", r.StatusCode)
	}
	if h, _ := srv.Client().Get(srv.URL + "/healthz"); h.StatusCode != 200 {
		t.Fatalf("healthz should not need a token, got %d", h.StatusCode)
	}
}

func TestSendRejectsBadRequests(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "t"))
	defer srv.Close()
	c := &client.Client{URLs: []string{srv.URL}, Token: "t"}
	for name, m := range map[string]msg.Message{
		"no body": {From: "a/b", To: "all"},
		"no from": {To: "all", Body: "x"},
		"no to":   {From: "a/b", Body: "x"},
	} {
		if _, err := c.Send(m); err == nil {
			t.Errorf("%s: expected a 400", name)
		}
	}
}

func TestBootstrapServesOnlyKeyedFreshFile(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "a")
	f := filepath.Join(t.TempDir(), "b.tgz")
	os.WriteFile(f, []byte("payload"), 0o644)
	srv := httptest.NewServer(WithBootstrap(NewHandler(s, "t"), "k0123456789012345678901234", f, time.Hour))
	defer srv.Close()
	get := func(p string) int { r, _ := srv.Client().Get(srv.URL + p); return r.StatusCode }
	if get("/bootstrap/k0123456789012345678901234.tgz") != 200 {
		t.Fatal("keyed path should serve")
	}
	if c := get("/bootstrap/wrong.tgz"); c == 200 { // falls through to the authenticated API: never the file
		t.Fatal("wrong key served the file")
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(f, old, old)
	if get("/bootstrap/k0123456789012345678901234.tgz") != 404 {
		t.Fatal("stale file must not be served")
	}
	os.Remove(f)
	if get("/bootstrap/k0123456789012345678901234.tgz") != 404 {
		t.Fatal("removed file must 404")
	}
}

func TestReplicationConvergesAndSurvivesNodeLoss(t *testing.T) {
	a, _ := store.Open(t.TempDir(), "a")
	b, _ := store.Open(t.TempDir(), "b")
	sa := httptest.NewServer(NewHandler(a, "tok"))
	sb := httptest.NewServer(NewHandler(b, "tok"))
	defer sb.Close()

	// writes land on different nodes while they are not connected
	m1, _ := a.Send(msg.Message{From: "imac/claude", To: "air/codex", Body: "from a"})
	m2, _ := b.Send(msg.Message{From: "air/codex", To: "imac/claude", Body: "from b"})
	b.Ack(m2.ID, "imac/claude")

	if _, err := PullOnce(b, sa.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := PullOnce(a, sb.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*store.Store{a, b} {
		if len(s.Inbox("air/codex", false)) != 1 || len(s.Inbox("imac/claude", false)) != 0 {
			t.Fatal("nodes did not converge (message or ack missing)")
		}
	}
	// replay is idempotent
	if n, _ := PullOnce(b, sa.URL, "tok"); n != 0 {
		t.Fatalf("replay applied %d", n)
	}
	// node a dies; b still holds a's message and keeps working
	sa.Close()
	if len(b.Thread(m1.ID)) != 1 {
		t.Fatal("b lost a's message")
	}
	if _, err := b.Send(msg.Message{From: "air/codex", To: "all", Body: "still up"}); err != nil {
		t.Fatal(err)
	}
	// client failover: dead node first, live node second
	c := &client.Client{URLs: []string{sa.URL, sb.URL}, Token: "tok"}
	if out, err := c.Inbox("imac/claude", false); err != nil || len(out) != 1 {
		t.Fatalf("failover: %v %d", err, len(out))
	}
	// a rejoins and catches up
	a2, _ := store.Open(a.Dir(), "a")
	if _, err := PullOnce(a2, sb.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	if len(a2.Inbox("imac/claude", false)) != 1 {
		t.Fatal("rejoined node did not catch up")
	}
}

func TestDuplicateClientWriteIsIdempotent(t *testing.T) {
	a, _ := store.Open(t.TempDir(), "a")
	b, _ := store.Open(t.TempDir(), "b")
	m := msg.Message{ID: "00aa11bb22", From: "x/y", To: "all", Body: "once"}
	a.Send(m)
	b.Send(m) // same client write landed on both nodes
	sa := httptest.NewServer(NewHandler(a, "t"))
	defer sa.Close()
	if _, err := PullOnce(b, sa.URL, "t"); err != nil {
		t.Fatal(err)
	}
	if got := len(b.Inbox("z/z", false)); got != 1 {
		t.Fatalf("message duplicated: %d", got)
	}
}

func TestInboxDeliversSelectorMessagesByDeclaredLabelsAndRosterShowsThem(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "tok"))
	defer srv.Close()
	s.Send(msg.Message{From: "mac/claude.aaaa", To: "@repo=alpha", Body: "for alpha"})

	c := &client.Client{URLs: []string{srv.URL}, Token: "tok", Labels: msg.Labels{"repo": "alpha", "tool": "claude"}}
	if in, err := c.Inbox("air/claude.bbbb", false); err != nil || len(in) != 1 {
		t.Fatalf("alpha session: %v %+v", err, in)
	}
	c.Labels = msg.Labels{"repo": "beta"}
	if in, _ := c.Inbox("imac/claude.cccc", false); len(in) != 0 {
		t.Fatalf("beta session got %+v", in)
	}
	c.Labels = nil
	if in, _ := c.Inbox("old/agent", false); len(in) != 0 {
		t.Fatalf("a client declaring nothing got %+v", in)
	}

	r, err := c.Roster()
	if err != nil || len(r) != 4 {
		t.Fatalf("roster: %v %+v", err, r)
	}
	for _, e := range r {
		if e.Me == "air/claude.bbbb" && e.Labels["repo"] != "alpha" {
			t.Errorf("roster lost the labels: %+v", e)
		}
	}
	if _, err := (&client.Client{URLs: []string{srv.URL}, Token: "wrong"}).Roster(); err == nil {
		t.Error("the roster needs the token")
	}
}

func TestJunkLabelsNeverBreakAnInboxRead(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "tok"))
	defer srv.Close()
	s.Send(msg.Message{From: "a/b", To: "all", Body: "plain"})
	for _, junk := range []string{"labels=%00%ff", "labels=" + strings.Repeat("k=v,", 5000), "labels=%3D%3D%3D", "labels="} {
		resp, body := get(t, srv, "/v1/inbox?me=x/y&"+junk, "tok")
		if resp.StatusCode != 200 || !strings.Contains(body, "plain") {
			t.Errorf("%.40s: %d %.80s", junk, resp.StatusCode, body)
		}
	}
}

func TestSendRejectsBadPriorityAndTTLWith400(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "t"))
	defer srv.Close()
	c := &client.Client{URLs: []string{srv.URL}, Token: "t"}
	for name, m := range map[string]msg.Message{
		"unknown priority": {From: "a/b", To: "all", Body: "x", Prio: "critical"},
		"ttl too short":    {From: "a/b", To: "all", Body: "x", TTL: 5},
		"ttl too long":     {From: "a/b", To: "all", Body: "x", TTL: 99999999},
		"negative ttl":     {From: "a/b", To: "all", Body: "x", TTL: -1},
	} {
		if _, err := c.Send(m); err == nil || !strings.Contains(err.Error(), "400") {
			t.Errorf("%s: want a 400, got %v", name, err)
		}
	}
	if got := len(s.Inbox("z/z", false)); got != 0 {
		t.Errorf("rejected messages must not be stored, found %d", got)
	}
}

func TestPriorityAndExpiryRoundTripThroughTheAPI(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "t"))
	defer srv.Close()
	c := &client.Client{URLs: []string{srv.URL}, Token: "t"}
	before := time.Now()
	if _, err := c.Send(msg.Message{ID: msg.NewID(), From: "a/b", To: "all", Body: "x", Prio: "urgent", TTL: 3600}); err != nil {
		t.Fatal(err)
	}
	in, err := c.Inbox("z/z", false)
	if err != nil || len(in) != 1 {
		t.Fatalf("%v %+v", err, in)
	}
	m := in[0]
	if m.Prio != "urgent" || m.TTL != 0 || m.Exp == nil || m.Exp.Before(before.Add(59*time.Minute)) || m.Exp.After(time.Now().Add(61*time.Minute)) {
		t.Errorf("round trip: %+v", m)
	}
}

func TestSendOverTheLimitAnswers429WithRetryAfter(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	s.SetSendLimit(1)
	srv := httptest.NewServer(NewHandler(s, "t"))
	defer srv.Close()
	post := func(body string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/send", strings.NewReader(`{"from":"a/b","to":"all","body":"`+body+`"}`))
		req.Header.Set("Authorization", "Bearer t")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := post("one"); r.StatusCode != 200 {
		t.Fatalf("first send: %d", r.StatusCode)
	}
	r := post("two")
	if r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("second send: %d, Retry-After %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
}

func TestObserverTokenReadsTheFeedAndNothingElse(t *testing.T) {
	const observer = "observer-token-0123456789abcdef"
	s, _ := store.Open(t.TempDir(), "n1")
	m, _ := s.Send(msg.Message{From: "a/b", To: "all", Body: "hello"})
	srv := httptest.NewServer(NewHandler(s, "agent-token", WithObserverToken(observer)))
	defer srv.Close()
	call := func(token, method, path, body string) int {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	for _, path := range []string{"/v1/sync?vv=%7B%7D", "/v1/roster", "/v1/who", "/v1/thread?id=" + m.ID} {
		if got := call(observer, "GET", path, ""); got != 200 {
			t.Errorf("observer GET %s: %d, want 200", path, got)
		}
		if got := call("agent-token", "GET", path, ""); got != 200 {
			t.Errorf("agent GET %s: %d, want 200", path, got)
		}
	}
	forbidden := []struct{ method, path, body string }{
		{"POST", "/v1/send", `{"from":"x/y","to":"all","body":"forged"}`},
		{"POST", "/v1/ack", `{"id":"` + m.ID + `","by":"x/y"}`},
		{"GET", "/v1/inbox?me=x/y", ""},
		{"GET", "/v1/release/manifest.json", ""},
		{"POST", "/v1/who", ""}, // a read-only route, but not read with a POST
	}
	for _, f := range forbidden {
		if got := call(observer, f.method, f.path, f.body); got != http.StatusForbidden {
			t.Errorf("observer %s %s: %d, want 403", f.method, f.path, got)
		}
	}
	if r := s.Roster(); len(r) != 1 || r[0].Me != "a/b" { // only the sender of the seed message: no inbox was read
		t.Errorf("the observer registered a session: %+v", r)
	}
	if n := len(s.Inbox("z/z", true)); n != 1 { // reading this inbox registers z/z, so it comes last
		t.Errorf("the observer changed the messages: %d", n)
	}
	if got := call("wrong", "GET", "/v1/roster", ""); got != 401 {
		t.Errorf("an unknown token: %d, want 401", got)
	}
	if got := call("", "GET", "/v1/roster", ""); got != 401 {
		t.Errorf("no token: %d, want 401", got)
	}
}

func TestWithoutAnObserverTokenNoObserverExists(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "agent-token", WithObserverToken("")))
	defer srv.Close()
	for _, tok := range []string{"", "observer-token-0123456789abcdef"} {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/roster", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("token %q: %d, want 401", tok, resp.StatusCode)
		}
	}
}

func TestAckWithAnUnknownViaIsRefusedAndCharsAreBounded(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	m, _ := s.Send(msg.Message{From: "a/b", To: "all", Body: "hi"})
	srv := httptest.NewServer(NewHandler(s, "t"))
	defer srv.Close()
	post := func(body string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/ack", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer t")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post(`{"id":"` + m.ID + `","by":"x/y","via":"carrier-pigeon"}`); got != 400 {
		t.Errorf("unknown via: %d, want 400", got)
	}
	if got := post(`{"id":"` + m.ID + `","by":"x/y","via":"hook","chars":999999999}`); got != 200 {
		t.Fatalf("huge chars: %d", got)
	}
	if got := post(`{"id":"` + m.ID + `","by":"p/q","via":"hook","chars":-5}`); got != 200 {
		t.Fatalf("negative chars: %d", got)
	}
	for _, r := range s.Since(map[string]uint64{}, 100) {
		if r.Ack != nil && (r.Ack.Chars < 0 || r.Ack.Chars > msg.MaxBody) {
			t.Errorf("chars out of bounds in the log: %+v", r.Ack)
		}
	}
}

func TestLoadObserverToken(t *testing.T) {
	write := func(content string) string {
		f := filepath.Join(t.TempDir(), "tok")
		os.WriteFile(f, []byte(content), 0o600)
		return f
	}
	const shared = "shared-token-0123456789abcdefghij"
	if tok, err := loadObserverToken("", shared); tok != "" || err != nil {
		t.Errorf("none configured: %q %v", tok, err)
	}
	if tok, err := loadObserverToken(write("observer-token-0123456789abcdef\n"), shared); tok != "observer-token-0123456789abcdef" || err != nil {
		t.Errorf("a good token, trimmed: %q %v", tok, err)
	}
	for name, file := range map[string]string{
		"too short":      write("short"),
		"same as shared": write(shared),
		"missing file":   filepath.Join(t.TempDir(), "nope"),
		"empty":          write(""),
	} {
		if tok, err := loadObserverToken(file, shared); err == nil || tok != "" {
			t.Errorf("%s: want an error, got %q %v", name, tok, err)
		}
	}
}
