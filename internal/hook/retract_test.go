package hook

import (
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// A retraction is applied where mail is read, so the hooks need no code of their own: these tests
// pin that they stay silent about a withdrawn message however it was marked, and that withdrawing
// one after it was shown gives one notice (urgent for a leak), which the hook shows and acks like any
// other message.

func TestARetractedUnreadMessageIsNotPrintedAtSessionStart(t *testing.T) {
	s, _ := mesh(t)
	for _, prio := range []string{msg.PrioFYI, msg.PrioAsk, msg.PrioUrgent} {
		m, _ := s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "withdrawn " + prio, Prio: prio})
		if _, err := s.Retract(m.ID, "other/agent", false); err != nil {
			t.Fatal(err)
		}
	}
	if got := runHook(t, ""); got != "" {
		t.Fatalf("nothing is left to say: %q", got)
	}

	// next to one that stands, only that one is counted and shown
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "this one stands"})
	got := runHook(t, "")
	if !strings.Contains(got, "1 unread message(s)") || !strings.Contains(got, "this one stands") || strings.Contains(got, "withdrawn") || strings.Contains(got, "fyi") {
		t.Errorf("%q", got)
	}
}

func TestARetractedUrgentMessageIsNotPrintedByThePerPromptHook(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	u, _ := s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "prod is down, stop everything", Prio: msg.PrioUrgent})
	if _, err := s.Retract(u.ID, "other/agent", true); err != nil { // even one flagged as a leak: the hook has nothing to show
		t.Fatal(err)
	}
	if got := promptHook(t, ""); got != "" {
		t.Fatalf("the prompt hook prints nothing for a withdrawn message: %q", got)
	}
	// and a live urgent message beside it is still shown
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "CI is red on main", Prio: msg.PrioUrgent})
	if got := promptHook(t, ""); !strings.Contains(got, "CI is red on main") || strings.Contains(got, "prod is down") {
		t.Errorf("%q", got)
	}
}

func TestAMessageRetractedAfterItWasShownGivesOneNoticeAtTheNextSessionStart(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	m, _ := s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "deploy at five", Prio: msg.PrioUrgent})
	if got := promptHook(t, ""); !strings.Contains(got, "deploy at five") {
		t.Fatalf("shown once: %q", got)
	}
	res, err := s.Retract(m.ID, "other/agent", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReadBy != 1 {
		t.Errorf("the receipt of the hook's showing stands: it is how many sessions read it: %+v", res)
	}
	if got := promptHook(t, ""); got != "" {
		t.Errorf("a plain retraction's notice is not urgent: it waits for the session start: %q", got)
	}
	got := runHook(t, "")
	want := "[retracted] other withdrew message " + m.ID
	if !strings.Contains(got, "1 unread message(s)") || !strings.Contains(got, want) || !strings.Contains(got, msg.NoticeFrom) ||
		!strings.Contains(got, "Disregard it and do not act on it.") || strings.Contains(got, "deploy at five") {
		t.Errorf("the next session start tells it, without the words: %q", got)
	}
	if got := runHook(t, ""); got != "" {
		t.Errorf("once: the hook acked the notice: %q", got)
	}
	if all := s.Inbox("air/codex", true); len(all) != 1 || all[0].Retracted == nil || strings.Contains(all[0].Body, "deploy at five") {
		t.Errorf("only the tombstone remains to be listed: %+v", all)
	}
}

func TestALeakNoticeIsUrgentThroughThePerPromptHookOnce(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	m, _ := s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "try password swordfish"})
	if got := runHook(t, ""); !strings.Contains(got, "swordfish") {
		t.Fatalf("shown at session start: %q", got)
	}
	if _, err := s.Retract(m.ID, "other/agent.b", true); err != nil {
		t.Fatal(err)
	}
	got := promptHook(t, "")
	if !strings.Contains(got, "1 URGENT message(s)") || !strings.Contains(got, "withdrew message "+m.ID) ||
		!strings.Contains(got, "treat it as exposed and tell the user") || strings.Contains(got, "swordfish") {
		t.Errorf("the next prompt is told at once: %q", got)
	}
	if got := promptHook(t, ""); got != "" {
		t.Errorf("then silent: %q", got)
	}
	if got := runHook(t, ""); got != "" {
		t.Errorf("and the next session start too: %q", got)
	}
}

func TestAnFYIOnlyCountedAtSessionStartGivesNoNotice(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	m, _ := s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "fyi: the key is swordfish", Prio: msg.PrioFYI})
	if got := runHook(t, ""); !strings.Contains(got, "1 low-priority (fyi)") || strings.Contains(got, "swordfish") {
		t.Fatalf("counted, not shown: %q", got)
	}
	s.Retract(m.ID, "other/agent", true)
	if got := promptHook(t, "") + runHook(t, ""); got != "" {
		t.Errorf("the session never saw the text: nothing to retract from it: %q", got)
	}
}
