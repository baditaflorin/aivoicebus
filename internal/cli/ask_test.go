package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
)

// standBy is a session standing by as me (a read held open, so the nodes count it as listening)
// that hands the first unread task reaching it to act.
func standBy(t *testing.T, c *client.Client, me string, act func(m msg.Message)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		var gen uint64
		has := false
		for ctx.Err() == nil {
			ms, g, err := c.InboxWait(ctx, me, gen, has, 20*time.Second)
			if err != nil {
				return
			}
			gen, has = g, true
			for _, m := range ms {
				if m.Kind == msg.KindTask {
					act(m)
					return
				}
			}
		}
	}()
	time.Sleep(800 * time.Millisecond)
}

// polled is a session that is seen (it reads its inbox every 100ms) and hands the first unread
// task reaching it to act. act marks it read if it should.
func polled(t *testing.T, c *client.Client, me string, act func(m msg.Message)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			ms, _ := c.Inbox(me, false)
			for _, m := range ms {
				if m.Kind == msg.KindTask {
					act(m)
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	time.Sleep(300 * time.Millisecond)
}

// answering marks a task read and answers it, as a session standing by does.
func answering(c *client.Client, me, body string) func(msg.Message) {
	return func(m msg.Message) {
		c.AckVia(m.ID, me, msg.ViaCLI, 0)
		c.Send(msg.Message{ID: msg.NewID(), From: me, To: m.From, Re: m.ID, Thread: m.Thread, Kind: msg.KindAnswer, Body: body})
	}
}

// The session listening now is asked (not the one only seen), with the conversation limits, and
// the one command prints its answer.
func TestAskGetsTheAnswerFromTheSessionListeningNow(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	present(t, sa) // seen just now, not listening
	standBy(t, c, "lap/codex.l1", answering(c, "lap/codex.l1", "42, from codex"))
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "ask", "lap", "what is 6*7?")
	if code != 0 || !strings.Contains(out, "42, from codex") || !strings.Contains(e, "ask: lap/codex.l1: listening now") {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	id, _ := sentID(t, out)
	ms, _ := c.Thread(id)
	if len(ms) == 0 || ms[0].To != "lap/codex.l1" || ms[0].Kind != msg.KindTask || ms[0].MaxTurns != tasks.AskMaxTurns || ms[0].Deadline != int64(tasks.AskDeadline/time.Second) {
		t.Fatalf("the question did not carry the limits: %+v", ms)
	}
	// --json: one object with the outcome and why that session.
	standBy(t, c, "lap/codex.l1", answering(c, "lap/codex.l1", "still 42"))
	code, out, e = run(t, "", "ask", "lap/codex", "and 7*6?", "--json")
	var r resultView
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || r.Outcome != "answered" || !strings.Contains(r.Why, "listening now") ||
		r.TakenBy != "lap/codex.l1" || len(r.Answers) != 1 || r.Answers[0].Body != "still 42" {
		t.Fatalf("json: %d %v\n%s\n%s", code, err, out, e)
	}
}

// Nobody there: nothing is sent (exit 3). Somebody seen who does not take it in --claim-within:
// the question is taken back (exit 3), not left waiting.
func TestAskExitsThreeWhenNobodyTakesIt(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "ask", "nowhere", "anyone?")
	if code != exitNotFound || out != "" || !strings.Contains(e, "nobody took it: nothing was sent") {
		t.Fatalf("nobody there: %d %q %q", code, out, e)
	}
	code, out, _ = run(t, "", "ask", "nowhere", "anyone?", "--json")
	var r resultView
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != exitNotFound || r.Outcome != "nobody" || r.Answered {
		t.Fatalf("nobody there, json: %d %v %s", code, err, out)
	}
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e = run(t, "", "ask", "lap", "is the build green?", "--claim-within", "5s", "--wait-reply", "20s")
	if code != exitNotFound || !strings.Contains(e, "ask: lap/claude.a1: nobody there is listening") || !strings.Contains(e, "nobody took it within 5s") {
		t.Fatalf("not taken: %d\n%s\n%s", code, out, e)
	}
	id, _ := sentID(t, out)
	if ms, _ := c.Thread(id); len(ms) == 0 || ms[0].Retracted == nil {
		t.Errorf("the question was not taken back: %+v", ms)
	}
}

// Taken (claimed, or read by the one session asked) but not answered in --wait-reply: exit 6,
// and the question stays with them.
func TestAskExitsSixWhenTakenButNotAnswered(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	present(t, sa)
	polled(t, c, sa, func(m msg.Message) { // claims it (as sa, whatever this process's identity is), never answers
		c.Send(msg.Message{ID: msg.NewID(), From: sa, To: m.From, Re: m.ID, Thread: m.Thread, Kind: msg.KindClaim, Body: "on it"})
	})
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "ask", "lap/claude", "how long is the suite?", "--claim-within", "5s", "--wait-reply", "8s")
	if code != exitTimeout || !strings.Contains(e, "took it") || !strings.Contains(e, "no answer in 8s") {
		t.Fatalf("claimed: %d\n%s\n%s", code, out, e)
	}
	present(t, sb)
	polled(t, c, sb, func(m msg.Message) { c.AckVia(m.ID, sb, msg.ViaCLI, 0) })
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e = run(t, "", "ask", sb, "and how long is lint?", "--claim-within", "5s", "--wait-reply", "8s")
	if code != exitTimeout || !strings.Contains(e, "ask: lap/codex.b2: named") || !strings.Contains(e, "lap/codex.b2 read it; still waiting") {
		t.Fatalf("read: %d\n%s\n%s", code, out, e)
	}
	id, _ := sentID(t, out)
	if ms, _ := c.Thread(id); len(ms) == 0 || ms[0].Retracted != nil {
		t.Errorf("a question that was read is not taken back: %+v", ms)
	}
}

// Two sessions nothing tells apart: ask lists both and refuses (exit 2); --pick N asks one.
func TestAskRefusesLookAlikesUnlessPicked(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	present(t, "m1/claude.bb")
	present(t, "m1/claude.aa")
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "ask", "m1", "which branch?")
	if code != exitUsage || out != "" || !strings.Contains(e, "2 sessions at m1 look alike") || !strings.Contains(e, "1  m1/claude.aa") ||
		!strings.Contains(e, "2  m1/claude.bb") || !strings.Contains(e, "--pick N") {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	code, out, e = run(t, "", "ask", "m1", "which branch now?", "--pick", "2", "--claim-within", "5s", "--wait-reply", "6s")
	if code != exitNotFound || !strings.Contains(e, "ask: m1/claude.bb: --pick 2 of 2") {
		t.Fatalf("--pick: %d\n%s\n%s", code, out, e)
	}
	id, _ := sentID(t, out)
	if ms, _ := c.Thread(id); len(ms) == 0 || ms[0].To != "m1/claude.bb" {
		t.Errorf("--pick 2 went elsewhere: %+v", ms)
	}
	for _, args := range [][]string{
		{"ask", "m1", "q", "--max-turns", "1"},
		{"ask", "m1", "q", "--wait-reply", "20m"},
		{"ask", "@repo=x", "q"},
		{"ask", "all", "q"},
		{"ask", "--any"},
		{"ask", "m1"},
		{"ask", "m1/claude", "q", "--tool", "codex"},
		{"ask", "--any", "q", "--fallback", "m2"},
	} {
		if code, _, e := run(t, "", args...); code != exitUsage {
			t.Errorf("%v: %d %s", args, code, e)
		}
	}
}

// --fallback as on task: nobody at the first machine takes it, so it is taken back and asked at
// the next one (of the session picked there), with the same limits.
func TestAskMovesOnToTheFallback(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	present(t, sa)
	const helper = "desk2/claude.q7"
	polled(t, c, helper, answering(c, helper, "done on desk2"))
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "ask", "lap", "build the docs?", "--claim-within", "5s", "--wait-reply", "40s", "--fallback", "desk2")
	if code != 0 || !strings.Contains(out, "done on desk2") || !strings.Contains(e, "gave it to desk2/claude.q7") {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	first, _ := sentID(t, out)
	if ms, _ := c.Thread(first); len(ms) == 0 || ms[0].Retracted == nil {
		t.Errorf("the first copy was not taken back: %+v", ms)
	}
	ms, _ := c.Inbox(helper, true)
	var moved *msg.Message
	for i := range ms {
		if ms[i].Kind == msg.KindTask {
			moved = &ms[i]
		}
	}
	if moved == nil || moved.MaxTurns != tasks.AskMaxTurns || moved.Deadline == 0 {
		t.Errorf("the moved copy lost the limits: %+v", moved)
	}
}

// A short --wait-reply with the default --claim-within used to stop ask with a usage error (the
// 2m default was not shorter than the wait): found live on 0.11.0 with `ask HOST "q" --wait-reply 2m`.
func TestAskFitsItsDefaultClaimWaitInsideAShortReplyWait(t *testing.T) {
	for _, c := range []struct {
		set         bool
		claim, wait time.Duration
		want        time.Duration
	}{
		{false, 2 * time.Minute, 15 * time.Minute, 2 * time.Minute}, // the default fits: kept
		{false, 2 * time.Minute, 2 * time.Minute, time.Minute},      // equal: half the wait
		{false, 2 * time.Minute, 6 * time.Second, 5 * time.Second},  // never under 5s
		{true, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute},   // set by the person: kept, checked later
	} {
		if got := askClaimWithin(c.set, c.claim, c.wait); got != c.want {
			t.Errorf("askClaimWithin(%v, %s, %s) = %s, want %s", c.set, c.claim, c.wait, got, c.want)
		}
	}
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	code, _, e := run(t, "", "ask", "lap", "a short one?", "--wait-reply", "6s")
	if code == exitUsage || strings.Contains(e, "claim-within is at least") {
		t.Fatalf("a short --wait-reply alone must work: %d %q", code, e)
	}
}
