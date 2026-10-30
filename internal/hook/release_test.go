package hook

import (
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/version"
)

func TestReleaseNews(t *testing.T) {
	now := time.Date(2026, 10, 25, 20, 0, 0, 0, time.UTC)
	hourAgo, dayAgo := now.Add(-time.Hour), now.Add(-25*time.Hour)
	off, on := config.AutoUpdate{}, config.AutoUpdate{On: true}
	type st = config.AutoState
	cases := []struct {
		name         string
		latest, here string
		a            config.AutoUpdate
		st           st
		prev         noticeRecord
		kind         news
		start        bool
		want         string // in the line
	}{
		{"current: nothing", "0.3.0", "0.3.0", on, st{}, noticeRecord{}, newsNone, false, ""},
		{"ahead: nothing", "0.3.0", "0.4.0", off, st{}, noticeRecord{}, newsNone, false, ""},
		{"no release announced: nothing", "", "0.3.0", on, st{}, noticeRecord{}, newsNone, false, ""},
		{"behind, off: the line for the person", "0.3.0", "0.2.0", off, st{}, noticeRecord{}, newsBehind, false, "For the person, not the agent: the nodes have release 0.3.0, this machine runs 0.2.0."},
		{"behind, off, said an hour ago", "0.3.0", "0.2.0", off, st{}, noticeRecord{At: hourAgo}, newsNone, false, ""},
		{"behind, off, a newer release an hour after the line", "0.3.1", "0.2.0", off, st{}, noticeRecord{Version: "0.3.0", At: hourAgo}, newsNone, false, ""},
		{"behind, off, said a day ago", "0.3.0", "0.2.0", off, st{}, noticeRecord{At: dayAgo}, newsBehind, false, "aivoicebus update --auto on"},
		{"behind, on: a run and no line", "0.3.0", "0.2.0", on, st{}, noticeRecord{}, newsNone, true, ""},
		{"behind, on, tried an hour ago: nothing", "0.3.0", "0.2.0", on, st{Tried: hourAgo}, noticeRecord{}, newsNone, false, ""},
		{"behind, on, a run failed: said at once", "0.3.0", "0.2.0", on, st{Tried: hourAgo, Result: config.AutoFailed, To: "0.3.0", Error: "no route to host"}, noticeRecord{At: hourAgo}, newsFailed, false, "auto-update did not install release 0.3.0 (no route to host)"},
		{"behind, on, a rollback: said at once", "0.3.0", "0.2.0", on, st{Tried: hourAgo, From: "0.2.0", Result: config.AutoRolledBack, To: "0.3.0", Error: "setup failed", Skip: "0.3.0"}, noticeRecord{At: hourAgo}, newsFailed, false, "auto-update installed release 0.3.0, its setup failed, and it put 0.2.0 back (setup failed); it leaves 0.3.0 out from now on"},
		{"behind, on, the rollback told: nothing, not even daily", "0.3.0", "0.2.0", on, st{Tried: dayAgo, Result: config.AutoRolledBack, To: "0.3.0", Told: true, Skip: "0.3.0"}, noticeRecord{At: dayAgo}, newsNone, false, ""},
		{"behind, on, the failure told and a line today", "0.3.0", "0.2.0", on, st{Tried: hourAgo, Result: config.AutoFailed, Told: true}, noticeRecord{At: hourAgo}, newsNone, false, ""},
		{"behind, on, the failure told yesterday", "0.3.0", "0.2.0", on, st{Tried: hourAgo, Result: config.AutoFailed, To: "0.3.0", Told: true}, noticeRecord{At: dayAgo}, newsFailed, false, "aivoicebus update, which says why"},
		{"behind, on, failed a day ago, not told: told first", "0.3.0", "0.2.0", on, st{Tried: dayAgo, Result: config.AutoFailed, To: "0.3.0"}, noticeRecord{}, newsFailed, false, "did not install release 0.3.0"},
		{"behind, on, failed a day ago and told: tried again", "0.3.0", "0.2.0", on, st{Tried: dayAgo, Result: config.AutoFailed, Told: true}, noticeRecord{}, newsNone, true, ""},
		{"behind, on, a dev build: the line, never a run", "0.3.0", "dev", on, st{}, noticeRecord{}, newsBehind, false, "this machine runs dev"},
		{"behind, on, the person rolled it back: nothing", "0.3.0", "0.2.0", on, st{Tried: dayAgo, Skip: "0.3.0"}, noticeRecord{}, newsNone, false, ""},
		{"behind, on, newer than what was rolled back: a run", "0.3.1", "0.2.0", on, st{Tried: dayAgo, Skip: "0.3.0"}, noticeRecord{}, newsNone, true, ""},
		{"installed by itself: said once", "0.3.0", "0.3.0", on, st{Tried: hourAgo, Result: config.AutoInstalled, From: "0.2.0", To: "0.3.0"}, noticeRecord{}, newsInstalled, false, "This machine installed release 0.3.0 by itself (auto-update, which the person turned on; it ran 0.2.0"},
		{"installed and told: nothing", "0.3.0", "0.3.0", on, st{Tried: hourAgo, Result: config.AutoInstalled, To: "0.3.0", Told: true}, noticeRecord{}, newsNone, false, ""},
		{"installed, then rolled back by hand: not claimed", "0.3.0", "0.2.0", on, st{Tried: hourAgo, Result: config.AutoInstalled, To: "0.3.0", Skip: "0.3.0"}, noticeRecord{}, newsNone, false, ""},
	}
	for _, c := range cases {
		line, kind, start := releaseNews(c.latest, c.here, c.a, c.st, c.prev, now)
		if kind != c.kind || start != c.start || (c.want == "") != (line == "") || !strings.Contains(line, c.want) {
			t.Errorf("%s: kind %d start %v line %q; want kind %d start %v and %q", c.name, kind, start, line, c.kind, c.start, c.want)
		}
	}
}

// stubStart counts the automatic updates a session start would start, instead of starting one.
func stubStart(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	old := startAuto
	startAuto = func() error { n.Add(1); return nil }
	t.Cleanup(func() { startAuto = old })
	return &n
}

// With auto-update on, a session start starts the run (once a day, whatever the number of sessions)
// instead of saying anything, and later says once that it failed.
func TestSessionStartStartsTheAutoUpdateOnceADay(t *testing.T) {
	releaseMesh(t, "0.2.0", "0.3.0")
	starts := stubStart(t)
	if err := config.SaveAutoUpdate(config.AutoUpdate{On: true}); err != nil {
		t.Fatal(err)
	}
	if got := runHook(t, ""); got != "" {
		t.Errorf("a session start that starts the run says nothing: %q", got)
	}
	if starts.Load() != 1 {
		t.Fatalf("started %d run(s)", starts.Load())
	}
	st := config.LoadAutoState()
	if st.Tried.IsZero() || st.From != "0.2.0" || st.To != "0.3.0" || st.Result != "" {
		t.Fatalf("the run is stamped before it starts: %+v", st)
	}
	if got := runHook(t, ""); got != "" || starts.Load() != 1 {
		t.Fatalf("the next session start the same day: %q, %d run(s)", got, starts.Load())
	}
	_ = config.EditAutoState(func(s *config.AutoState) bool {
		s.Result, s.Error = config.AutoFailed, "refusing the release from the mesh: bad signature"
		return true
	})
	if got := runHook(t, ""); !strings.Contains(got, "auto-update did not install release 0.3.0 (refusing the release from the mesh: bad signature)") {
		t.Errorf("the failure: %q", got)
	}
	if got := runHook(t, ""); got != "" || starts.Load() != 1 {
		t.Errorf("the failure is said once a day, and tried again tomorrow: %q, %d run(s)", got, starts.Load())
	}
	restore := setClock(func() time.Time { return time.Now().Add(25 * time.Hour) })
	defer restore()
	runHook(t, "")
	if starts.Load() != 2 {
		t.Errorf("a day later it is tried again: %d run(s)", starts.Load())
	}
}

// Of many sessions starting at once, exactly one starts the run: the record is checked again under
// its lock before it is stamped.
func TestManySessionStartsAtOnceStartOneRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	old := version.Release
	version.Release = "0.2.0"
	t.Cleanup(func() { version.Release = old })
	if err := config.SaveAutoUpdate(config.AutoUpdate{On: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var starts atomic.Int32
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			if _, start := releaseNotice("0.3.0", now, false); start {
				starts.Add(1)
			}
		}()
	}
	close(gate)
	wg.Wait()
	if starts.Load() != 1 {
		t.Fatalf("%d session starts started a run", starts.Load())
	}
}

// A session start that finds the run due, but whose record another session stamps before it can,
// starts nothing.
func TestASessionThatLosesTheStampStartsNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	old := version.Release
	version.Release = "0.2.0"
	t.Cleanup(func() { version.Release = old; beforeStamp = func() {} })
	if err := config.SaveAutoUpdate(config.AutoUpdate{On: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	beforeStamp = func() {
		_ = config.EditAutoState(func(s *config.AutoState) bool { s.Tried = now; return true })
	}
	if _, start := releaseNotice("0.3.0", now, false); start {
		t.Fatal("started a run another session had just started")
	}
}

// With it off (the default), a session start never starts a run.
func TestSessionStartStartsNothingWhileAutoUpdateIsOff(t *testing.T) {
	releaseMesh(t, "0.2.0", "0.3.0")
	starts := stubStart(t)
	if got := runHook(t, ""); !strings.Contains(got, "the nodes have release 0.3.0") || starts.Load() != 0 {
		t.Fatalf("%d run(s), line %q", starts.Load(), got)
	}
	if !config.LoadAutoState().Tried.IsZero() {
		t.Error("nothing was tried")
	}
}

// A run is a process of its own: the tool's end of the hook's process group must not end it.
func TestTheRunIsDetached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix only")
	}
	cmd := exec.Command("true")
	detach(cmd)
	if cmd.SysProcAttr == nil || !sysProcSetsid(cmd) {
		t.Fatal("the run is not put in a session of its own")
	}
}

// A fleet update task (sent by 0.11.4 to 0.13.x) for a release this machine runs waits for nobody:
// the person line leaves it out, and names it while the machine is still behind.
func TestPersonLineLeavesOutAnUpdateTaskForTheReleaseThatRunsHere(t *testing.T) {
	s, _ := personMesh(t, "0.11.4")
	s.Send(msg.Message{From: "deploy/claude", To: "air/human", Kind: msg.KindTask, Body: updateTask}) // asks for 0.11.5
	old := version.Release
	t.Cleanup(func() { version.Release = old })
	for _, v := range []string{"0.11.5", "0.12.0"} {
		version.Release = v
		if got := runHook(t, ""); strings.Contains(got, "for the user:") {
			t.Errorf("running %s, the task for 0.11.5 is done: %q", v, got)
		}
	}
	version.Release = "0.11.4"
	if got := runHook(t, ""); !strings.Contains(got, "[aivoicebus] for the user: task") {
		t.Errorf("running 0.11.4, the task still waits: %q", got)
	}
}

// A task for the person raises one notification on this machine (macOS), whichever session or hook
// sees it first, with fixed words: a count, never what a sender wrote.
func TestTasksForThePersonRaiseOneNotificationEach(t *testing.T) {
	s, _ := personMesh(t, "0.11.4")
	everyPrompt(t)
	var calls []int
	old := notifyPerson
	notifyPerson = func(n int) { calls = append(calls, n) }
	t.Cleanup(func() { notifyPerson = old })
	task := func(body string) {
		if _, err := s.Send(msg.Message{From: "deploy/claude", To: "air/human", Kind: msg.KindTask, Body: msg.PersonTag + " " + body}); err != nil {
			t.Fatal(err)
		}
	}
	runHook(t, "")
	if len(calls) != 0 {
		t.Fatalf("nothing waits, nothing raised: %v", calls)
	}
	task("rotate the deploy key")
	runHook(t, "")
	t.Setenv("AIVOICEBUS_ME", "air/claude.zz") // another session on the same machine
	runHook(t, "")
	if len(calls) != 1 || calls[0] != 1 {
		t.Fatalf("one notification for the one task: %v", calls)
	}
	task("plug in the backup disk")
	promptHook(t, `{"session_id":"zz"}`) // it arrives while that session works
	if len(calls) != 2 || calls[1] != 1 {
		t.Fatalf("the new task raises one more, for one new task: %v", calls)
	}
	task("one")
	task("two")
	t.Setenv("AIVOICEBUS_ME", "air/codex.n3")
	runHook(t, "")
	if len(calls) != 3 || calls[2] != 2 {
		t.Fatalf("two new tasks, one notification counting them: %v", calls)
	}
	runHook(t, "")
	if len(calls) != 3 {
		t.Errorf("nothing new, nothing raised: %v", calls)
	}
}
