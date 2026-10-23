package cli

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// The pieces 0.9 added to send meet here: a pinned role, capability routing, a reply whose target
// is inferred, and --json. Whom a message goes to is always said before it is sent, and stdout
// stays one document under --json.
func TestSendCombinesRolesCapsReplyAndJSONAndAlwaysSaysWhoItGoesTo(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	session(t, "build-a/claude.b1", "role=build,repo=alpha,caps=run")
	run(t, "", "inbox") // declares the role, the repo and the capability
	if code, _, e := run(t, "", "send", "--to", "me/claude.0001", "--queue", "ping"); code != 0 {
		t.Fatalf("ping: %d %q", code, e)
	}

	session(t, "me/claude.0001", "repo=beta")
	run(t, "", "role", "pin", "build", "build-*/claude.*")

	// an inferred reply names whom it answers on stderr before anything is sent, once
	code, out, e := run(t, "", "reply", "--json", "pong")
	var sent struct{ ID, Thread, To, Re string }
	if err := json.Unmarshal([]byte(out), &sent); code != 0 || err != nil || sent.To != "build-a/claude.b1" || sent.Re == "" {
		t.Fatalf("reply --json: %d %q %q (%v)", code, out, e, err)
	}
	if !strings.Contains(e, "replying to "+sent.Re+" from build-a/claude.b1") || strings.Contains(e, "no --to") {
		t.Errorf("an inferred reply says whom it answers, once: %q", e)
	}

	// an explicit --re with no --to takes the target from that message: it says so too
	code, out, e = run(t, "", "send", "--re", sent.Re, "again")
	if code != 0 || !strings.HasPrefix(out, "sent ") || !strings.Contains(e, "no --to: answering build-a/claude.b1, the other side of message "+sent.Re) {
		t.Errorf("--re without --to: %d %q %q", code, out, e)
	}

	// a role alias under --json: stdout is one document, the resolution is on stderr
	code, out, e = run(t, "", "send", "--json", "--to", "@build", "ready?")
	var viaRole struct{ ID, To string }
	if err := json.Unmarshal([]byte(out), &viaRole); code != 0 || err != nil || viaRole.To != "build-a/claude.b1" {
		t.Fatalf("send --json to a role: %d %q %q (%v)", code, out, e, err)
	}
	if !strings.Contains(e, "resolved @build -> build-a/claude.b1 (matches your pin pattern; identities are self-declared)") {
		t.Errorf("the role resolution is printed even under --json: %q", e)
	}

	// capability routing names the sessions it resolved to, whatever else is on the line
	code, out, e = run(t, "", "send", "--to", "@repo=alpha,can=run", "go")
	if code != 0 || !strings.Contains(e, "routed by capability") || !strings.Contains(e, "build-a/claude.b1") {
		t.Errorf("can= send: %d %q %q", code, out, e)
	}

	// a dry run under --json is one document and sends nothing
	code, out, e = run(t, "", "send", "--dry-run", "--json", "--to", "@build", "x")
	var plan map[string]any
	if err := json.Unmarshal([]byte(out), &plan); code != 0 || err != nil {
		t.Errorf("dry-run --json: %d %q %q (%v)", code, out, e, err)
	}

	// who --tools --json is the same list, one document, with the capabilities in the labels
	var who struct {
		Sessions []struct {
			Me     string
			Labels map[string]string
		}
	}
	runJSON(t, &who, "who", "--tools")
	found := false
	for _, s := range who.Sessions {
		if s.Me == "build-a/claude.b1" {
			found = s.Labels["caps"] == "run"
		}
	}
	if !found {
		t.Errorf("who --tools --json: %+v", who)
	}
}

// `tail push --to @role` sends a log link: an unpinned role (anyone can claim one) never gets it,
// even when the machine trusts roles, and a pinned one is named on stderr before the link goes.
func TestTailPushToARoleNeedsAPinAndSaysWhoItResolvedTo(t *testing.T) {
	mesh(t)
	services(t)
	t.Setenv("AIVOICEBUS_LABELS", "role=build")
	present(t, "build-a/codex.b1")
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Setenv("AIVOICEBUS_ME", "imac/claude")
	t.Setenv("AIVOICEBUS_TRUST_ROLES", "1")
	if code, _, e := run(t, "line\n", "tail", "push", "--to", "@build", "following"); code == 0 || !strings.Contains(e, "never used for an urgent message or a file or log link") {
		t.Errorf("unpinned: %d %q", code, e)
	}
	if code, _, e := run(t, "", "role", "pin", "build", "build-*/codex.*"); code != 0 {
		t.Fatal(e)
	}
	code, out, e := run(t, "line\n", "tail", "push", "--to", "@build", "following")
	if code != 0 || !strings.Contains(e, "resolved @build -> build-a/codex.b1 (matches your pin pattern; identities are self-declared)") || strings.Contains(out, "resolved @build") {
		t.Errorf("pinned: %d out %q err %q", code, out, e)
	}
	if i, j := strings.Index(e, "resolved @build"), strings.Index(e, "sent "); i < 0 || j < i {
		t.Errorf("the resolution comes before the send: %q", e)
	}
}

// Two probes in a row must not be the same message: a node before 0.7 suppresses a repeat within ten
// minutes and answers with the first one's (already acknowledged) id.
func TestProbeBodiesDifferAndKeepTheVisiblePrefix(t *testing.T) {
	a, b := newProbeBody(), newProbeBody()
	if a == b || !strings.HasPrefix(a, "aivoicebus probe ") || !strings.HasPrefix(b, probeBodyPrefix) {
		t.Errorf("%q %q", a, b)
	}
}

func TestJoinSendsAtMostSixProbesAnHourPerMachine(t *testing.T) {
	st, _ := store.Open(t.TempDir(), "n1")
	ts := &server.TokenSet{}
	ts.Add(machineToken, server.Principal{Name: "machine", Host: testHost})
	srv := httptest.NewServer(server.NewHandler(st, goodToken, server.WithTokenSet(ts)))
	defer srv.Close()
	bareMachine(t)
	trustNode(t, srv.URL)
	var runs strings.Builder
	for i := 0; i < selftestMaxRuns; i++ {
		runs.WriteString(strconv.FormatInt(time.Now().Add(-time.Duration(i)*time.Minute).Unix(), 10) + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(joinRunsPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(joinRunsPath(), []byte(runs.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, machineToken+"\n", "join", "--url", srv.URL)
	if code != 1 || !strings.Contains(out, "already ran") || st.Stats().Messages != 0 {
		t.Errorf("the seventh probe in an hour is refused before anything is sent: %d\n%s", code, out)
	}
}

func TestHelpPrintsRealUsageForTheCommandsWithoutFlags(t *testing.T) {
	mesh(t)
	for _, cmd := range []string{"name", "role", "ack"} {
		code, out, e := run(t, "", cmd, "-h")
		if code != 0 || !strings.Contains(out, "usage: "+cmd) || strings.Contains(e, "invalid") {
			t.Errorf("%s -h: %d %q %q", cmd, code, out, e)
		}
	}
}

// A session that names itself like a probe vanishes from who, but a send still counts it: the
// reach count (and with it the one-recipient rule for a file) is a decision, not a display.
func TestASessionNamedLikeAProbeIsHiddenFromWhoButCountedByReach(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	session(t, "box/join-reader", "repo=alpha")
	run(t, "", "inbox")
	session(t, "me/claude.0001", "repo=beta")
	if _, out, _ := run(t, "", "who"); strings.Contains(out, "join-reader") {
		t.Errorf("who is a display: %s", out)
	}
	code, out, e := run(t, "", "send", "--dry-run", "--to", "@repo=alpha", "x")
	if code != 0 || !strings.Contains(out, "reaches   1 session(s)") {
		t.Errorf("the dry run counts it: %d %q %q", code, out, e)
	}
}

// run2 is run with one stream for both stdout and stderr, so the order of what is printed shows.
func run2(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var b strings.Builder
	code := Run(args, Streams{In: strings.NewReader(""), Out: &b, Err: &b})
	return code, b.String()
}

func TestCapabilityRoutesAreShownBeforeTheSendAndNeverGetUrgentOrAttachments(t *testing.T) {
	mesh(t)
	services(t)
	t.Chdir(t.TempDir())
	session(t, "build-a/claude.b1", "repo=alpha,caps=run")
	run(t, "", "inbox")
	session(t, "me/claude.0001", "repo=beta")
	code, out := run2(t, "send", "--to", "@repo=alpha,can=run", "go")
	i, j := strings.Index(out, "routed by capability"), strings.Index(out, "sent ")
	if code != 0 || i < 0 || j < 0 || i > j {
		t.Errorf("the recipients come before the send: %d %q", code, out)
	}
	file := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(file, []byte("hello"), 0o600)
	for _, args := range [][]string{
		{"send", "--to", "@can=run", "--prio", "urgent", "x"},
		{"send", "--to", "@can=run", "--file", file, "x"},
		{"send", "--to", "@can=run", "--tail", "new", "x"},
	} {
		if code, out := run2(t, args...); code == 0 || !strings.Contains(out, "routes by capability") {
			t.Errorf("%v: %d %q", args, code, out)
		}
	}
	if code, _, e := run(t, "line\n", "tail", "push", "--to", "@can=run", "following"); code == 0 || !strings.Contains(e, "routes by capability") {
		t.Errorf("tail push: %d %q", code, e)
	}
}

func TestAnAbbreviatedReIsRefusedForUrgentAndAttachmentsButResolvedFirstOtherwise(t *testing.T) {
	mesh(t)
	services(t)
	t.Chdir(t.TempDir())
	session(t, "build-a/claude.b1", "")
	id := sendID(t, "build-a/claude.b1", "me/claude.0001", "question?")
	session(t, "me/claude.0001", "")
	file := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(file, []byte("hello"), 0o600)
	for _, args := range [][]string{
		{"send", "--re", id[:5], "--prio", "urgent", "x"},
		{"send", "--re", id[:5], "--file", file, "x"},
	} {
		if code, out := run2(t, args...); code == 0 || !strings.Contains(out, "abbreviation") {
			t.Errorf("%v: %d %q", args, code, out)
		}
	}
	code, out := run2(t, "send", "--re", id[:5], "plain")
	if code != 0 || !strings.Contains(out, "resolved "+id[:5]+" -> "+id) {
		t.Errorf("a plain send prints the resolution first: %d %q", code, out)
	}
	if code, out := run2(t, "send", "--re", id, "--prio", "urgent", "x"); code != 0 {
		t.Errorf("a full id is fine: %d %q", code, out)
	}
}

func TestPlanViewAndSentViewShowAPeersIdentityWithoutRawControls(t *testing.T) {
	b, _ := json.Marshal(planView(outbox.Plan{Msg: msg.Message{From: "a/b", To: "x\u0085/c\u202ed"}}, ""))
	if strings.Contains(string(b), "\u0085") || strings.Contains(string(b), "\u202e") || strings.Contains(string(b), `\u0085`) || strings.Contains(string(b), ``+"\u202e"+``) {
		t.Errorf("%s", b)
	}
}

func TestTopLevelErrorsAndJSONFailuresCarryNoRawControls(t *testing.T) {
	mesh(t)
	for _, args := range [][]string{{"frob\u0085\x1b[2Jnicate"}, {"frob\u0085nicate", "--json"}} {
		code, out, e := run(t, "", args...)
		if code == 0 || strings.ContainsAny(out+e, "\u0085\x1b") {
			t.Errorf("%q: %d %q %q", args, code, out, e)
		}
	}
}

// The node list is written to a file the shell sources: nothing the shell could run may get there,
// and the value is single-quoted; the client reads the quoted form back.
func TestNodeURLsAreRestrictedAndQuotedInTheEnvFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_URL", "")
	t.Setenv("AIVOICEBUS_TOKEN", "")
	envPath := filepath.Join(home, ".config", "aivoicebus", "env")
	for _, bad := range []string{"http://u:$(id)@127.0.0.1:7777", "http://127.0.0.1:7777;id", "http://h`id`:1", "http://a b:1", "http://h:1'x"} {
		code, out, e := run(t, "", "setup", "--yes", "--tools", "claude", "--url", bad)
		if b, _ := os.ReadFile(envPath); strings.Contains(string(b), "id") || code == 0 && !strings.Contains(out+e, "refused") {
			t.Errorf("%q: %d %q %q env %q", bad, code, out, e, b)
		}
		if code, _, e := run(t, "", "join", "--dry-run", "--url", bad); code == 0 {
			t.Errorf("join accepted %q: %q", bad, e)
		}
	}
	if code, out, e := run(t, "", "setup", "--yes", "--tools", "claude", "--url", "http://[::1]:7777,http://127.0.0.1:7778"); code != 0 {
		t.Fatalf("a good list: %d %q %q", code, out, e)
	}
	b, _ := os.ReadFile(envPath)
	if !strings.Contains(string(b), "export AIVOICEBUS_URL='http://[::1]:7777,http://127.0.0.1:7778'\n") {
		t.Errorf("the value is single-quoted: %q", b)
	}
	config.LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_URL"); got != "http://[::1]:7777,http://127.0.0.1:7778" {
		t.Errorf("the quoted value reads back: %q", got)
	}
}
