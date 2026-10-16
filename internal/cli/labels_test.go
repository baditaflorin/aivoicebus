package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// session makes the following commands run as identity me with the given labels.
func session(t *testing.T, me, labels string) {
	t.Helper()
	t.Setenv("AIVOICEBUS_ME", me)
	t.Setenv("AIVOICEBUS_LABELS", labels)
}

func TestSelectorsRouteMessagesToTheRightSessionsEndToEnd(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir()) // not a git checkout: labels come only from the environment

	// two sessions declare themselves by reading their inbox
	session(t, "mac/claude.aaaa", "repo=alpha,tool=claude")
	run(t, "", "inbox")
	session(t, "air/codex.bbbb", "repo=beta,tool=codex")
	run(t, "", "inbox")

	session(t, "other/agent", "")
	for _, to := range []string{"@repo=alpha", "@repo=beta,tool=codex", "@tool=claude", "@repo=gamma", "@repo=*"} {
		// --queue: @repo=gamma matches nobody on purpose, to prove it reaches no one
		if code, out, e := run(t, "", "send", "--queue", "--to", to, "for "+to); code != 0 {
			t.Fatalf("send %s: %d %s %s", to, code, out, e)
		}
	}

	session(t, "mac/claude.aaaa", "repo=alpha,tool=claude")
	_, out, _ := run(t, "", "inbox")
	for _, want := range []string{"for @repo=alpha", "for @tool=claude", "for @repo=*"} {
		if !strings.Contains(out, want) {
			t.Errorf("alpha/claude should receive %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"for @repo=beta,tool=codex", "for @repo=gamma"} {
		if strings.Contains(out, not) {
			t.Errorf("alpha/claude must not receive %q:\n%s", not, out)
		}
	}
	session(t, "air/codex.bbbb", "repo=beta,tool=codex")
	_, out, _ = run(t, "", "inbox")
	if !strings.Contains(out, "for @repo=beta,tool=codex") || !strings.Contains(out, "for @repo=*") || strings.Contains(out, "for @repo=alpha") {
		t.Errorf("beta/codex got:\n%s", out)
	}
}

func TestSendRejectsABadSelectorBeforeAnythingIsSent(t *testing.T) {
	mesh(t)
	session(t, "a/b", "")
	for _, to := range []string{"@", "@repo", "@bad key=x", "@repo=a;b"} {
		if code, _, e := run(t, "", "send", "--to", to, "hi"); code != exitUsage || !strings.Contains(e, "bad target") {
			t.Errorf("%q: %d %q", to, code, e)
		}
	}
	// and nothing reached the mesh
	session(t, "c/d", "")
	if _, out, _ := run(t, "", "inbox"); !strings.Contains(out, "(no unread messages)") {
		t.Errorf("a rejected send leaked a message:\n%s", out)
	}
}

func TestWhoShowsLabelsAndPreviewsASelector(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	session(t, "mac/claude.aaaa", "repo=alpha")
	run(t, "", "inbox")
	session(t, "air/codex.bbbb", "repo=beta")
	run(t, "", "inbox")
	session(t, "old/agent", "")
	t.Setenv("AIVOICEBUS_LABELS", "")

	_, out, _ := run(t, "", "who")
	if !strings.Contains(out, "mac/claude.aaaa") || !strings.Contains(out, "repo=alpha") || !strings.Contains(out, "repo=beta") {
		t.Errorf("who should show each session's labels:\n%s", out)
	}
	_, out, _ = run(t, "", "who", "@repo=alpha")
	if !strings.Contains(out, "mac/claude.aaaa") || strings.Contains(out, "air/codex.bbbb") {
		t.Errorf("who @repo=alpha:\n%s", out)
	}
	if _, out, _ = run(t, "", "who", "@repo=nobody"); !strings.Contains(out, "(no session this node knows of matches @repo=nobody)") {
		t.Errorf("an empty preview should say so:\n%s", out)
	}
	if code, _, e := run(t, "", "who", "@bad key=1"); code != 1 || !strings.Contains(e, "bad selector") {
		t.Errorf("bad selector: %d %q", code, e)
	}
}

func TestWhoamiLabels(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	session(t, "laptop-a/claude.a1b2", "repo=alpha,role=reviewer")
	code, out, _ := run(t, "", "whoami", "--labels")
	if code != 0 || strings.TrimSpace(out) != "machine=laptop-a,repo=alpha,role=reviewer,tool=claude" {
		t.Errorf("%d %q", code, out)
	}
}

func TestWhoFallsBackOnANodeWithoutARoster(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	inner := server.NewHandler(s, "tok")
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/roster" { // a node without a roster
			http.NotFound(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer old.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", old.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	session(t, "mac/claude.aaaa", "")
	run(t, "", "inbox")

	code, out, e := run(t, "", "who")
	if code != 0 || !strings.Contains(out, "mac/claude.aaaa") {
		t.Fatalf("who on an old node: %d %q %q", code, out, e)
	}
	if code, _, e = run(t, "", "who", "@repo=x"); code != 1 || !strings.Contains(e, "does not report labels") {
		t.Errorf("a selector preview needs a new node: %d %q", code, e)
	}
}

// staleMesh starts a node whose clock the test controls, with a session that
// declared itself three days ago and another just now.
func staleMesh(t *testing.T) (set func(time.Time), srvURL string) {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	var nanos atomic.Int64
	set = func(tm time.Time) { nanos.Store(tm.UnixNano()) }
	s.SetClock(func() time.Time { return time.Unix(0, nanos.Load()).UTC() })
	srv := httptest.NewServer(server.NewHandler(s, "tok"))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Chdir(t.TempDir())
	set(time.Now().Add(-3 * 24 * time.Hour))
	session(t, "old/claude.aaaa", "repo=alpha")
	run(t, "", "inbox")
	set(time.Now())
	session(t, "new/claude.bbbb", "repo=alpha")
	run(t, "", "inbox")
	return set, srv.URL
}

func TestWhoHidesStaleSessionsAndSaysHowManyAreHidden(t *testing.T) {
	staleMesh(t)
	_, out, _ := run(t, "", "who")
	if !strings.Contains(out, "new/claude.bbbb") || strings.Contains(out, "old/claude.aaaa") {
		t.Errorf("default: only the recent session:\n%s", out)
	}
	if !strings.Contains(out, "(1 session(s) not seen in the last 24h hidden; --all shows them)") {
		t.Errorf("the output must say what it hid:\n%s", out)
	}
	for _, args := range [][]string{{"who", "--all"}, {"who", "--within", "5d"}, {"who", "--within=96h"}} {
		_, out, _ = run(t, "", args...)
		if !strings.Contains(out, "old/claude.aaaa") || !strings.Contains(out, "new/claude.bbbb") || strings.Contains(out, "hidden") {
			t.Errorf("%v should list both and hide nothing:\n%s", args, out)
		}
	}
	if code, _, e := run(t, "", "who", "--within", "soon"); code != exitUsage || !strings.Contains(e, "--within:") {
		t.Errorf("bad --within: %d %q", code, e)
	}
}

func TestWhoPreviewRespectsTheWindowAndSaysSo(t *testing.T) {
	set, _ := staleMesh(t)
	// a session that matched only a different repo, last heard three days ago
	set(time.Now().Add(-3 * 24 * time.Hour))
	session(t, "ghost/claude.cccc", "repo=ghost")
	run(t, "", "inbox")
	set(time.Now())
	session(t, "other/agent", "")

	_, out, _ := run(t, "", "who", "@repo=alpha")
	if !strings.Contains(out, "new/claude.bbbb") || strings.Contains(out, "old/claude.aaaa") || !strings.Contains(out, "(1 session(s) not seen in the last 24h hidden") {
		t.Errorf("preview with a window:\n%s", out)
	}
	// the only match is stale: say that, instead of claiming nobody matches
	_, out, _ = run(t, "", "who", "@repo=ghost")
	if !strings.Contains(out, "no session seen in the last 24h matches @repo=ghost; 1 older one(s) hidden, --all shows them") {
		t.Errorf("stale-only match:\n%s", out)
	}
	if _, out, _ = run(t, "", "who", "--all", "@repo=ghost"); !strings.Contains(out, "ghost/claude.cccc") {
		t.Errorf("--all shows the stale match:\n%s", out)
	}
	// nothing matches at all
	if _, out, _ = run(t, "", "who", "@repo=nobody"); !strings.Contains(out, "(no session this node knows of matches @repo=nobody)") {
		t.Errorf("no match at all:\n%s", out)
	}
}

func TestWhoWindowAlsoAppliesOnANodeWithoutARoster(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	var nanos atomic.Int64
	nanos.Store(time.Now().Add(-3 * 24 * time.Hour).UnixNano())
	s.SetClock(func() time.Time { return time.Unix(0, nanos.Load()).UTC() })
	inner := server.NewHandler(s, "tok")
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/roster" {
			http.NotFound(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer old.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", old.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	session(t, "old/agent", "")
	run(t, "", "inbox")
	nanos.Store(time.Now().UnixNano())
	session(t, "new/agent", "")
	run(t, "", "inbox")

	_, out, _ := run(t, "", "who")
	if !strings.Contains(out, "new/agent") || strings.Contains(out, "old/agent") || !strings.Contains(out, "1 session(s) not seen") {
		t.Errorf("fallback path:\n%s", out)
	}
}

func TestWhoamiWhy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "claude.a1b2")
	code, out, _ := run(t, "", "whoami", "--why")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || !strings.HasSuffix(lines[0], "/claude.a1b2") || !strings.Contains(lines[1], "tool claude") {
		t.Errorf("%d %q", code, out)
	}
	// plain whoami stays one line: scripts read it
	if _, out, _ := run(t, "", "whoami"); strings.Count(out, "\n") != 1 {
		t.Errorf("whoami printed %q", out)
	}
}

func TestDoctorShowsTheDetectedTool(t *testing.T) {
	setUpMachine(t, "http://127.0.0.1:1", goodToken)
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "codex.q1w2")
	_, out, _ := run(t, "", "doctor", "--offline")
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "identity") {
			line = l
		}
	}
	if !strings.Contains(line, "/codex.q1w2") || !strings.Contains(line, "tool codex") {
		t.Errorf("identity line: %q\n%s", line, out)
	}
}

// With no tool above it, `aivoicebus mcp` is mcp.<4 chars> until the client says who it is.
func TestMcpIdentityFollowsClientInfo(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_DETECT", "") // on: the other tests of this package turn it off
	if identity.Implicit().Tool != "" {
		t.Skip("this test runs under an agent tool, which detection (correctly) finds")
	}
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "")
	for _, c := range []struct{ client, wantPrefix string }{
		{"codex-mcp-client", "codex."},
		{"gemini-cli-mcp-client", "gemini."},
		{"some-inspector", "mcp."},
	} {
		t.Setenv("AIVOICEBUS_AGENT", "")
		in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"` + c.client + `"}}}` + "\n"
		if code, _, e := run(t, in, "mcp"); code != 0 {
			t.Fatalf("%s: code %d %s", c.client, code, e)
		}
		got := os.Getenv("AIVOICEBUS_AGENT")
		if !strings.HasPrefix(got, c.wantPrefix) || len(got) != len(c.wantPrefix)+4 {
			t.Errorf("%s: agent %q, want %s<4 chars>", c.client, got, c.wantPrefix)
		}
	}
	// an explicit identity is left alone
	t.Setenv("AIVOICEBUS_AGENT", "mine.1")
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"codex-mcp-client"}}}` + "\n"
	run(t, in, "mcp")
	if got := os.Getenv("AIVOICEBUS_AGENT"); got != "mine.1" {
		t.Errorf("explicit agent overwritten: %q", got)
	}
}

func TestMcpDetectOffDoesNotRenameAndHookIdentityIsInherited(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "")
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"codex-mcp-client"}}}` + "\n"
	t.Setenv("AIVOICEBUS_DETECT", "off")
	t.Setenv("AIVOICEBUS_AGENT", "")
	run(t, in, "mcp")
	if got := os.Getenv("AIVOICEBUS_AGENT"); !strings.HasPrefix(got, "mcp.") {
		t.Errorf("clientInfo renamed the identity with detection off: %q", got)
	}
	// the hook's identity reaches the MCP server and is kept
	t.Setenv("AIVOICEBUS_AGENT", "claude.ab12")
	t.Setenv("AIVOICEBUS_AGENT_SRC", "hook")
	run(t, in, "mcp")
	if got := os.Getenv("AIVOICEBUS_AGENT"); got != "claude.ab12" {
		t.Errorf("hook identity lost: %q", got)
	}
}

func TestWhyAndDoctorDoNotEchoControlCharacters(t *testing.T) {
	setUpMachine(t, "http://127.0.0.1:1", goodToken)
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "x\x1b[2Jy")
	_, out, _ := run(t, "", "whoami", "--why")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 2 || strings.Contains(lines[1], "\x1b") {
		t.Errorf("why line: %q", out)
	}
	_, out, _ = run(t, "", "doctor", "--offline")
	if strings.Contains(out, "\x1b") {
		t.Errorf("doctor echoed an escape character: %q", out)
	}
}

// A tool started from a Claude Bash command inherits the hook's identity variables; the MCP
// server it runs must not be called claude.xxxx.
func TestMcpOfAnotherToolDoesNotInheritTheHooksIdentity(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_DETECT", "")
	if identity.Implicit().Tool != "" {
		t.Skip("this test runs under an agent tool, whose process the detector finds first")
	}
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "claude.ab12")
	t.Setenv("AIVOICEBUS_AGENT_SRC", "hook")
	t.Setenv("CODEX_THREAD_ID", "019a-thread")
	run(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n", "mcp")
	if got := os.Getenv("AIVOICEBUS_AGENT"); !strings.HasPrefix(got, "codex.") {
		t.Errorf("agent %q, want codex.<hash>", got)
	}
}

func TestWhoToolsGroupsSessionsByToolAndRepo(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	for _, s := range [][2]string{
		{"mac/claude.aaaa", "repo=zeta,tool=claude"},
		{"mac/claude.bbbb", "repo=zeta,tool=claude"},
		{"air/claude.cccc", "repo=alpha,tool=claude"},
		{"air/codex.dddd", "tool=codex"},
	} {
		session(t, s[0], s[1])
		run(t, "", "inbox")
	}
	_, out, _ := run(t, "", "who", "--tools")
	for _, want := range []string{"claude (3)", "codex (1)", "zeta", "(no repo)", "mac/claude.aaaa, mac/claude.bbbb"} {
		if !strings.Contains(out, want) {
			t.Errorf("who --tools lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "last seen") {
		t.Errorf("--tools groups instead of listing:\n%s", out)
	}
	if strings.Index(out, "zeta") > strings.Index(out, "alpha") {
		t.Errorf("the busier repo comes first:\n%s", out)
	}
	_, out, _ = run(t, "", "who", "--tools", "@repo=alpha")
	if strings.Contains(out, "zeta") || !strings.Contains(out, "claude (1)") {
		t.Errorf("a selector narrows the groups:\n%s", out)
	}
}

func TestPrintToolGroupsShowsCapsAndSanitisesEveryPeerString(t *testing.T) {
	var b strings.Builder
	printToolGroups(&b, []msg.RosterEntry{
		{Me: "h/claude.a", Labels: msg.Labels{"tool": "cl\x1b[2Jaude", "repo": "re\x1b[2Jpo", "caps": "ask+run"}},
		{Me: "h/claude.b", Labels: msg.Labels{"tool": "cl\x1b[2Jaude", "repo": "re\x1b[2Jpo"}},
	})
	out := b.String()
	if strings.Contains(out, "\x1b") || !strings.Contains(out, "h/claude.a [ask+run]") || !strings.Contains(out, "h/claude.b") || strings.Contains(out, "h/claude.b [") {
		t.Errorf("%q", out)
	}
}

func TestPrintToolGroupsNamesAreSanitisedAndCapped(t *testing.T) {
	var rows []msg.RosterEntry
	for i := 0; i < 8; i++ {
		rows = append(rows, msg.RosterEntry{Me: fmt.Sprintf("h/claude.%d\x1b[2J", i), Labels: msg.Labels{"tool": "claude", "repo": "r"}})
	}
	var b strings.Builder
	printToolGroups(&b, rows)
	if strings.Contains(b.String(), "\x1b") || !strings.Contains(b.String(), ", +3 more") {
		t.Errorf("%q", b.String())
	}
	b.Reset()
	printToolGroups(&b, []msg.RosterEntry{{Me: "old/codex.x"}, {Me: "weird"}}) // an old node reports no labels
	if !strings.Contains(b.String(), "codex (1)") || !strings.Contains(b.String(), "unknown (1)") {
		t.Errorf("%q", b.String())
	}
}

func TestSendersRouteByAdvertisedCapabilities(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	t.Setenv("AIVOICEBUS_CAPS", "ask,run")
	session(t, "mac/claude.aaaa", "repo=alpha")
	run(t, "", "inbox")
	t.Setenv("AIVOICEBUS_CAPS", "read-only")
	session(t, "mac/claude.bbbb", "repo=alpha")
	run(t, "", "inbox")
	t.Setenv("AIVOICEBUS_CAPS", "")

	session(t, "other/agent", "")
	if _, out, _ := run(t, "", "who", "@can=run"); !strings.Contains(out, "claude.aaaa") || strings.Contains(out, "claude.bbbb") {
		t.Errorf("who @can=run:\n%s", out)
	}
	if code, out, e := run(t, "", "send", "--to", "@repo=alpha,can=run", "run the tests"); code != 0 {
		t.Fatalf("%d %s %s", code, out, e)
	}
	t.Setenv("AIVOICEBUS_CAPS", "ask,run")
	session(t, "mac/claude.aaaa", "repo=alpha")
	if _, out, _ := run(t, "", "inbox"); !strings.Contains(out, "run the tests") {
		t.Errorf("the session that can run got it:\n%s", out)
	}
	t.Setenv("AIVOICEBUS_CAPS", "read-only")
	session(t, "mac/claude.bbbb", "repo=alpha")
	if _, out, _ := run(t, "", "inbox"); strings.Contains(out, "run the tests") {
		t.Errorf("the read-only session must not get it:\n%s", out)
	}
	if code, _, e := run(t, "", "send", "--to", "@can= x", "x"); code != 1 || !strings.Contains(e, `"can= x"`) {
		t.Errorf("a malformed can term quotes what was typed: %d %q", code, e)
	}
	if code, _, e := run(t, "", "send", "--to", "@can=nothing-like-this", "x"); code != 1 || !strings.Contains(e, "no session seen") {
		t.Errorf("a capability nobody advertises reaches no one: %d %q", code, e)
	}
}

func TestMcpStartLearnsTheNodeListWithoutAHook(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	old := trustedKeys
	trustedKeys = func() []ed25519.PublicKey { return []ed25519.PublicKey{pub} }
	t.Cleanup(func() { trustedKeys = old })
	dir := t.TempDir()
	s, _ := store.Open(t.TempDir(), "n")
	srv := httptest.NewServer(server.NewHandler(s, "tok", server.WithReleaseDir(dir)))
	t.Cleanup(srv.Close)
	extra := "http://100.64.0.12:7777"
	if _, err := release.PackNodes(release.PackNodesOptions{Dir: dir, Serial: 9, Key: priv,
		Nodes: []release.NodeEntry{{Name: "n", URL: srv.URL}, {Name: "other", URL: extra}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_ME", "air/codex.zzzz")
	if code, _, e := run(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n", "mcp"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if urls := config.URLs(); len(urls) != 2 || urls[1] != extra {
		t.Errorf("the MCP server learned the signed list at start: %v", urls)
	}
}

func TestACapabilitySendNamesItsRecipientsAndWarnsWhenFewAdvertiseIt(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	t.Setenv("AIVOICEBUS_CAPS", "ask,run")
	session(t, "mac/claude.aaaa", "repo=alpha")
	run(t, "", "inbox")
	session(t, "air/codex.bbbb", "repo=alpha")
	run(t, "", "inbox")
	t.Setenv("AIVOICEBUS_CAPS", "dry-run") // not "run"
	session(t, "box/gemini.cccc", "repo=alpha")
	run(t, "", "inbox")
	t.Setenv("AIVOICEBUS_CAPS", "")
	session(t, "other/agent", "")

	_, out, _ := run(t, "", "send", "--dry-run", "--to", "@can=run", "run the tests")
	for _, want := range []string{"air/codex.bbbb", "mac/claude.aaaa", "self-declared", "only 2 session(s) advertise"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gemini.cccc") {
		t.Errorf("dry-run is not run:\n%s", out)
	}
	_, out, e := run(t, "", "send", "--to", "@can=run", "run the tests")
	if !strings.Contains(e, "air/codex.bbbb, mac/claude.aaaa") || !strings.Contains(e, "only 2 session(s)") {
		t.Errorf("the note after sending names who it went to:\n%s%s", out, e)
	}
	// a plain selector keeps its old, quiet output
	_, out, e = run(t, "", "send", "--to", "@repo=alpha", "hello")
	if strings.Contains(out+e, "self-declared") {
		t.Errorf("only a capability send carries the capability note:\n%s%s", out, e)
	}
	// three sessions: names, but no "only a few" warning
	t.Setenv("AIVOICEBUS_CAPS", "run")
	session(t, "box/gemini.cccc", "repo=alpha")
	run(t, "", "inbox")
	t.Setenv("AIVOICEBUS_CAPS", "")
	session(t, "other/agent", "")
	_, out, _ = run(t, "", "send", "--dry-run", "--to", "@can=run", "run the tests")
	if !strings.Contains(out, "gemini.cccc") || strings.Contains(out, "only 3 session") {
		t.Errorf("three recipients: named, not warned:\n%s", out)
	}
}

func TestWhoDoesNotEchoControlCharactersFromARoster(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	inner := server.NewHandler(s, "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/roster" {
			w.Write([]byte(`[{"me":"h/claude.a\u001b[2Jb","seen":"2030-01-01T00:00:00Z","labels":{"repo":"x\u001b[2Jy"}}]`))
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	session(t, "me/agent", "")
	_, out, _ := run(t, "", "who", "--all")
	if !strings.Contains(out, "h/claude.a") || strings.Contains(out, "\x1b") {
		t.Errorf("%q", out)
	}
}

func TestMcpStartWithHungNodesDoesNotWaitForThem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_ME", "air/codex.zzzz")
	start := time.Now()
	run(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n", "mcp")
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the MCP server waited %s for hung nodes at start", d)
	}
}

func TestCapabilityRecipientsAreNamedInSortedOrderWhateverOrderTheNodeReports(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	inner := server.NewHandler(s, "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/roster" {
			now := time.Now().UTC().Format(time.RFC3339)
			w.Write([]byte(`[{"me":"z/claude.1","seen":"` + now + `","labels":{"caps":"run"}},{"me":"a/codex.2","seen":"` + now + `","labels":{"caps":"run"}}]`))
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	session(t, "me/agent", "")
	_, out, _ := run(t, "", "send", "--dry-run", "--to", "@can=run", "go")
	if !strings.Contains(out, "a/codex.2, z/claude.1") {
		t.Errorf("%s", out)
	}
}

func TestARoleAliasIsPinnedThenResolvedByWhoAndSendEndToEnd(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	session(t, "build-a/claude.b1", "role=build,repo=alpha")
	run(t, "", "inbox") // declares the role

	session(t, "me/claude.0001", "repo=beta")
	// unpinned: explained, nothing sent
	if code, _, e := run(t, "", "send", "--to", "@build", "ready?"); code != 1 || !strings.Contains(e, "role pin build 'build-a/claude.*'") {
		t.Fatalf("unpinned: %d %q", code, e)
	}
	if code, _, e := run(t, "", "role", "pin", "build", "*"); code != exitUsage || !strings.Contains(e, "host") {
		t.Errorf("a pin of * is refused: %d %q", code, e)
	}
	if code, out, e := run(t, "", "role", "pin", "build", "build-*/claude.*"); code != 0 || !strings.Contains(out, "pinned") {
		t.Fatalf("pin: %d %q %q", code, out, e)
	}
	if code, out, _ := run(t, "", "role"); code != 0 || strings.TrimSpace(out) != "@build -> build-*/claude.*" {
		t.Errorf("list: %d %q", code, out)
	}
	code, out, _ := run(t, "", "who", "@build")
	if code != 0 || !strings.Contains(out, "build-a/claude.b1") || !strings.Contains(out, "@build resolved to build-a/claude.b1") {
		t.Errorf("who: %d %q", code, out)
	}
	code, out, e := run(t, "", "send", "--to", "@build", "ready?")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || lines[0] != "resolved @build -> build-a/claude.b1 (matches your pin pattern; identities are self-declared)" || !strings.HasPrefix(lines[1], "sent ") {
		t.Fatalf("send prints who it resolved to before it sends, on stdout: %d %q %q", code, out, e)
	}
	session(t, "build-a/claude.b1", "role=build,repo=alpha")
	if _, out, _ := run(t, "", "inbox"); !strings.Contains(out, "ready?") || !strings.Contains(out, "me/claude.0001 -> build-a/claude.b1") {
		t.Errorf("the holder got it, addressed to its identity:\n%s", out)
	}
	session(t, "me/claude.0001", "repo=beta")
	if code, _, e := run(t, "", "send", "--to", "@deploy", "x"); code != 1 || !strings.Contains(e, "no session claims @deploy") {
		t.Errorf("%d %q", code, e)
	}
	if code, out, _ := run(t, "", "role", "unpin", "build"); code != 0 || !strings.Contains(out, "--trust-role") {
		t.Errorf("unpin: %d %q", code, out)
	}
	if code, _, e := run(t, "", "send", "--to", "@build", "--trust-role", "again"); code != 0 {
		t.Errorf("--trust-role: %d %q", code, e)
	}
	if code, _, e := run(t, "", "send", "--to", "@build", "--trust-role", "--prio", "urgent", "again"); code != 1 || !strings.Contains(e, "never used for an urgent") {
		t.Errorf("urgent: %d %q", code, e)
	}
}
