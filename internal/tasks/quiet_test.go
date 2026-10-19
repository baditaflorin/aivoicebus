package tasks

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// LastWord: a claim starts the clock, progress moves it on, and an answer or a question back ends
// the watch; the sender's own messages never count.
func TestLastWordOfATakenTask(t *testing.T) {
	t0 := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	task := msg.Message{ID: "aaaaaaaaaa", From: "desk/claude.z9", To: "lap", Kind: msg.KindTask, Thread: "aaaaaaaaaa", At: t0}
	claim := msg.Message{ID: "bbbbbbbbbb", From: "lap/codex.b2", Re: task.ID, Thread: task.Thread, Kind: msg.KindClaim, At: t0.Add(time.Minute)}
	prog := msg.Message{ID: "cccccccccc", From: "lap/codex.b2", Re: task.ID, Thread: task.Thread, Kind: msg.KindProgress, At: t0.Add(5 * time.Minute)}
	nudge := msg.Message{ID: "dddddddddd", From: task.From, Re: task.ID, Thread: task.Thread, At: t0.Add(9 * time.Minute)}
	if _, _, taken, open := LastWord(task, []msg.Message{task}); taken || !open {
		t.Errorf("untaken: %v %v", taken, open)
	}
	owner, last, taken, open := LastWord(task, []msg.Message{task, claim, prog, nudge})
	if !taken || !open || owner.From != claim.From || !last.Equal(prog.At) {
		t.Errorf("progress: %v %v %v %v", owner, last, taken, open)
	}
	answer := msg.Message{ID: "eeeeeeeeee", From: claim.From, Re: task.ID, Thread: task.Thread, Kind: msg.KindAnswer, At: t0.Add(20 * time.Minute)}
	if _, _, _, open := LastWord(task, []msg.Message{task, claim, answer}); open {
		t.Error("an answered task is still watched")
	}
	gone := task
	gone.Retracted = &msg.Retraction{}
	if _, _, taken, open := LastWord(gone, []msg.Message{gone, claim}); taken || open {
		t.Error("a task taken back is still watched")
	}
}

// The alert names ids, sessions and times only.
func TestSilentTextAndSummary(t *testing.T) {
	now := time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC)
	line := SilentText(Silent{ID: "aaaaaaaaaa", To: "lap", By: "lap/codex.b2", TakenAt: now.Add(-25 * time.Minute), LastWord: now.Add(-12 * time.Minute)}, now)
	if !strings.Contains(line, "task aaaaaaaaaa to lap was taken by lap/codex.b2") || !strings.Contains(line, "silent since") || !strings.Contains(line, "12m") {
		t.Errorf("%s", line)
	}
	if s := Summary("a\nb\tc\x07 "+strings.Repeat("y", 200), 20); len([]rune(s)) != 20 || strings.ContainsAny(s, "\n\t\x07") || !strings.HasSuffix(s, "…") {
		t.Errorf("summary %q", s)
	}
}
