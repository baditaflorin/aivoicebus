package msg

import (
	"testing"
	"time"
)

func TestActivePause(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	p := Message{ID: "p000000001", From: "lap/claude.a", Body: PauseBody(t0.Add(time.Hour), "runaway loop"), At: t0}
	if m, until, ok := ActivePause([]Message{p}, t0.Add(time.Minute)); !ok || m.ID != p.ID || !until.Equal(t0.Add(time.Hour)) {
		t.Fatal("a pause is in force")
	}
	if _, _, ok := ActivePause([]Message{p}, t0.Add(2*time.Hour)); ok {
		t.Error("a pause runs out")
	}
	other := Message{ID: "r000000001", From: "desk/codex.b", Body: ResumeBody(""), At: t0.Add(time.Minute)}
	if _, _, ok := ActivePause([]Message{p, other}, t0.Add(2*time.Minute)); !ok {
		t.Error("another machine's resume lifted the pause")
	}
	mine := Message{ID: "r000000002", From: "lap/claude.z", Body: ResumeBody("fixed"), At: t0.Add(time.Minute)}
	if _, _, ok := ActivePause([]Message{p, mine}, t0.Add(2*time.Minute)); ok {
		t.Error("the pausing machine's resume did not lift it")
	}
	again := Message{ID: "p000000002", From: "lap/claude.a", Body: PauseBody(t0.Add(time.Hour), "again"), At: t0.Add(2 * time.Minute)}
	if _, _, ok := ActivePause([]Message{p, mine, again}, t0.Add(3*time.Minute)); !ok {
		t.Error("a pause after the resume is in force")
	}
}
