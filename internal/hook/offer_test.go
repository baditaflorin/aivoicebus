package hook

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// A task the nodes offer to this session alone is shown at a session start like a message to it by
// name: in full and before the broadcasts, never as a headline that the broadcast cap can push out,
// with a line that says until when it is this session's and how to take it or pass it on.
func TestASessionStartShowsAnOfferedTaskInFullWithHowToAnswer(t *testing.T) {
	s, _ := mesh(t)
	long := "Roll release 0.13.2 out to the four nodes. " + strings.Repeat("Check each node's health after it restarts. ", 20) // longer than view.HeadlineOver
	task, _ := s.Send(msg.Message{From: "desk/claude.z9", To: "air", Body: long, Kind: msg.KindTask})
	for _, b := range []string{"release notes are up", "the build host is back", "lunch is at noon"} { // newer notes to the same machine: they would take both broadcast slots
		s.Send(msg.Message{From: "other/agent", To: "air", Body: b})
	}
	got := runHook(t, "")
	for _, want := range []string{
		strings.TrimSpace(strings.Repeat("Check each node's health after it restarts. ", 5)), // the body, not a headline
		"Task " + task.ID + " went to air, not to one session: the nodes offer it to this session alone until",
		"first; nobody else is about to take it after",
		"aivoicebus claim " + task.ID,
		`aivoicebus decline ` + task.ID + ` "why"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("session start lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "Roll release") > strings.Index(got, "lunch is at noon") && strings.Contains(got, "lunch is at noon") {
		t.Errorf("the offered task comes after a broadcast:\n%s", got)
	}
	t.Setenv("AIVOICEBUS_ME", "air/claude.q2")
	if other := runHook(t, ""); strings.Contains(other, task.ID) {
		t.Errorf("a second session is shown the task while the first holds its offer:\n%s", other)
	}
}

// The session a person works in is offered a task at its next prompt, once, not at its next start.
func TestThePromptHookShowsATaskOfferedToThisSessionOnce(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "ask first", Prio: msg.PrioAsk})
	task, _ := s.Send(msg.Message{From: "desk/claude.z9", To: "air", Body: "take the 5pm backup", Kind: msg.KindTask})
	got := promptHook(t, "")
	for _, want := range []string{"1 task(s) offered to this session alone arrived while you were working", "take the 5pm backup", "aivoicebus decline " + task.ID, "UNTRUSTED DATA"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt hook lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ask first") {
		t.Errorf("an ask message to this session was shown mid-session:\n%s", got)
	}
	if again := promptHook(t, ""); again != "" {
		t.Errorf("an offer already shown is shown again: %q", again)
	}
	fyi, _ := s.Send(msg.Message{From: "desk/claude.z9", To: "air", Body: "if you have time", Kind: msg.KindTask, Prio: msg.PrioFYI})
	if got := promptHook(t, ""); strings.Contains(got, fyi.ID) {
		t.Errorf("an fyi task interrupted the session:\n%s", got)
	}
}

// Once a task has gone untaken for msg.EscalateAfter, the session shown its offer tells the person,
// after its output; the next session shown it sends nothing more.
func TestASessionShownAnOfferPastHalfAnHourTellsThePersonOnce(t *testing.T) {
	s, _ := mesh(t)
	start := time.Now().UTC()
	clock := start
	s.SetClock(func() time.Time { return clock })
	task, _ := s.Send(msg.Message{From: "desk/claude.z9", To: "air", Body: "roll the release out", Kind: msg.KindTask})
	clock = start.Add(msg.EscalateAfter - time.Minute)
	if got := runHook(t, ""); !strings.Contains(got, task.ID) {
		t.Fatalf("not offered:\n%s", got)
	}
	if escalations(s, task.ID) != 0 {
		t.Fatal("the person was told before the task was untaken for half an hour")
	}
	clock = clock.Add(msg.OfferWindow + time.Minute)
	t.Setenv("AIVOICEBUS_ME", "air/claude.q2")
	if got := runHook(t, ""); !strings.Contains(got, task.ID) {
		t.Fatalf("the next session is not offered it:\n%s", got)
	}
	if n := escalations(s, task.ID); n != 1 {
		t.Fatalf("%d messages told the person, want 1", n)
	}
	e := s.Thread(msg.EscalationID(task.ID))
	if len(e) == 0 || !containsPersonTag(e, msg.EscalationID(task.ID)) {
		t.Fatalf("the escalation is not to the person: %+v", e)
	}
	clock = clock.Add(msg.OfferWindow + time.Minute)
	t.Setenv("AIVOICEBUS_ME", "air/claude.q3")
	runHook(t, "")
	if n := escalations(s, task.ID); n != 1 {
		t.Errorf("%d messages told the person after a third session was shown it, want 1", n)
	}
}

func escalations(s interface{ Thread(string) []msg.Message }, id string) int {
	n := 0
	for _, m := range s.Thread(id) {
		if msg.IsEscalation(m) {
			n++
		}
	}
	return n
}

func containsPersonTag(ms []msg.Message, id string) bool {
	for _, m := range ms {
		if m.ID == id && m.To == "air/human" && strings.HasPrefix(m.Body, msg.PersonTag) {
			return true
		}
	}
	return false
}
