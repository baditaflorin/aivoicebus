package view

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestADigestSaysWhoTookItTheLastWordsAndWhetherItEnded(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	m := func(id, from, to, kind, body string, d time.Duration) msg.Message {
		return msg.Message{ID: id, From: from, To: to, Re: "t000000001", Thread: "t000000001", Kind: kind, Body: body, At: t0.Add(d)}
	}
	task := msg.Message{ID: "t000000001", From: "lap/claude.a1", To: "build", Thread: "t000000001", Kind: msg.KindTask, Body: "run the integration suite\nand report", At: t0}
	ms := []msg.Message{
		task,
		m("c000000001", "build/codex.b2", "lap/claude.a1", msg.KindClaim, "on it", time.Minute),
		m("r000000001", "build/codex.b2", "lap/claude.a1", "", "2 fail: login, cookie", 5*time.Minute),
		m("r000000002", "lap/claude.a1", "build/codex.b2", "", "fixed in 4f2a, rerun please", 7*time.Minute),
	}
	var b strings.Builder
	Digest(&b, ms)
	out := b.String()
	for _, want := range []string{"UNTRUSTED", "4 message(s) from 2 session(s)", "task by lap/claude.a1 -> build: \"run the integration suite and report\"",
		"taken by build/codex.b2", "size: 4 message(s), 89 characters (about 22 tokens to read)", "last from build/codex.b2 (2, ", "\"2 fail: login, cookie\"", "last from lap/claude.a1 (2, ", "open: the last word is lap/claude.a1's"} {
		if !strings.Contains(out, want) {
			t.Errorf("digest lacks %q:\n%s", want, out)
		}
	}
	ms = append(ms, m("e000000001", "build/codex.b2", "lap/claude.a1", msg.KindEnd, "all green", 9*time.Minute))
	b.Reset()
	Digest(&b, ms)
	if !strings.Contains(b.String(), "ended by build/codex.b2") || !strings.Contains(b.String(), "\"all green\"") {
		t.Errorf("ended:\n%s", b.String())
	}
	b.Reset()
	Digest(&b, ms[:1])
	if !strings.Contains(b.String(), "nobody has taken it") {
		t.Errorf("untaken:\n%s", b.String())
	}
	// a task its sender withdrew (task --fallback moved it on) is not "open" (found live)
	gone := task
	gone.Retracted = &msg.Retraction{By: "lap", At: t0.Add(time.Minute)}
	b.Reset()
	Digest(&b, []msg.Message{gone})
	if out := b.String(); !strings.Contains(out, "withdrawn by its sender") || strings.Contains(out, "open:") || strings.Contains(out, "nobody has taken it") {
		t.Errorf("withdrawn:\n%s", out)
	}
}
