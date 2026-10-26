package hook

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
	"github.com/baditaflorin/aivoicebus/internal/version"
)

func TestShortSession(t *testing.T) {
	if got := shortSession("A1b2-C3d4-xyz"); got != "a1b2" {
		t.Fatal(got)
	}
	if shortSession("") != "" || shortSession("--") != "" {
		t.Fatal("empty expected")
	}
}

// mesh starts one node and configures the environment to use it as `me`.
func mesh(t *testing.T) (*store.Store, string) {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, "tok"))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir()) // no real ~/.config/aivoicebus: no saved node list, no update notice state
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_OFF", "")
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	t.Chdir(t.TempDir()) // no stray .aivoicebus-off
	return s, srv.URL
}

func runHook(t *testing.T, stdin string) string {
	t.Helper()
	var out strings.Builder
	var in io.Reader
	if stdin != "" {
		in = strings.NewReader(stdin)
	}
	Run([]string{"--tool", "codex"}, in, &out)
	return codexText(t, out.String())
}

// codexText is the text inside a Codex hook's JSON output ("" for no output).
func codexText(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var d struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil || d.HookSpecificOutput.HookEventName == "" {
		t.Fatalf("codex hook output is not its JSON: %v %q", err, out)
	}
	return d.HookSpecificOutput.AdditionalContext
}

func TestGivesSessionIdentityShowsAndAcks(t *testing.T) {
	s, _ := mesh(t)
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "") // Run sets it; t.Setenv restores it afterwards
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	s.Send(msg.Message{From: "other/agent", To: "all", Body: "deploy finished"})

	var out strings.Builder
	Run([]string{"--tool", "claude"}, strings.NewReader(`{"session_id":"A1B2-zzz"}`), &out)
	got := out.String()
	for _, want := range []string{"1 unread message(s)", "deploy finished", "UNTRUSTED DATA", "/claude.a1b2", "one-line summary"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if b, _ := os.ReadFile(envFile); !strings.Contains(string(b), "export AIVOICEBUS_AGENT=claude.a1b2") {
		t.Errorf("session identity not exported to CLAUDE_ENV_FILE: %q", b)
	}

	var again strings.Builder // the first run acked the message
	Run([]string{"--tool", "claude"}, nil, &again)
	if again.Len() != 0 {
		t.Errorf("nothing unread should print nothing, got %q", again.String())
	}
}

func TestSilentWhenNothingUnread(t *testing.T) {
	mesh(t)
	if got := runHook(t, ""); got != "" {
		t.Fatalf("expected silence, got %q", got)
	}
}

func TestSilentWhenMeshIsDownOrUnconfigured(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_URL", "")
	if got := runHook(t, ""); got != "" {
		t.Errorf("no URL configured: got %q", got)
	}
	t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:1") // nothing listens there
	if got := runHook(t, ""); got != "" {
		t.Errorf("mesh down: got %q", got)
	}
}

func TestOptOutByEnvAndByFile(t *testing.T) {
	s, _ := mesh(t)
	s.Send(msg.Message{From: "other/agent", To: "all", Body: "hello"})

	t.Setenv("AIVOICEBUS_OFF", "1")
	if got := runHook(t, ""); got != "" {
		t.Errorf("AIVOICEBUS_OFF: got %q", got)
	}
	t.Setenv("AIVOICEBUS_OFF", "")
	os.WriteFile(OptOutFile, nil, 0o644)
	if got := runHook(t, ""); got != "" {
		t.Errorf("%s: got %q", OptOutFile, got)
	}
	os.Remove(OptOutFile)
	if got := runHook(t, ""); !strings.Contains(got, "hello") {
		t.Errorf("without an opt-out the message should show, got %q", got)
	}
}

func TestCapsMessagesAndBodiesAndLeavesTheRestUnread(t *testing.T) {
	s, _ := mesh(t)
	for i := 1; i <= 7; i++ { // directed: broadcasts have a smaller cap of their own, see rank_test.go
		s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: fmt.Sprintf("msg-%d %s", i, strings.Repeat("x", 400))})
	}
	got := runHook(t, "")
	if n := strings.Count(got, "--- "); n != maxShown {
		t.Errorf("printed %d messages, want %d:\n%s", n, maxShown, got)
	}
	for _, want := range []string{"7 unread message(s)", "+2 more unread", "…cut; full text: aivoicebus thread"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, strings.Repeat("x", maxBody+1)) {
		t.Error("a body was not cut")
	}
	// only the shown ones were acked: the other two come next time
	if left := s.Inbox("air/codex", false); len(left) != 2 {
		t.Errorf("%d messages left unread, want 2", len(left))
	}
}

func TestGivesUpSilentlyAfterTheDeadline(t *testing.T) {
	mesh(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer slow.Close()
	t.Setenv("AIVOICEBUS_URL", slow.URL)
	old := Deadline
	Deadline = 150 * time.Millisecond
	defer func() { Deadline = old }()

	start := time.Now()
	got := runHook(t, "")
	if got != "" {
		t.Errorf("expected silence, got %q", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("hook took %v, the deadline is %v", d, Deadline)
	}
}

// releaseMesh is mesh() with a node that announces release `latest`, and this
// binary pretending to be release `running`, with a scratch HOME.
func releaseMesh(t *testing.T, running, latest string) *store.Store {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schema":1,"version":"`+latest+`","files":{}}`), 0o644)
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, "tok", server.WithReleaseDir(dir)))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_OFF", "")
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	t.Chdir(t.TempDir())
	old := version.Release
	version.Release = running
	t.Cleanup(func() { version.Release = old })
	return s
}

func TestNoticeWhenANewerReleaseIsAnnouncedThenQuietForADay(t *testing.T) {
	releaseMesh(t, "0.2.0", "0.3.0")
	got := runHook(t, "")
	for _, want := range []string{"Release 0.3.0 is available", "runs 0.2.0", "aivoicebus update", "OK"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q:\n%s", want, got)
		}
	}
	if again := runHook(t, ""); again != "" {
		t.Errorf("the same release must not be announced twice in a day, got %q", again)
	}
}

func TestNoNoticeWhenCurrentOrAhead(t *testing.T) {
	releaseMesh(t, "0.3.0", "0.3.0")
	if got := runHook(t, ""); got != "" {
		t.Errorf("current: %q", got)
	}
	version.Release = "0.4.0"
	if got := runHook(t, ""); got != "" {
		t.Errorf("ahead of the announced release: %q", got)
	}
}

func TestNoticeAndMessagesTogether(t *testing.T) {
	s := releaseMesh(t, "dev", "0.3.0")
	s.Send(msg.Message{From: "other/agent", To: "all", Body: "hello"})
	got := runHook(t, "")
	if !strings.Contains(got, "Release 0.3.0 is available") || !strings.Contains(got, "hello") {
		t.Errorf("both the notice and the message should show:\n%s", got)
	}
}

func TestShouldNotify(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		prev noticeRecord
		last string
		want bool
	}{
		{"never mentioned", noticeRecord{}, "0.3.0", true},
		{"mentioned an hour ago", noticeRecord{"0.3.0", now.Add(-time.Hour)}, "0.3.0", false},
		{"mentioned a day ago", noticeRecord{"0.3.0", now.Add(-24 * time.Hour)}, "0.3.0", true},
		{"a newer release since", noticeRecord{"0.3.0", now.Add(-time.Minute)}, "0.4.0", true},
	}
	for _, c := range cases {
		if got := shouldNotify(c.prev, c.last, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// gitCheckout makes a real repository with the given origin, for hook tests.
func gitCheckout(t *testing.T, origin string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"symbolic-ref", "HEAD", "refs/heads/main"}, {"remote", "add", "origin", origin}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestHookDeclaresLabelsFromTheSessionDirectoryExportsThemAndReceivesSelectorMessages(t *testing.T) {
	s, _ := mesh(t)
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_LABELS", "")
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	repo := gitCheckout(t, "git@github.com:example-org/Alpha-Repo.git")
	s.Send(msg.Message{From: "other/agent", To: "@repo=alpha-repo", Body: "for the alpha repo"})
	s.Send(msg.Message{From: "other/agent", To: "@repo=beta-repo", Body: "for the beta repo"})
	s.Send(msg.Message{From: "other/agent", To: "@repo=alpha-repo,tool=codex", Body: "for alpha codex only"})

	var out strings.Builder
	Run([]string{"--tool", "claude"}, strings.NewReader(`{"session_id":"A1B2-zzz","cwd":"`+repo+`"}`), &out)
	got := out.String()
	if !strings.Contains(got, "for the alpha repo") {
		t.Errorf("a session working in alpha-repo should receive its selector message:\n%s", got)
	}
	for _, not := range []string{"for the beta repo", "for alpha codex only"} {
		if strings.Contains(got, not) {
			t.Errorf("%q must not reach this session:\n%s", not, got)
		}
	}
	b, _ := os.ReadFile(envFile)
	exported := string(b)
	for _, want := range []string{"export AIVOICEBUS_AGENT=claude.a1b2", "export AIVOICEBUS_AGENT_SRC=hook", "export AIVOICEBUS_LABELS='", "repo=alpha-repo", "branch=main", "tool=claude"} {
		if !strings.Contains(exported, want) {
			t.Errorf("CLAUDE_ENV_FILE should contain %q:\n%s", want, exported)
		}
	}
}

func TestHookKeepsLabelsAlreadyInTheEnvironment(t *testing.T) {
	s, _ := mesh(t)
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	t.Setenv("AIVOICEBUS_LABELS", "repo=pinned")
	s.Send(msg.Message{From: "other/agent", To: "@repo=pinned", Body: "for pinned"})
	var out strings.Builder
	Run([]string{"--tool", "codex"}, nil, &out)
	if !strings.Contains(out.String(), "for pinned") {
		t.Errorf("the pinned label should route:\n%s", out.String())
	}
	if b, _ := os.ReadFile(envFile); strings.Contains(string(b), "AIVOICEBUS_LABELS") {
		t.Errorf("labels already set by the user must not be re-exported: %q", b)
	}
}

func TestUrgentFirstAndFYIOnlyCountedAndAcked(t *testing.T) {
	s, _ := mesh(t)
	s.Send(msg.Message{From: "o/a", To: "all", Body: "plain question"})
	s.Send(msg.Message{From: "o/a", To: "all", Body: "lunch at noon", Prio: "fyi"})
	s.Send(msg.Message{From: "o/a", To: "all", Body: "build is broken", Prio: "urgent"})
	s.Send(msg.Message{From: "o/a", To: "all", Body: "release notes drafted", Prio: "fyi"})

	got := runHook(t, "")
	if strings.Index(got, "build is broken") < 0 || strings.Index(got, "build is broken") > strings.Index(got, "plain question") {
		t.Errorf("urgent must come before ask:\n%s", got)
	}
	for _, want := range []string{"2 unread message(s)", "1 marked urgent: tell the user now", "[urgent]", "2 low-priority (fyi) message(s) arrived and were not shown", "aivoicebus inbox --all"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	for _, hidden := range []string{"lunch at noon", "release notes drafted"} {
		if strings.Contains(got, hidden) {
			t.Errorf("an fyi body must not be printed: %q\n%s", hidden, got)
		}
	}
	if left := s.Inbox("air/codex", false); len(left) != 0 {
		t.Errorf("shown and fyi messages are marked read, %d left unread", len(left))
	}
	if all := s.Inbox("air/codex", true); len(all) != 4 {
		t.Errorf("--all still lists everything, got %d", len(all))
	}
	if again := runHook(t, ""); again != "" {
		t.Errorf("nothing new: %q", again)
	}
}

func TestOnlyFYIPrintsJustTheCount(t *testing.T) {
	s, _ := mesh(t)
	s.Send(msg.Message{From: "o/a", To: "all", Body: "low value", Prio: "fyi"})
	got := runHook(t, "")
	if strings.TrimSpace(got) == "" || strings.Count(got, "\n") != 1 || !strings.Contains(got, "1 low-priority (fyi) message(s)") {
		t.Errorf("expected one short line, got:\n%s", got)
	}
	if strings.Contains(got, "UNTRUSTED") || strings.Contains(got, "You are") {
		t.Errorf("no banner or header for a bare count:\n%s", got)
	}
}

func TestExpiredMessagesNeverReachTheHook(t *testing.T) {
	s, _ := mesh(t)
	clock := time.Now().Add(-3 * time.Hour)
	s.SetClock(func() time.Time { return clock })
	s.Send(msg.Message{From: "o/a", To: "all", Body: "old news", TTL: 3600}) // expired two hours ago
	s.SetClock(time.Now)
	if got := runHook(t, ""); got != "" {
		t.Errorf("an expired message must be silent, got %q", got)
	}
}

func TestHookLeavesADeliveryReceiptWithTheCharactersItInjected(t *testing.T) {
	s, _ := mesh(t)
	urgent, _ := s.Send(msg.Message{From: "other/agent", To: "all", Body: "build is red", Prio: msg.PrioUrgent})
	fyi, _ := s.Send(msg.Message{From: "other/agent", To: "all", Body: "lunch at noon", Prio: msg.PrioFYI})
	runHook(t, "")
	acks := map[string]msg.Ack{}
	for _, r := range s.Since(map[string]uint64{}, 100) {
		if r.Ack != nil {
			acks[r.Ack.ID] = *r.Ack
		}
	}
	if a := acks[urgent.ID]; a.Via != msg.ViaHook || a.Chars <= len("build is red") {
		t.Errorf("a shown message is receipted with the header and body it injected: %+v", a)
	}
	if a := acks[fyi.ID]; a.Via != msg.ViaHookCount || a.Chars != 0 {
		t.Errorf("an fyi message is only counted, so it cost nothing: %+v", a)
	}
}

func TestSessionStartLearnsTheNodeListOnceThenLeavesItAlone(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	old := trustedKeys
	trustedKeys = func() []ed25519.PublicKey { return []ed25519.PublicKey{pub} }
	t.Cleanup(func() { trustedKeys = old })

	dir := t.TempDir()
	var fetches atomic.Int64
	s, _ := store.Open(t.TempDir(), "n1")
	inner := server.NewHandler(s, "tok", server.WithReleaseDir(dir))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "nodes.json") {
			fetches.Add(1)
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	extra := "http://100.64.0.12:7777"
	if _, err := release.PackNodes(release.PackNodesOptions{Dir: dir, Serial: 3, Key: priv,
		Nodes: []release.NodeEntry{{Name: "n1", URL: srv.URL}, {Name: "node-c", URL: extra}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_OFF", "")
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	t.Chdir(t.TempDir())

	if got := runHook(t, ""); got != "" {
		t.Fatalf("learning the list is silent: %q", got)
	}
	if got := config.LoadDiscovered(); got.Serial != 3 || len(got.Nodes) != 2 {
		t.Fatalf("the list was saved: %+v", got)
	}
	if urls := config.URLs(); len(urls) != 2 || urls[1] != extra {
		t.Errorf("the next session knows the new node: %v", urls)
	}
	runHook(t, "")
	runHook(t, "")
	if fetches.Load() != 1 { // the first session fetched nodes.json; the next two found the list fresh
		t.Errorf("a fresh list must not be fetched again at every session start: %d fetches of nodes.json", fetches.Load())
	}
}

func TestQuietHoursHoldAsksBackButNeverUrgent(t *testing.T) {
	s, _ := mesh(t)
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "please review when free", Prio: msg.PrioAsk})
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "prod is down", Prio: msg.PrioUrgent})
	q, _ := config.ParseQuiet("22:00-07:00")
	if err := config.SaveQuiet(q); err != nil {
		t.Fatal(err)
	}
	defer setClock(func() time.Time { return time.Date(2026, 10, 14, 23, 30, 0, 0, time.Local) })()
	got := runHook(t, "")
	if !strings.Contains(got, "prod is down") || strings.Contains(got, "please review") {
		t.Fatalf("at night the urgent one shows and the ask is held:\n%s", got)
	}
	if !strings.Contains(got, "1 message(s) held during quiet hours") {
		t.Errorf("held messages are counted: %s", got)
	}
	if left := s.Inbox("air/codex", false); len(left) != 1 || left[0].Body != "please review when free" {
		t.Errorf("a held ask stays unread: %+v", left)
	}

	setClock(func() time.Time { return time.Date(2026, 10, 15, 9, 0, 0, 0, time.Local) })
	if got := runHook(t, ""); !strings.Contains(got, "please review when free") || strings.Contains(got, "quiet hours") {
		t.Errorf("in the morning it is shown as usual:\n%s", got)
	}
}

// quietUntil sets the do-not-disturb setting to hold for the next hour, whatever the clock says.
func quietUntil(t *testing.T) {
	t.Helper()
	if err := config.SaveQuiet(config.Quiet{Until: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func TestQuietSettingSilencesTheUpdateNoticeAndSaysItLater(t *testing.T) {
	releaseMesh(t, "0.2.0", "0.3.0")
	quietUntil(t)
	if got := runHook(t, ""); got != "" {
		t.Fatalf("a notice is not urgent: it waits for the end of quiet hours: %q", got)
	}
	if err := config.SaveQuiet(config.Quiet{}); err != nil {
		t.Fatal(err)
	}
	if got := runHook(t, ""); !strings.Contains(got, "Release 0.3.0 is available") {
		t.Errorf("held back means said later, not swallowed: %q", got)
	}
}

func TestQuietSettingSilencesTheRadarButNotUrgent(t *testing.T) {
	s := radarMesh(t, nil)
	now := time.Now()
	s.MergePresence([]msg.RosterEntry{peer("laptop/codex.x9", "aivoicebus", "main", "laptop", 4*time.Minute, now)})
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "prod is down", Prio: msg.PrioUrgent})
	quietUntil(t)
	got := runHook(t, "")
	if strings.Contains(got, "other session(s) on this repo") || !strings.Contains(got, "prod is down") {
		t.Errorf("urgent shows, the radar does not:\n%s", got)
	}
	if err := config.SaveQuiet(config.Quiet{}); err != nil {
		t.Fatal(err)
	}
	if got := runHook(t, ""); !strings.Contains(got, "other session(s) on this repo") {
		t.Errorf("outside quiet hours the radar speaks:\n%s", got)
	}
}

func TestHookDoesNotPinCapabilitiesIntoTheSessionLabels(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Setenv("AIVOICEBUS_CAPS", "ask,run")
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	Run([]string{"--tool", "claude"}, strings.NewReader(`{"session_id":"A1B2-zzz"}`), io.Discard)
	b, _ := os.ReadFile(envFile)
	if !strings.Contains(string(b), "export AIVOICEBUS_LABELS='") || strings.Contains(string(b), "caps=") {
		t.Errorf("caps follow AIVOICEBUS_CAPS, they are not frozen at session start:\n%s", b)
	}
}

// The session start declares the machine's facts with its labels, so `who --has` sees them at
// once, and does not pin them: they are worked out again every few minutes.
func TestHookAdvertisesTheMachinesFactsWithoutPinningThem(t *testing.T) {
	s, _ := mesh(t)
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Setenv("AIVOICEBUS_FACTS", "")
	t.Setenv("AIVOICEBUS_FACT_SSH", "off")
	home, _ := os.UserHomeDir()
	os.MkdirAll(filepath.Join(home, ".config", "aivoicebus"), 0o700)
	os.WriteFile(filepath.Join(home, ".config", "aivoicebus", "release-signing.key"), []byte("seed"), 0o600)
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	Run([]string{"--tool", "claude"}, strings.NewReader(`{"session_id":"A1B2-zzz"}`), io.Discard)
	b, _ := os.ReadFile(envFile)
	if !strings.Contains(string(b), "export AIVOICEBUS_LABELS='") || strings.Contains(string(b), msg.HasLabel+"=") {
		t.Errorf("the facts are not frozen at session start:\n%s", b)
	}
	var said string
	for _, e := range s.Roster() {
		said += e.Me + " " + e.Labels[msg.HasLabel] + "\n"
		if e.Labels[msg.HasLabel] == msg.FactSigningKey {
			return
		}
	}
	t.Errorf("no session declared has=signing-key:\n%s", said)
}

// Gemini CLI reads a hook's stdout as JSON only: the text goes in additionalContext, and a hook
// with nothing to say prints nothing. Codex passes the session's model, which becomes its label.
func TestGeminiGetsJSONAndCodexSaysItsModel(t *testing.T) {
	s, _ := mesh(t)
	t.Setenv("AIVOICEBUS_ME", "air/gemini.g1")
	var quiet strings.Builder
	Run([]string{"--tool", "gemini"}, strings.NewReader(`{"session_id":"g1","hook_event_name":"SessionStart","source":"startup"}`), &quiet)
	if quiet.Len() != 0 {
		t.Errorf("nothing to say must print nothing: %q", quiet.String())
	}
	s.Send(msg.Message{From: "other/agent", To: "air/gemini.g1", Body: "deploy finished"})
	var out strings.Builder
	Run([]string{"--tool", "gemini"}, strings.NewReader(`{"session_id":"g1","hook_event_name":"SessionStart","source":"startup"}`), &out)
	var doc struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil || !strings.Contains(doc.HookSpecificOutput.AdditionalContext, "deploy finished") ||
		!strings.Contains(doc.HookSpecificOutput.AdditionalContext, "UNTRUSTED") {
		t.Fatalf("gemini output is not the JSON it reads: %v %q", err, out.String())
	}

	t.Setenv("AIVOICEBUS_ME", "air/codex.c1")
	t.Setenv("AIVOICEBUS_MODEL", "")
	var c strings.Builder
	Run([]string{"--tool", "codex"}, strings.NewReader(`{"session_id":"c1","hook_event_name":"SessionStart","model":"gpt-5.3-codex"}`), &c)
	found := false
	for _, e := range s.Roster() {
		if e.Me == "air/codex.c1" && e.Labels["model"] == "gpt-5.3-codex" {
			found = true
		}
	}
	if !found {
		t.Errorf("codex's model did not become the session's label: %+v", s.Roster())
	}
}

// Codex reads hook output that starts with '[' or '{' as JSON: "[aivoicebus] ..." failed as
// "invalid session start JSON output" and the session never saw its messages (found live, 0.8.0).
// It gets the JSON its schema asks for, with hookEventName, at session start and on a prompt.
func TestCodexGetsTheJSONItsSchemaAsksFor(t *testing.T) {
	s, _ := mesh(t)
	t.Setenv("AIVOICEBUS_ME", "air/codex.c2")
	var quiet strings.Builder
	Run([]string{"--tool", "codex"}, strings.NewReader(`{"session_id":"c2","hook_event_name":"SessionStart"}`), &quiet)
	if quiet.Len() != 0 {
		t.Errorf("nothing to say must print nothing: %q", quiet.String())
	}
	type out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	s.Send(msg.Message{From: "other/agent", To: "air/codex.c2", Body: "tests are green"})
	var b strings.Builder
	Run([]string{"--tool", "codex"}, strings.NewReader(`{"session_id":"c2","hook_event_name":"SessionStart"}`), &b)
	var d out
	if err := json.Unmarshal([]byte(b.String()), &d); err != nil || d.HookSpecificOutput.HookEventName != "SessionStart" ||
		!strings.Contains(d.HookSpecificOutput.AdditionalContext, "tests are green") {
		t.Fatalf("start: %v %q", err, b.String())
	}
	s.Send(msg.Message{From: "other/agent", To: "all", Body: "main is broken", Prio: msg.PrioUrgent})
	var p strings.Builder
	Run([]string{"--tool", "codex", "--event", "prompt"}, strings.NewReader(`{"session_id":"c2","hook_event_name":"UserPromptSubmit"}`), &p)
	d = out{}
	if err := json.Unmarshal([]byte(p.String()), &d); err != nil || d.HookSpecificOutput.HookEventName != "UserPromptSubmit" ||
		!strings.Contains(d.HookSpecificOutput.AdditionalContext, "main is broken") {
		t.Fatalf("prompt: %v %q", err, p.String())
	}
}
