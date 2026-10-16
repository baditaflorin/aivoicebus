package hook

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

// at makes the hooks' clock read t until the test ends.
func at(t *testing.T, now time.Time) func(time.Time) {
	t.Helper()
	old := clock
	t.Cleanup(func() { clock = old })
	set := func(v time.Time) { now = v; clock = func() time.Time { return now } }
	set(now)
	return set
}

func readStatus(t *testing.T) config.HookStatus {
	t.Helper()
	st, ok := config.LoadHookStatus(config.HookStatusPath())
	if !ok {
		t.Fatalf("no hook status at %s", config.HookStatusPath())
	}
	return st
}

func TestAdvanceCountsFailuresAndWarnsOnceADay(t *testing.T) {
	var st config.HookStatus
	step := func(outcome string, now time.Time) (line string, save bool) {
		var next config.HookStatus
		next, line, save = advance(st, outcome, "n1:7777", now)
		st = next
		return
	}

	// a refused token will not fix itself: the first run says so, the rest of the day stay quiet
	if line, save := step(config.HookRejected, t0); line != warnRejected || !save || st.Failures != 1 || st.WarnedAt != t0 {
		t.Fatalf("first refusal: line=%q save=%v %+v", line, save, st)
	}
	if line, _ := step(config.HookRejected, t0.Add(time.Hour)); line != "" || st.Failures != 2 {
		t.Errorf("a second refusal within the day is silent: %q %+v", line, st)
	}
	if line, _ := step(config.HookRejected, t0.Add(24*time.Hour)); line != warnRejected || st.WarnedAt != t0.Add(24*time.Hour) {
		t.Errorf("a day later it says so again: %q %+v", line, st)
	}

	// recovery clears the streak but keeps the stamp, so a mesh that flaps cannot nag
	if line, save := step(config.HookOK, t0.Add(25*time.Hour)); line != "" || !save || st.Failures != 0 || st.LastOK != t0.Add(25*time.Hour) || st.WarnedAt != t0.Add(24*time.Hour) {
		t.Errorf("recovery: line=%q save=%v %+v", line, save, st)
	}

	// an unreachable mesh is given three runs and half an hour before the line (and a day since the last one)
	for i, want := range []string{"", "", warnUnreachable, ""} {
		line, _ := step(config.HookUnreachable, t0.Add(50*time.Hour+time.Duration(i)*20*time.Minute))
		if line != want || st.Failures != i+1 {
			t.Errorf("unreachable run %d: line=%q failures=%d, want line %q", i+1, line, st.Failures, want)
		}
	}
	if st.LastOK != t0.Add(25*time.Hour) {
		t.Errorf("a failure must not move last_ok: %v", st.LastOK)
	}
	if st.FailingSince != t0.Add(50*time.Hour) {
		t.Errorf("the run of failures began at its first failure: %v", st.FailingSince)
	}
}

// A laptop that has just left the wifi fails every prompt for a minute. That is not a mesh
// that has been unreachable for several runs.
func TestAQuickRunOfFailuresIsNotWorthALine(t *testing.T) {
	var st config.HookStatus
	for i := 0; i < 20; i++ {
		var line string
		st, line, _ = advance(st, config.HookUnreachable, "n1:7777", t0.Add(time.Duration(i)*time.Second))
		if line != "" {
			t.Fatalf("run %d spoke after %v of failing: %q", i+1, time.Duration(i)*time.Second, line)
		}
	}
	// the same streak, half an hour on, is worth the line
	if _, line, _ := advance(st, config.HookUnreachable, "n1:7777", t0.Add(warnAfterFailing)); line != warnUnreachable {
		t.Errorf("a streak that lasted %v says so: %q", warnAfterFailing, line)
	}
	// and a success in between starts the clock again
	st, _, _ = advance(st, config.HookOK, "n1:7777", t0.Add(time.Hour))
	if !st.FailingSince.IsZero() || st.Failures != 0 {
		t.Errorf("recovery clears the streak: %+v", st)
	}
	// a stamp from the future (the clock was set back) restarts it instead of counting backwards
	st = config.HookStatus{Outcome: config.HookUnreachable, Failures: 5, FailingSince: t0.Add(100 * time.Hour)}
	if next, line, _ := advance(st, config.HookUnreachable, "n1:7777", t0); line != "" || next.FailingSince != t0 {
		t.Errorf("%q %+v", line, next)
	}
}

// The lines are injected into an agent's context, so they stay fixed, one line each, and
// the only thing they say about the mesh is what the hook itself concluded.
func TestTheWarningLinesAreFixedSingleLines(t *testing.T) {
	for _, l := range []string{warnRejected, warnUnreachable} {
		if strings.ContainsAny(l, "\n\r%") || !strings.HasPrefix(l, "[aivoicebus] ") || !strings.Contains(l, "`aivoicebus doctor`") {
			t.Errorf("a warning line must be one fixed line that points at doctor: %q", l)
		}
	}
}

func TestAStampInTheFutureDoesNotSilenceTheWarningForGood(t *testing.T) {
	prev := config.HookStatus{LastRun: t0, Outcome: config.HookRejected, Failures: 1, WarnedAt: t0.Add(400 * 24 * time.Hour)}
	if _, line, _ := advance(prev, config.HookRejected, "", t0.Add(time.Minute)); line != warnRejected {
		t.Errorf("the clock was set back: %q", line)
	}
}

func TestAHealthyMachineWritesItsNoteAtMostEveryTenMinutes(t *testing.T) {
	prev := config.HookStatus{LastRun: t0, Outcome: config.HookOK, LastOK: t0, Node: "n1:7777"}
	if _, _, save := advance(prev, config.HookOK, "n1:7777", t0.Add(9*time.Minute)); save {
		t.Error("nothing but the time changed: no write")
	}
	if _, _, save := advance(prev, config.HookOK, "n1:7777", t0.Add(10*time.Minute)); !save {
		t.Error("an unchanged note is refreshed after ten minutes")
	}
	if _, _, save := advance(prev, config.HookOK, "n2:7777", t0.Add(time.Minute)); !save {
		t.Error("a different node is a change")
	}
	if _, _, save := advance(prev, config.HookRejected, "n1:7777", t0.Add(time.Minute)); !save {
		t.Error("a change of outcome is written at once")
	}
}

func TestNodeHostKeepsHostAndPortOnly(t *testing.T) {
	for in, want := range map[string]string{
		"http://node1:7777":                  "node1:7777",
		"http://user:hunter2@node1:7777/x?y": "node1:7777",
		"https://node1":                      "node1",
		"::not a url":                        "",
	} {
		if got := nodeHost([]string{in, "http://other:1"}); got != want {
			t.Errorf("nodeHost(%q) = %q, want %q", in, got, want)
		}
	}
	if nodeHost(nil) != "" {
		t.Error("no node, no host")
	}
}

// tokenMesh is mesh() with a token the test chooses, and a way to give the hook a different one.
func tokenMesh(t *testing.T, token string) (*store.Store, string) {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, token))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", token)
	t.Setenv("AIVOICEBUS_OFF", "")
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	t.Chdir(t.TempDir())
	return s, srv.URL
}

func TestTheHookNotesItsRunsAndWarnsWhenTheTokenIsRejected(t *testing.T) {
	good := "good-token-0123456789-0123456789"
	s, _ := tokenMesh(t, good)
	set := at(t, t0)
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "private plans for the quarter"})

	// ok: the message arrives, the note says so, and nothing else is printed
	got := runHook(t, "")
	if !strings.Contains(got, "private plans") || strings.Contains(got, warnRejected) {
		t.Fatalf("a healthy run shows the message and no warning:\n%s", got)
	}
	if st := readStatus(t); st.Outcome != config.HookOK || st.Failures != 0 || st.LastRun != t0 || st.LastOK != t0 || st.Node == "" {
		t.Fatalf("after an ok run: %+v", st)
	}

	// the token is retired: ok -> rejected, with the one line
	bad := "retired-token-0123456789-012345"
	t.Setenv("AIVOICEBUS_TOKEN", bad)
	set(t0.Add(time.Hour))
	if got := runHook(t, ""); got != warnRejected+"\n" {
		t.Fatalf("a refused token prints the fixed line and nothing else: %q", got)
	}
	st := readStatus(t)
	if st.Outcome != config.HookRejected || st.Failures != 1 || st.LastOK != t0 || st.WarnedAt != t0.Add(time.Hour) {
		t.Fatalf("after a refusal: %+v", st)
	}

	// the same day: silent, but the note keeps counting
	set(t0.Add(5 * time.Hour))
	if got := runHook(t, ""); got != "" {
		t.Errorf("one line a day: %q", got)
	}
	if st := readStatus(t); st.Failures != 2 {
		t.Errorf("failures: %+v", st)
	}
	// the next day: the line again
	set(t0.Add(26 * time.Hour))
	if got := runHook(t, ""); got != warnRejected+"\n" {
		t.Errorf("a day on, the line is repeated: %q", got)
	}

	// the token is fixed: rejected -> ok
	t.Setenv("AIVOICEBUS_TOKEN", good)
	set(t0.Add(27 * time.Hour))
	runHook(t, "")
	if st := readStatus(t); st.Outcome != config.HookOK || st.Failures != 0 || st.LastOK != t0.Add(27*time.Hour) {
		t.Errorf("after the fix: %+v", st)
	}

	// the note is private, and is not a copy of anything it could leak
	path := config.HookStatusPath()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v %v", fi, err)
	}
	b, _ := os.ReadFile(path)
	for _, secret := range []string{good, bad, "private plans", "air/codex"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the status file contains %q:\n%s", secret, b)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestAForbiddenReadCountsAsRejectedToo(t *testing.T) {
	// a machine token may read only its own machine's inbox: asking as another machine is a 403
	const machineTok = "machine-token-mmmmmmmmmmmmmmmmmmmmmm"
	ts, err := server.ParseTokens("shared-token-aaaaaaaaaaaaaaaaaaaaaaaa\nlaptop-a:" + machineTok + "\n")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := store.Open(t.TempDir(), "n2")
	srv := httptest.NewServer(server.NewHandler(s, "", server.WithTokenSet(ts)))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", machineTok)
	t.Setenv("AIVOICEBUS_OFF", "")
	t.Setenv("AIVOICEBUS_ME", "other-host/claude")
	t.Chdir(t.TempDir())
	at(t, t0)
	if got := runHook(t, ""); got != warnRejected+"\n" {
		t.Errorf("a 403 means this machine receives nothing too: %q", got)
	}
	if st := readStatus(t); st.Outcome != config.HookRejected {
		t.Errorf("%+v", st)
	}
}

func TestThreeFailedRunsInARowOverHalfAnHourAreWorthOneLine(t *testing.T) {
	tokenMesh(t, "tok")
	t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:1") // nothing listens there
	set := at(t, t0)
	for i, want := range []string{"", "", warnUnreachable + "\n", "", ""} {
		set(t0.Add(time.Duration(i) * 20 * time.Minute))
		if got := runHook(t, ""); got != want {
			t.Errorf("run %d printed %q, want %q", i+1, got, want)
		}
	}
	if st := readStatus(t); st.Outcome != config.HookUnreachable || st.Failures != 5 || !st.LastOK.IsZero() || st.Node != "127.0.0.1:1" {
		t.Errorf("%+v", st)
	}
}

func TestTheWarningComesFromThePerPromptHookToo(t *testing.T) {
	tokenMesh(t, "tok")
	t.Setenv("AIVOICEBUS_TOKEN", "wrong-token-0123456789-0123456789")
	everyPrompt(t)
	at(t, t0)
	if got := promptHook(t, ""); got != warnRejected+"\n" {
		t.Errorf("%q", got)
	}
	if got := promptHook(t, ""); got != "" {
		t.Errorf("once a day across both hooks, not once per prompt: %q", got)
	}
	if got := runHook(t, ""); got != "" {
		t.Errorf("and the session-start hook shares the stamp: %q", got)
	}
}

func TestAHookThatTimesOutCountsAsUnreachableAndStillWarns(t *testing.T) {
	tokenMesh(t, "tok")
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(time.Second) }))
	defer slow.Close()
	t.Setenv("AIVOICEBUS_URL", slow.URL)
	old := Deadline
	Deadline = 150 * time.Millisecond
	defer func() { Deadline = old }()
	set := at(t, t0)

	var lines []string
	for i := 0; i < 4; i++ {
		set(t0.Add(time.Duration(i) * 20 * time.Minute))
		if got := runHook(t, ""); got != "" {
			lines = append(lines, got)
		}
	}
	if len(lines) != 1 || lines[0] != warnUnreachable+"\n" {
		t.Errorf("a black-holed mesh is noticed after three runs over half an hour, once: %q", lines)
	}
	if st := readStatus(t); st.Outcome != config.HookUnreachable || st.Failures < 3 {
		t.Errorf("%+v", st)
	}
}

// hang is a node that takes the connection and never answers.
func hang(t *testing.T) string {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv.URL
}

// The per-prompt hook has 700 ms: too little to tell a slow node from a dead one, and it runs
// on every prompt of every session. A look that got no answer is not a failure it records, so
// a node that answers in 600 ms can never make it warn about an unreachable mesh.
func TestThePromptHookNeverCountsALookThatGotNoAnswer(t *testing.T) {
	tokenMesh(t, "tok")
	everyPrompt(t)
	set := at(t, t0)

	t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:1") // refused at once
	for i := 0; i < 6; i++ {
		set(t0.Add(time.Duration(i) * 20 * time.Minute))
		if got := promptHook(t, ""); got != "" {
			t.Errorf("prompt %d printed %q", i+1, got)
		}
	}
	if st, ok := config.LoadHookStatus(config.HookStatusPath()); ok {
		t.Errorf("no note of looks nobody answered: %+v", st)
	}

	t.Setenv("AIVOICEBUS_URL", hang(t)) // answers after the deadline, i.e. never
	old := PromptDeadline
	PromptDeadline = 100 * time.Millisecond
	defer func() { PromptDeadline = old }()
	for i := 0; i < 6; i++ {
		set(t0.Add(3*time.Hour + time.Duration(i)*20*time.Minute))
		if got := promptHook(t, ""); got != "" {
			t.Errorf("a prompt that timed out printed %q", got)
		}
	}
	if st, ok := config.LoadHookStatus(config.HookStatusPath()); ok {
		t.Errorf("a look that ran out of time leaves no note either: %+v", st)
	}
}

// What the prompt hook does see is as good as any: an answer ends a run of failures.
func TestAPromptThatIsAnsweredEndsTheRunOfFailures(t *testing.T) {
	_, url := tokenMesh(t, "tok")
	everyPrompt(t)
	set := at(t, t0)
	t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:1")
	runHook(t, "")
	runHook(t, "")
	if st := readStatus(t); st.Failures != 2 {
		t.Fatalf("%+v", st)
	}
	t.Setenv("AIVOICEBUS_URL", url)
	set(t0.Add(time.Minute))
	promptHook(t, "")
	if st := readStatus(t); st.Outcome != config.HookOK || st.Failures != 0 || !st.FailingSince.IsZero() {
		t.Errorf("%+v", st)
	}
}

func TestHealthyRunsRewriteTheNoteOnlyEveryTenMinutes(t *testing.T) {
	mesh(t)
	set := at(t, t0)
	runHook(t, "")
	first := readStatus(t)
	set(t0.Add(time.Minute))
	runHook(t, "")
	if st := readStatus(t); st != first {
		t.Errorf("a minute later nothing was written: %+v", st)
	}
	set(t0.Add(11 * time.Minute))
	runHook(t, "")
	if st := readStatus(t); st.LastRun != t0.Add(11*time.Minute) {
		t.Errorf("after ten minutes the note is refreshed: %+v", st)
	}
}

// A home that cannot be written to must not change what the hook says, and must not turn the
// daily line into a line on every run.
func TestAnUnwritableHomeNeverBreaksTheHookOrRepeatsTheLine(t *testing.T) {
	s, _ := tokenMesh(t, "tok")
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.WriteFile(filepath.Join(home, ".config"), []byte("a file where the directory should be"), 0o644)
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "still delivered"})
	at(t, t0)

	if got := runHook(t, ""); !strings.Contains(got, "still delivered") {
		t.Fatalf("the message must arrive whether or not the note can be written: %q", got)
	}
	t.Setenv("AIVOICEBUS_TOKEN", "wrong-token-0123456789-0123456789")
	for i := 0; i < 3; i++ {
		if got := runHook(t, ""); got != "" {
			t.Errorf("without a stamp to rate-limit it there is no line: %q", got)
		}
	}
}

func TestWithoutAHomeThereIsNoNoteAndNoLine(t *testing.T) {
	tokenMesh(t, "tok")
	t.Setenv("AIVOICEBUS_TOKEN", "wrong-token-0123456789-0123456789")
	t.Setenv("HOME", "")
	if config.HookStatusPath() != "" {
		t.Skip("this platform finds a home without $HOME")
	}
	for i := 0; i < 2; i++ {
		if got := runHook(t, ""); got != "" {
			t.Errorf("a line that cannot be rate-limited is not printed: %q", got)
		}
	}
}
