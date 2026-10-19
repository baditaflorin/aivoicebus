package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/worker"
)

// phone is a notify URL that records what is posted to it.
func phone(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts = append(posts, r.Header.Get("Title")+"\n"+string(b))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AIVOICEBUS_NOTIFY_URL", srv.URL)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), posts...)
	}
}

const peerSecret = "rotate the zebra-plan on box9" // task text: a peer's words, never on a phone

// A stuck agent's sender hears it as progress in the task's thread, and the person gets the whole
// reason in their inbox and a short summary on the phone that carries none of the task's text.
func TestStuckTellsTheSenderAndThePerson(t *testing.T) {
	mesh(t)
	posts := phone(t)
	c, _ := client.FromEnv()
	id := sendTask(t, boss, "lap", peerSecret)
	t.Setenv("AIVOICEBUS_ME", sb)
	if code, _, e := run(t, "", "claim", id); code != 0 {
		t.Fatalf("claim: %s", e)
	}
	reason := "waiting for my person's yes before I touch production; " + strings.Repeat("the long tail of the reason ", 6) + "END-OF-REASON"
	code, out, e := run(t, "", "stuck", "--re", id, reason)
	if code != 0 || !strings.Contains(out, "told "+boss) || !strings.Contains(out, "told the person") || !strings.Contains(out, "short summary went to the phone") {
		t.Fatalf("stuck: %d\n%s\n%s", code, out, e)
	}
	ms, _ := c.Thread(id)
	var side *msg.Message
	for i := range ms {
		if ms[i].Kind == msg.KindProgress && ms[i].From == sb {
			side = &ms[i]
		}
	}
	if side == nil || side.To != boss || side.Re != id || !msg.IsStuck(side.Body) || !strings.Contains(side.Body, "END-OF-REASON") {
		t.Fatalf("the sender's copy: %+v", side)
	}
	in, _ := c.Inbox(msg.HumanOf(sb), false)
	if len(in) != 1 || !msg.IsStuck(in[0].Body) || !strings.Contains(in[0].Body, "END-OF-REASON") || !strings.Contains(in[0].Body, id) || in[0].Prio != msg.PrioUrgent {
		t.Fatalf("the person's inbox: %+v", in)
	}
	p := posts()
	if len(p) != 1 || !strings.Contains(p[0], "is stuck on task "+id) || !strings.Contains(p[0], "aivoicebus answer "+in[0].ID) {
		t.Fatalf("phone: %q", p)
	}
	if strings.Contains(p[0], "zebra") || strings.Contains(p[0], "END-OF-REASON") {
		t.Errorf("the phone got more than the summary: %q", p[0])
	}
	// The person lists it and can answer it at a terminal.
	if code, out, _ := run(t, "", "answer"); code != 0 || !strings.Contains(out, in[0].ID) || !strings.Contains(out, "is stuck") {
		t.Errorf("answer lists it: %d %s", code, out)
	}
	// The sender's wait says who is stuck, not "took it".
	var b strings.Builder
	claimProgress(Streams{Err: &b})(*side)
	if !strings.Contains(b.String(), sb+" is STUCK: \"waiting for my person's yes") {
		t.Errorf("wait shows: %s", b.String())
	}
}

// Without --re only the person hears it; a reason is required; --re must be a whole id.
func TestStuckWithoutAThreadAndItsUsage(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", sb)
	c, _ := client.FromEnv()
	if code, out, e := run(t, "", "stuck", "the build host is down"); code != 0 || !strings.Contains(out, "told the person") || strings.Contains(out, "phone") {
		t.Fatalf("%d %s %s", code, out, e)
	}
	if in, _ := c.Inbox(msg.HumanOf(sb), false); len(in) != 1 {
		t.Errorf("inbox: %+v", in)
	}
	if code, _, _ := run(t, "", "stuck"); code != exitUsage {
		t.Errorf("no reason: %d", code)
	}
	if code, _, _ := run(t, "", "stuck", "--re", "abc", "x"); code != exitUsage {
		t.Errorf("short id: %d", code)
	}
	if code, _, _ := run(t, "", "stuck", "--need", "two words", "x"); code != exitUsage {
		t.Errorf("bad need: %d", code)
	}
}

// --need peer goes to a session listening now (not the person); --need WORD to one listening with
// that tool or capability, and to the person when none is.
func TestStuckNeedPeerGoesToAListeningSession(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	const helper = "box/codex.l2"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // hold a read open again and again, as a session standing by does
		_, g, err := c.InboxWait(ctx, helper, 0, false, 0)
		for err == nil && ctx.Err() == nil {
			_, g, err = c.InboxWait(ctx, helper, g, true, 30*time.Second)
		}
	}()
	time.Sleep(800 * time.Millisecond)
	t.Setenv("AIVOICEBUS_ME", sb)
	code, out, e := run(t, "", "stuck", "--need", "peer", "cannot reach the staging database")
	if code != 0 || !strings.Contains(out, "asked "+helper) || strings.Contains(out, "told the person") {
		t.Fatalf("peer: %d %s %s", code, out, e)
	}
	in, _ := c.Inbox(helper, false)
	if len(in) != 1 || in[0].Kind != msg.KindTask || !msg.IsStuck(in[0].Body) || !strings.Contains(in[0].Body, "staging database") {
		t.Fatalf("the helper got: %+v", in)
	}
	if h, _ := c.Inbox(msg.HumanOf(sb), false); len(h) != 0 {
		t.Errorf("the person was told too: %+v", h)
	}
	time.Sleep(1500 * time.Millisecond) // the helper's next read is held open again
	if code, out, _ := run(t, "", "stuck", "--need", "codex", "need a second pair of eyes on the diff"); code != 0 || !strings.Contains(out, "asked "+helper) {
		t.Errorf("by tool: %d %s", code, out)
	}
	code, out, e = run(t, "", "stuck", "--need", "gpu", "need a gpu for the eval")
	if code != 0 || !strings.Contains(e, "no session with gpu is listening right now: told the person instead") || !strings.Contains(out, "told the person") {
		t.Errorf("nobody with gpu: %d %s %s", code, out, e)
	}
}

// claimAs takes the next task that reaches who, as who (the env names the test's sender meanwhile),
// and returns its id ("" after d).
func claimAs(c *client.Client, who string, d time.Duration) string {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		ms, _ := c.Inbox(who, false)
		for _, m := range ms {
			if m.Kind == msg.KindTask {
				if _, err := c.Send(msg.Message{ID: msg.NewID(), From: who, To: m.From, Re: m.ID, Thread: m.Thread, Kind: msg.KindClaim, Body: "on it"}); err == nil {
					return m.ID
				}
			}
		}
	}
	return ""
}

// A task taken and then silent for --quiet-after is reported once to the waiting sender.
func TestAWaitSaysOnceWhenATakenTaskGoesSilent(t *testing.T) {
	mesh(t)
	present(t, sa)
	c, _ := client.FromEnv()
	go claimAs(c, sa, 5*time.Second)
	t.Setenv("AIVOICEBUS_ME", boss)
	code, _, e := run(t, "", "task", "lap", "silent one", "--wait-reply", "6s", "--quiet-after", "2s")
	if code != exitTimeout || strings.Count(e, "has been silent since") != 1 || !strings.Contains(e, "was taken by "+sa) {
		t.Fatalf("%d\n%s", code, e)
	}
}

// Progress heartbeats keep a taken task from looking silent: no alert, and the answer arrives.
func TestHeartbeatsKeepATakenTaskFromLookingSilent(t *testing.T) {
	mesh(t)
	present(t, sa)
	c, _ := client.FromEnv()
	go func() {
		id := claimAs(c, sa, 5*time.Second)
		if id == "" {
			return
		}
		ms, _ := c.Thread(id)
		for i := 0; i < 12; i++ { // a heartbeat every 500ms, well inside --quiet-after
			time.Sleep(500 * time.Millisecond)
			c.Send(msg.Message{ID: msg.NewID(), From: sa, To: boss, Re: id, Thread: ms[0].Thread, Kind: msg.KindProgress, Body: "still on it " + time.Now().Format("05.000")})
		}
		c.Send(msg.Message{ID: msg.NewID(), From: sa, To: boss, Re: id, Thread: ms[0].Thread, Kind: msg.KindAnswer, Body: "done after the heartbeats"})
	}()
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "task", "lap", "busy one", "--wait-reply", "20s", "--quiet-after", "2s")
	if code != 0 || !strings.Contains(out, "done after the heartbeats") || strings.Contains(e, "silent") {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
}

// With --fallback, a task taken and then silent moves on to the next machine, which answers.
func TestASilentTaskMovesOnToTheFallback(t *testing.T) {
	mesh(t)
	present(t, sa)
	c, _ := client.FromEnv()
	const helper = "desk2/claude.q7"
	go claimAs(c, sa, 5*time.Second)
	go func() {
		for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			ms, _ := c.Inbox(helper, false)
			for _, m := range ms {
				if m.Kind == msg.KindTask {
					c.Send(msg.Message{ID: msg.NewID(), From: helper, To: m.From, Re: m.ID, Thread: m.Thread, Kind: msg.KindAnswer, Body: "done on desk2"})
					return
				}
			}
		}
	}()
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "task", "lap", "build the site", "--wait-reply", "30s", "--claim-within", "5s", "--quiet-after", "2s", "--fallback", "desk2")
	if code != 0 || !strings.Contains(out, "done on desk2") || strings.Count(e, "has been silent since") != 1 || !strings.Contains(e, "said nothing for 2s: took") {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	first, _ := sentID(t, out)
	if ms, _ := c.Thread(first); len(ms) == 0 || ms[0].Retracted == nil {
		t.Errorf("the silent task was not taken back")
	}
	if code, _, _ := run(t, "", "task", "lap", "x", "--quiet-after", "-1s", "--wait-reply", "1m"); code != exitUsage {
		t.Errorf("negative --quiet-after: %d", code)
	}
}

// watch alerts once on a task taken and then silent, never on one with recent progress or an
// answer, and the alert names ids and sessions only.
func TestWatchAlertsOnceOnATakenSilentTask(t *testing.T) {
	mesh(t)
	posts := phone(t)
	c, _ := client.FromEnv()
	silent := sendTask(t, boss, "lap", peerSecret)
	busy := sendTask(t, boss, "lap", "busy task text")
	done := sendTask(t, boss, "lap", "answered task text")
	t.Setenv("AIVOICEBUS_ME", sa)
	for _, id := range []string{silent, busy, done} {
		if code, _, e := run(t, "", "claim", "--settle", "0s", id); code != 0 {
			t.Fatalf("claim %s: %s", id, e)
		}
	}
	if code, _, e := run(t, "", "reply", "--re", done, "here it is"); code != 0 {
		t.Fatalf("answer: %s", e)
	}
	time.Sleep(2500 * time.Millisecond)
	if code, _, e := run(t, "", "progress", "--re", busy, "zebra progress words"); code != 0 {
		t.Fatalf("progress: %s", e)
	}
	t.Setenv("AIVOICEBUS_ME", boss)
	var out strings.Builder
	n, err := watchOnce(c, time.Hour, 2*time.Second, &out)
	if err != nil || n != 1 || !strings.Contains(out.String(), "task "+silent+" to lap was taken by "+sa) || !strings.Contains(out.String(), "has been silent since") {
		t.Fatalf("%d %v\n%s", n, err, out.String())
	}
	p := posts()
	if len(p) != 1 || !strings.Contains(p[0], silent) || strings.Contains(p[0], "zebra") {
		t.Errorf("notify: %q", p)
	}
	if n, _ := watchOnce(c, time.Hour, 2*time.Second, io.Discard); n != 0 {
		t.Errorf("alerted twice: %d", n)
	}
}

// The worker's heartbeat is well inside the default quiet-after, so a running task never trips it.
func TestTheWorkerHeartbeatIsInsideQuietAfter(t *testing.T) {
	if worker.HeartbeatEvery*3 > tasks.QuietAfter {
		t.Errorf("heartbeat every %s, quiet after %s", worker.HeartbeatEvery, tasks.QuietAfter)
	}
}
