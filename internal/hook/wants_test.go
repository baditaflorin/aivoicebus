package hook

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/version"
)

func installedRelease(t *testing.T, v string) {
	t.Helper()
	old := version.Release
	version.Release = v
	t.Cleanup(func() { version.Release = old })
}

// A message whose done-when is true on this machine is shown to no session, the first or a new one,
// and is settled for the machine with one line to its sender (the node applies it: a release that does).
func TestAMetDoneWhenShowsNothingToANewSessionAndIsSettledOnce(t *testing.T) {
	s, _ := personMesh(t, "0.15.0")
	installedRelease(t, "0.14.1")
	body := msg.AppendWants("Update aivoicebus on air to 0.15.0.", msg.Wants{DoneWhen: "release>=0.15.0"})
	m, _ := s.Send(msg.Message{From: "deploy/claude.s1", To: "air", Body: body})
	p, _ := s.Send(msg.Message{From: "deploy/claude.s1", To: "air/human", Kind: msg.KindTask, Body: msg.PersonTag + " update by hand\n[done-when: release>=0.15.0]"})

	t.Setenv("AIVOICEBUS_ME", "air/codex.first")
	if got := runHook(t, ""); !strings.Contains(got, m.ID) || !strings.Contains(got, p.ID) {
		t.Fatalf("while the condition is false both are shown:\n%s", got)
	}

	installedRelease(t, "0.15.0") // the person updated this machine
	for _, me := range []string{"air/codex.first", "air/claude.second"} {
		t.Setenv("AIVOICEBUS_ME", me)
		if got := runHook(t, ""); strings.TrimSpace(got) != "" {
			t.Errorf("%s: a message whose condition is met costs nothing, got:\n%s", me, got)
		}
	}
	waitFor(t, func() bool { return len(s.Settled(m.ID)) == 1 && len(s.Settled(p.ID)) == 1 })
	t.Setenv("AIVOICEBUS_ME", "air/claude.third") // never started before: the node itself hides it now
	if ms := s.Inbox("air/claude.third", false); len(ms) != 0 {
		for _, x := range ms {
			if x.ID == m.ID || x.ID == p.ID {
				t.Errorf("the node still delivers %s", x.ID)
			}
		}
	}
	var told int
	for _, x := range s.Thread(m.ID) {
		if x.Kind == msg.KindProgress && x.To == "deploy/claude.s1" && x.Body == msg.DoneMetTag {
			told++
		}
	}
	if told < 1 {
		t.Errorf("the sender is told in the thread: %+v", s.Thread(m.ID))
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("it did not happen")
}

// Against a node that does not apply settles the hook still shows nothing, and writes nothing: every
// session start would tell the sender again.
func TestAMetDoneWhenOnAnOldNodeIsHiddenButNotWritten(t *testing.T) {
	s, _ := personMesh(t, "0.14.0")
	installedRelease(t, "0.15.0")
	m, _ := s.Send(msg.Message{From: "deploy/claude.s1", To: "air", Body: msg.AppendWants("update", msg.Wants{DoneWhen: "release>=0.15.0"})})
	t.Setenv("AIVOICEBUS_ME", "air/codex.first")
	if got := runHook(t, ""); strings.TrimSpace(got) != "" {
		t.Errorf("shown:\n%s", got)
	}
	time.Sleep(300 * time.Millisecond)
	if len(s.Settled(m.ID)) != 0 || len(s.Thread(m.ID)) != 1 {
		t.Errorf("an old node got a settle or a progress line: %+v", s.Thread(m.ID))
	}
}

func TestNodeSettles(t *testing.T) {
	for v, want := range map[string]bool{"dev": true, "0.15.0": true, "0.15.1": true, "1.0.0": true, "0.14.0": false, "0.13.1": false, "": false, "junk": false} {
		if nodeSettles(v) != want {
			t.Errorf("%q: %v", v, !want)
		}
	}
}
