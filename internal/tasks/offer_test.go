package tasks

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestWhatThePersonIsToldNamesTheTaskAndWhatToRun(t *testing.T) {
	now := time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)
	task := msg.Message{ID: "0123456789", From: "desk/claude.z9", To: "lap", At: now.Add(-42 * time.Minute), Body: "roll \"0.13.2\" out\nto the nodes"}
	got := EscalationBody(task, 3, now)
	for _, want := range []string{msg.PersonTag + " Nobody took task 0123456789", "desk/claude.z9 sent to lap 42m ago", "offered it to 3 session(s)",
		`It says: "roll '0.13.2' out to the nodes".`, "aivoicebus claim 0123456789", "aivoicebus status 0123456789"} {
		if !strings.Contains(got, want) {
			t.Errorf("escalation lacks %q:\n%s", want, got)
		}
	}
	task.Body = msg.SealedPrefix + "c2VhbGVk"
	if got := EscalationBody(task, 0, now); strings.Contains(got, "It says") || !strings.Contains(got, "to 1 session(s)") {
		t.Errorf("a sealed task is quoted, or nobody counted: %s", got)
	}
}

func TestAWatchAlertCountsTheOffersButQuotesNothing(t *testing.T) {
	now := time.Now()
	s := Stuck{ID: "0123456789", From: "desk/claude.z9", To: "lap", At: now.Add(-45 * time.Minute)}
	if got := AlertText(s, now); strings.Contains(got, "offered") {
		t.Errorf("an alert for a task with no offers: %s", got)
	}
	s.Offered, s.Declined, s.Told = 3, 1, true
	got := AlertText(s, now)
	if !strings.Contains(got, "(sent by desk/claude.z9; offered to 3 session(s) one at a time, 1 declined; the person there was told). aivoicebus status 0123456789") {
		t.Errorf("alert: %s", got)
	}
}
