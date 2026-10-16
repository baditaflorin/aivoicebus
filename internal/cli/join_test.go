package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

const (
	retiredToken = "retired-token-0123456789-012345"
	machineToken = "machine-token-0123456789-012345"
	testHost     = "testbox"
)

// bareMachine is a home with nothing of aivoicebus's on it, a stand-in for the running binary,
// a fixed host label, and no terminal to confirm anything on.
func bareMachine(t *testing.T) (home, exe string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_URL", "")
	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_TOKEN_FILE", "")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_HOST", testHost)
	exe = filepath.Join(t.TempDir(), "aivoicebus-running")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe, oldConfirm := executable, confirmHost
	executable = func() (string, error) { return exe, nil }
	confirmHost = func(string, string) (bool, error) { return false, errors.New("no terminal here") }
	t.Cleanup(func() { executable, confirmHost = oldExe, oldConfirm })
	return home, exe
}

func answer(t *testing.T, fn func(prompt, want string) (bool, error)) {
	t.Helper()
	confirmHost = fn
}

// trustNode records url as a node of the signed list, so join may send the token there unasked.
func trustNode(t *testing.T, url string) {
	t.Helper()
	if err := config.SaveDiscovered(config.Discovered{Serial: 1, Nodes: []string{url}, FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

type seen struct {
	Method, Path, Auth string
	Body               []byte
}

// spyNode wraps a node handler: it records every request and lets a test answer some itself.
type spyNode struct {
	inner http.Handler
	mu    sync.Mutex
	reqs  []seen
	hook  func(w http.ResponseWriter, r *http.Request, n int) bool // n counts earlier requests to the same path
}

func (s *spyNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(b)))
	s.mu.Lock()
	n := 0
	for _, q := range s.reqs {
		if q.Path == r.URL.Path {
			n++
		}
	}
	s.reqs = append(s.reqs, seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), b})
	s.mu.Unlock()
	if s.hook != nil && s.hook(w, r, n) {
		return
	}
	s.inner.ServeHTTP(w, r)
}

func (s *spyNode) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.reqs) }

func (s *spyNode) to(path string) []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []seen
	for _, q := range s.reqs {
		if q.Path == path {
			out = append(out, q)
		}
	}
	return out
}

func newSpy(t *testing.T, inner http.Handler) (*spyNode, *httptest.Server) {
	t.Helper()
	sp := &spyNode{inner: inner}
	srv := httptest.NewServer(sp)
	t.Cleanup(srv.Close)
	return sp, srv
}

func TestJoinBootstrapsABareMachineAndTheProbeIsPrivateAndInvisible(t *testing.T) {
	sp, srv := newSpy(t, nodeHandler(t))
	home, exe := bareMachine(t)
	trustNode(t, srv.URL)

	code, out, e := run(t, goodToken+"\n", "join", "--url", srv.URL)
	if code != 0 || !strings.Contains(out, "join ok") || !strings.Contains(out, "acknowledged and retracted") {
		t.Fatalf("exit %d\n%s\n%s", code, out, e)
	}
	if strings.Contains(out+e, goodToken) {
		t.Errorf("join printed the token:\n%s\n%s", out, e)
	}
	if fi, err := os.Stat(filepath.Join(home, ".config/aivoicebus/token")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("token file: %v %v", fi, err)
	}
	want, _ := os.ReadFile(exe)
	if got, _ := os.ReadFile(filepath.Join(home, ".local/bin/aivoicebus")); string(got) != string(want) {
		t.Errorf("the running binary was not installed")
	}

	// the probe goes from a throwaway identity to exactly one other, never to a selector or everyone
	sends := sp.to("/v1/send")
	if len(sends) != 1 {
		t.Fatalf("want one probe send, got %d", len(sends))
	}
	var m msg.Message
	if err := json.Unmarshal(sends[0].Body, &m); err != nil {
		t.Fatal(err)
	}
	if m.To != testHost+"/join-reader" || m.From != testHost+"/join-probe" || m.Prio != msg.PrioFYI || m.TTL != probeTTL {
		t.Errorf("probe is %+v", m)
	}
	// and it is cleaned up: acknowledged by the reader, then retracted by the sender
	acks := sp.to("/v1/ack")
	if len(acks) != 1 || !strings.Contains(string(acks[0].Body), `"by":"`+testHost+`/join-reader"`) || !strings.Contains(string(acks[0].Body), m.ID) {
		t.Errorf("probe ack: %+v", acks)
	}
	if rs := sp.to("/v1/retract"); len(rs) != 1 || !strings.Contains(string(rs[0].Body), `"by":"`+testHost+`/join-probe"`) {
		t.Errorf("probe retract: %+v", rs)
	}

	// nobody sees a session that was never one
	code, who, e := run(t, "", "who", "--all")
	if code != 0 || strings.Contains(who, "join-reader") || strings.Contains(who, "join-probe") {
		t.Errorf("who shows a probe: %d\n%s\n%s", code, who, e)
	}

	// joining again is safe and does not ask for the token (nothing is piped in)
	code, out, e = run(t, "", "join")
	if code != 0 || !strings.Contains(out, "kept") || strings.Contains(out, "changed") {
		t.Errorf("second join: %d\n%s\n%s", code, out, e)
	}
}

func TestJoinNeverSendsTheTokenToAnAddressNobodyVouchedFor(t *testing.T) {
	var prompts []string
	cases := []struct {
		name  string
		args  []string
		reply func(prompt, want string) (bool, error)
		want  string
	}{
		{"not in the signed list", nil, nil, "not in this machine's signed node list"},
		{"trusted by flag but nobody confirms", []string{"--trust-url"}, func(p, w string) (bool, error) { prompts = append(prompts, p); return false, nil }, "was not confirmed"},
		{"trusted by flag but the wrong host typed", []string{"--trust-url"}, func(p, w string) (bool, error) { return strings.TrimSpace("wrong") == w, nil }, "was not confirmed"},
		{"trusted by flag but no terminal", []string{"--trust-url"}, nil, "no terminal here"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sp, srv := newSpy(t, nodeHandler(t))
			home, _ := bareMachine(t)
			if c.reply != nil {
				answer(t, c.reply)
			}
			code, out, e := run(t, goodToken+"\n", append([]string{"join", "--url", srv.URL}, c.args...)...)
			if code != 1 || !strings.Contains(out+e, c.want) {
				t.Errorf("exit %d, want 1 with %q\n%s\n%s", code, c.want, out, e)
			}
			if sp.count() != 0 {
				t.Errorf("the node was contacted %d time(s): %+v", sp.count(), sp.reqs)
			}
			if left := tree(t, home); len(left) != 0 {
				t.Errorf("a refused join wrote %v", left)
			}
			if strings.Contains(out+e, goodToken) {
				t.Errorf("printed the token")
			}
		})
	}

	t.Run("confirmed by typing the host", func(t *testing.T) {
		sp, srv := newSpy(t, nodeHandler(t))
		bareMachine(t)
		var asked string
		answer(t, func(p, w string) (bool, error) { asked = p; return w == strings.TrimPrefix(srv.URL, "http://"), nil })
		code, out, e := run(t, goodToken+"\n", "join", "--url", srv.URL, "--trust-url")
		if code != 0 || sp.count() == 0 || !strings.Contains(asked, strings.TrimPrefix(srv.URL, "http://")) {
			t.Errorf("%d asked=%q\n%s\n%s", code, asked, out, e)
		}
	})

	t.Run("a different node than this machine is set up for", func(t *testing.T) {
		sp, srv := newSpy(t, nodeHandler(t))
		home, _ := bareMachine(t)
		trustNode(t, srv.URL) // vouched for, but not what this machine uses now
		t.Setenv("AIVOICEBUS_URL", "http://10.9.9.9:7777")
		answer(t, func(string, string) (bool, error) { return false, nil })
		code, out, e := run(t, goodToken+"\n", "join", "--url", srv.URL)
		if code != 1 || !strings.Contains(e, "this machine is configured for http://10.9.9.9:7777") || !strings.Contains(e, srv.URL) || sp.count() != 0 {
			t.Errorf("%d reqs=%d\n%s\n%s", code, sp.count(), out, e)
		}
		for _, f := range tree(t, home) { // only the discovered-list file the test itself made
			if !strings.HasSuffix(f, "nodes.json") && !strings.Contains(f, "nodes-") {
				t.Errorf("wrote %s", f)
			}
		}
	})

	t.Run("plain http across the open internet is refused even with confirmation", func(t *testing.T) {
		bareMachine(t)
		answer(t, func(string, string) (bool, error) { t.Error("asked to confirm"); return true, nil })
		code, out, e := run(t, goodToken+"\n", "join", "--url", "http://203.0.113.9:7777", "--trust-url")
		if code != 1 || !strings.Contains(e, "plain http") {
			t.Errorf("%d\n%s\n%s", code, out, e)
		}
	})
}

func TestPrivateHost(t *testing.T) {
	old := lookupIP
	t.Cleanup(func() { lookupIP = old })
	lookupIP = func(h string) ([]net.IP, error) {
		switch h {
		case "inside":
			return []net.IP{net.ParseIP("10.1.2.3"), net.ParseIP("100.64.0.9")}, nil
		case "mixed":
			return []net.IP{net.ParseIP("10.1.2.3"), net.ParseIP("8.8.8.8")}, nil
		}
		return nil, errors.New("no such host")
	}
	for host, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.0.0.1": true, "192.168.1.5": true, "172.16.0.1": true, "169.254.1.1": true,
		"100.64.0.1": true, "100.127.255.1": true, "100.128.0.1": false, "100.63.0.1": false,
		"8.8.8.8": false, "203.0.113.9": false, "inside": true, "mixed": false, "unknown": false,
	} {
		if got := privateHost(host); got != want {
			t.Errorf("privateHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestJoinWorksWithAMachineTokenBoundToTheHostLabel(t *testing.T) {
	st, _ := store.Open(t.TempDir(), "n1")
	ts := &server.TokenSet{}
	ts.Add(machineToken, server.Principal{Name: "machine", Host: testHost})
	srv := httptest.NewServer(server.NewHandler(st, goodToken, server.WithTokenSet(ts)))
	defer srv.Close()
	bareMachine(t) // AIVOICEBUS_HOST=testbox, as a real session's identity would use
	trustNode(t, srv.URL)
	code, out, e := run(t, machineToken+"\n", "join", "--url", srv.URL)
	if code != 0 || !strings.Contains(out, "join ok") || strings.Contains(out, "WARN") {
		t.Errorf("a host-bound token must work with the probe identities: %d\n%s\n%s", code, out, e)
	}
}

func TestJoinLeavesAWorkingSetupUntouchedWhenTheNewTokenFails(t *testing.T) {
	sp, srv := newSpy(t, nodeHandler(t))
	home, _ := bareMachine(t)
	trustNode(t, srv.URL)
	if code, out, _ := run(t, goodToken+"\n", "join", "--url", srv.URL); code != 0 {
		t.Fatal(out)
	}
	before := snapshot(t, home)

	// a wrong paste with --replace-token keeps the old token, env and everything else
	if code, out, _ := run(t, retiredToken+"\n", "join", "--replace-token"); code != 1 || !strings.Contains(out, "nothing on this machine was changed") {
		t.Errorf("wrong paste: %d\n%s", code, out)
	}
	if after := snapshot(t, home); after != before {
		t.Errorf("a failed replace changed files:\n%s\n--\n%s", before, after)
	}
	// so does a mesh that does not answer
	srv.Close()
	if code, out, _ := run(t, goodToken+"\n", "join", "--replace-token"); code != 1 {
		t.Errorf("down: %d\n%s", code, out)
	}
	if after := snapshot(t, home); after != before {
		t.Errorf("a failed join changed files")
	}
	_ = sp
}

func TestJoinRecoversFromARefusedTokenWithReplaceToken(t *testing.T) {
	_, srv := newSpy(t, nodeHandler(t))
	home, _ := bareMachine(t)
	trustNode(t, srv.URL)
	tok := filepath.Join(home, ".config/aivoicebus/token")
	os.MkdirAll(filepath.Dir(tok), 0o700)
	os.WriteFile(tok, []byte(retiredToken+"\n"), 0o600)
	// the saved (refused) token is kept and fails the same way, nothing is asked
	if code, out, _ := run(t, goodToken+"\n", "join", "--url", srv.URL); code != 1 || !strings.Contains(out, "kept") {
		t.Errorf("kept: %d\n%s", code, out)
	}
	if code, out, e := run(t, goodToken+"\n", "join", "--url", srv.URL, "--replace-token"); code != 0 || !strings.Contains(out, "join ok") {
		t.Errorf("replace: %d\n%s\n%s", code, out, e)
	}
	if b, _ := os.ReadFile(tok); strings.TrimSpace(string(b)) != goodToken {
		t.Errorf("the new token was not saved")
	}
}

func TestJoinFailureModes(t *testing.T) {
	_, srv := newSpy(t, nodeHandler(t))
	cases := []struct {
		name  string
		url   string
		stdin string
		args  []string
		code  int
		want  []string
	}{
		{"mesh refuses the token", srv.URL, retiredToken + "\n", nil, 1, []string{"join FAILED at probe", "rejected this machine's token", "--replace-token"}},
		{"nobody answers", "http://127.0.0.1:1", goodToken + "\n", nil, 1, []string{"join FAILED at probe", "check the node URLs"}},
		{"token too short", srv.URL, "short\n", nil, 1, []string{"join FAILED at token", "shorter than 24"}},
		{"token with a space", srv.URL, "two words in one paste for sure\n", nil, 1, []string{"join FAILED at token"}},
		{"no token to read", srv.URL, "", nil, 1, []string{"join FAILED at token"}},
		{"not a node url", "ftp://x", goodToken, nil, 1, nil},
		{"a token on the command line is not a thing", srv.URL, "", []string{"--token", goodToken}, 2, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home, _ := bareMachine(t)
			trustNode(t, c.url)
			args := append([]string{"join", "--url", c.url}, c.args...)
			code, out, e := run(t, c.stdin, args...)
			if code != c.code {
				t.Errorf("exit %d, want %d\n%s\n%s", code, c.code, out, e)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, tok := range []string{retiredToken, goodToken} {
				if strings.Contains(out+e, tok) {
					t.Errorf("printed a token:\n%s\n%s", out, e)
				}
			}
			for _, f := range tree(t, home) {
				if !strings.Contains(f, "nodes") { // the node list the test seeded
					t.Errorf("a failed join left %s", f)
				}
			}
		})
	}
}

func TestJoinProbesWithTheSavedTokenNotTheShells(t *testing.T) {
	_, srv := newSpy(t, nodeHandler(t))
	bareMachine(t)
	trustNode(t, srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", goodToken)
	code, out, _ := run(t, retiredToken+"\n", "join", "--url", srv.URL)
	if code != 1 || !strings.Contains(out, "rejected this machine's token") {
		t.Errorf("%d\n%s", code, out)
	}
}

func TestJoinSavesWhereTheClientReadsAndKeepsOnlyUsableTokenFiles(t *testing.T) {
	_, srv := newSpy(t, nodeHandler(t))
	home, _ := bareMachine(t)
	trustNode(t, srv.URL)
	custom := filepath.Join(t.TempDir(), "elsewhere", "tok")
	t.Setenv("AIVOICEBUS_TOKEN_FILE", custom)
	if code, out, e := run(t, goodToken+"\n", "join", "--url", srv.URL); code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	if b, _ := os.ReadFile(custom); strings.TrimSpace(string(b)) != goodToken {
		t.Errorf("the token is not where AIVOICEBUS_TOKEN_FILE says")
	}
	if _, err := os.Stat(filepath.Join(home, ".config/aivoicebus/token")); err == nil {
		t.Errorf("a token was also written to the default path")
	}

	// a token file others can read, or one that is not a token, is not kept
	for name, setup := range map[string]func(){
		"readable by others": func() { os.Chmod(custom, 0o644) },
		"not a token":        func() { os.WriteFile(custom, []byte("this has spaces in it and is long enough\n"), 0o600) },
	} {
		setup()
		code, out, e := run(t, goodToken+"\n", "join")
		if code != 0 || strings.Contains(out, "kept") || !strings.Contains(out, "changed      token") {
			t.Errorf("%s: %d\n%s\n%s", name, code, out, e)
		}
		if fi, _ := os.Stat(custom); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: saved with mode %v", name, fi.Mode().Perm())
		}
	}
}

func TestJoinSaysWhatCleanupLeftBehind(t *testing.T) {
	for _, path := range []string{"/v1/retract", "/v1/ack"} {
		t.Run(path, func(t *testing.T) {
			sp, srv := newSpy(t, nodeHandler(t))
			sp.hook = func(w http.ResponseWriter, r *http.Request, n int) bool {
				if r.URL.Path == path {
					http.Error(w, "broken", http.StatusInternalServerError)
					return true
				}
				return false
			}
			bareMachine(t)
			trustNode(t, srv.URL)
			code, out, e := run(t, goodToken+"\n", "join", "--url", srv.URL)
			if code != 0 || !strings.Contains(out, "left to expire in 300 s") || strings.Contains(out, "acknowledged and retracted") {
				t.Errorf("%d\n%s\n%s", code, out, e)
			}
		})
	}
}

func TestJoinReadsTheProbeBackEvenWhenTheFirstReadsMissIt(t *testing.T) {
	sp, srv := newSpy(t, nodeHandler(t))
	sp.hook = func(w http.ResponseWriter, r *http.Request, n int) bool {
		if r.URL.Path == "/v1/inbox" && n < 4 { // a read that lands on a node the write has not reached
			w.Write([]byte("[]"))
			return true
		}
		return false
	}
	bareMachine(t)
	trustNode(t, srv.URL)
	if code, out, e := run(t, goodToken+"\n", "join", "--url", srv.URL); code != 0 || !strings.Contains(out, "join ok") {
		t.Errorf("%d\n%s\n%s", code, out, e)
	}
}

func TestJoinFailsWhenTheProbeNeverComesBackAndIsNotFooledByAStaleError(t *testing.T) {
	sp, srv := newSpy(t, nodeHandler(t))
	sp.hook = func(w http.ResponseWriter, r *http.Request, n int) bool {
		if r.URL.Path != "/v1/inbox" {
			return false
		}
		if n == 0 {
			http.Error(w, "hiccup", http.StatusInternalServerError)
		} else {
			w.Write([]byte("[]"))
		}
		return true
	}
	bareMachine(t)
	trustNode(t, srv.URL)
	code, out, _ := run(t, goodToken+"\n", "join", "--url", srv.URL)
	if code != 1 || !strings.Contains(out, "did not come back") || strings.Contains(out, "reading it failed") {
		t.Errorf("%d\n%s", code, out)
	}
}

func TestJoinDryRunWritesAndSendsNothingAndReportsSetupProblems(t *testing.T) {
	sp, srv := newSpy(t, nodeHandler(t))
	home, _ := bareMachine(t)
	trustNode(t, srv.URL)
	before := snapshot(t, home)
	code, out, e := run(t, "", "join", "--url", srv.URL, "--dry-run")
	if code != 0 || !strings.Contains(out, "dry run: nothing was written") || !strings.Contains(out, "would ask") {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	if sp.count() != 0 || snapshot(t, home) != before {
		t.Errorf("a dry run touched something (requests: %d)", sp.count())
	}

	// a settings.json that cannot be edited is a problem the plan must show, not hide
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude/settings.json"), []byte("{not json"), 0o644)
	if code, out, _ := run(t, "", "join", "--url", srv.URL, "--dry-run"); code != 1 || !strings.Contains(out, "join FAILED at setup") {
		t.Errorf("dry run hid a setup failure: %d\n%s", code, out)
	}
}

func TestWriteTokenFileReplacesTheFileAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d", "token")
	if err := writeTokenFile(path, goodToken); err != nil {
		t.Fatal(err)
	}
	first, _ := os.Stat(path)
	if err := writeTokenFile(path, retiredToken); err != nil {
		t.Fatal(err)
	}
	second, _ := os.Stat(path)
	if os.SameFile(first, second) { // an in-place rewrite could leave half a token after a crash
		t.Errorf("the token was rewritten in place")
	}
	if b, _ := os.ReadFile(path); strings.TrimSpace(string(b)) != retiredToken || second.Mode().Perm() != 0o600 {
		t.Errorf("content %q mode %v", b, second.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".token-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

// tree lists every file under dir.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}
