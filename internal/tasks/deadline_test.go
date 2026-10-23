package tasks

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

func TestDeadlineLinesNameIdsAndSessionsAndSayWhatItCameTo(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 41, 0, 0, time.UTC)
	by := now.Add(-time.Minute)
	one := 1
	d := outbox.Deadline{ID: "aaaaaaaaaa", Thread: "bbbbbbbbbb", From: "desk/claude.z9", To: "lap/claude.a1", Kind: msg.DeadlineReply, Sent: by.Add(-15 * time.Minute), By: by}
	cases := []struct {
		name  string
		d     outbox.Deadline
		j     msg.Judgement
		want  []string
		alarm bool
	}{
		{"nobody read it", d, msg.Judgement{Outcome: msg.OutcomeNobodyRead, By: by, Of: &one}, []string{"message aaaaaaaaaa to lap/claude.a1", "read by 0 of 1, replies 0", "nobody read it, so silence is not consent", "aivoicebus status aaaaaaaaaa"}, true},
		{"read and silent", d, msg.Judgement{Outcome: msg.OutcomeNoObjection, By: by, Read: 2, Of: &one}, []string{"read by 2 of 1, replies 0", "it was read and nobody objected"}, false},
		{"a reply", d, msg.Judgement{Outcome: msg.OutcomeObjected, By: by, Read: 1, Replies: 1}, []string{"read by 1, replies 1", "somebody replied (aivoicebus thread bbbbbbbbbb)"}, true},
		{"a late read", d, msg.Judgement{Outcome: msg.OutcomeNobodyRead, By: by, LateReads: 1}, []string{"read by 0 (and 1 only after the deadline), replies 0"}, true},
		{"not due yet", d, msg.Judgement{Outcome: msg.OutcomePending, By: now.Add(4 * time.Minute)}, []string{"reply-by", "away", "so far"}, false},
	}
	for _, c := range cases {
		line := DeadlineText(c.d, c.j, now)
		for _, w := range c.want {
			if !strings.Contains(line, w) {
				t.Errorf("%s: %q lacks %q", c.name, line, w)
			}
		}
		if Alarming(c.j) != c.alarm {
			t.Errorf("%s: Alarming = %v", c.name, !c.alarm)
		}
		if strings.Contains(line, "\n") {
			t.Errorf("%s: one line: %q", c.name, line)
		}
	}
	offer := outbox.Deadline{ID: "cccccccccc", Thread: "cccccccccc", From: "desk/claude.z9", To: "lap", Kind: msg.DeadlineTake, Sent: by.Add(-20 * time.Minute), By: by}
	if l := DeadlineText(offer, msg.Judgement{Outcome: msg.OutcomeNoTaker, By: by}, now); !strings.Contains(l, "no taker") || !strings.Contains(l, "(20m)") || !strings.Contains(l, "not doing it") || !Alarming(msg.Judgement{Outcome: msg.OutcomeNoTaker}) {
		t.Errorf("an offer nobody took: %q", l)
	}
	if l := DeadlineText(offer, msg.Judgement{Outcome: msg.OutcomeTaken, By: by, TakenBy: "lap/codex.b2"}, now); !strings.Contains(l, "taken by lap/codex.b2") || Alarming(msg.Judgement{Outcome: msg.OutcomeTaken}) {
		t.Errorf("an offer that was taken: %q", l)
	}
	if hostile := DeadlineText(outbox.Deadline{ID: "aaaaaaaaaa", To: "lap/‮claude", Kind: msg.DeadlineReply}, msg.Judgement{Outcome: msg.OutcomeNobodyRead, By: by}, now); strings.ContainsRune(hostile, '‮') {
		t.Errorf("a name that only looks like another is made visible: %q", hostile)
	}
}

func TestAReminderSaysWhoWhatAndHowToAnswerInOneLine(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 41, 0, 0, time.UTC)
	d := outbox.Deadline{ID: "aaaaaaaaaa", Thread: "bbbbbbbbbb", From: "desk/claude.z9", To: "lap/claude.a1", Kind: msg.DeadlineReply, By: now.Add(-time.Minute)}
	b := ReminderBody(d, "force-pushing\nbranch x\x07 "+strings.Repeat("y", 900), now)
	for _, want := range []string{"[reminder]", "desk/claude.z9", "lap/claude.a1", "aaaaaaaaaa", "thread bbbbbbbbbb", "15:40:00Z", "nobody has read it", "aivoicebus reply --re aaaaaaaaaa"} {
		if !strings.Contains(b, want) {
			t.Errorf("reminder lacks %q: %s", want, b)
		}
	}
	if strings.ContainsAny(b, "\n\x07") || len(b) > 1000 {
		t.Errorf("one short line: %d %q", len(b), b)
	}
	if !strings.HasPrefix(b, "[reminder]") {
		t.Error("the reminder starts with its tag, so a reader can tell it from the message")
	}
	if utcClock(now.Add(-48*time.Hour), now) != "2026-10-20 15:41:00Z" {
		t.Errorf("a time on another day carries its date: %s", utcClock(now.Add(-48*time.Hour), now))
	}
}

func TestAReminderForAPrivateMessageRepeatsNoWords(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 41, 0, 0, time.UTC)
	d := outbox.Deadline{ID: "aaaaaaaaaa", Thread: "bbbbbbbbbb", From: "desk/claude.z9", To: "lap/claude.a1", Kind: msg.DeadlineReply, By: now}
	if b := ReminderBody(d, "", now); strings.Contains(b, "It said") || !strings.Contains(b, "nobody has read it") || !strings.Contains(b, "aivoicebus reply --re aaaaaaaaaa") {
		t.Errorf("%s", b)
	}
}

func TestSpanIsShort(t *testing.T) {
	for in, want := range map[time.Duration]string{15 * time.Minute: "15m", 25 * time.Hour: "25h", 90 * time.Second: "1m30s", 2*time.Hour + 5*time.Minute: "2h5m", 1500 * time.Millisecond: "2s", 24 * time.Hour: "24h"} {
		if got := Span(in); got != want {
			t.Errorf("Span(%v) = %q, want %q", in, got, want)
		}
	}
}
