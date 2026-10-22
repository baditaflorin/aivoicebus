package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// A sender that asks for a deadline finds out what became of the message instead of taking silence for
// consent. These tests run real nodes in process; the "other" sessions act through the client with their
// own identities while the command under test waits.

// readerOf reads (acknowledges) the first message from `from` that lands in who's inbox, like a session
// that saw it. It returns what it read, on a channel.
func readerOf(t *testing.T, c *client.Client, who, from string, then func(m msg.Message)) <-chan msg.Message {
	t.Helper()
	got := make(chan msg.Message, 1)
	go func() {
		for i := 0; i < 200; i++ {
			ms, _ := c.Inbox(who, false)
			for _, m := range ms {
				if m.From == from && !strings.HasPrefix(m.Body, "[reminder]") {
					_ = c.AckVia(m.ID, who, msg.ViaInbox, 0)
					if then != nil {
						then(m)
					}
					got <- m
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		close(got)
	}()
	return got
}

func deadlineJSON(t *testing.T, out string) deadlineView {
	t.Helper()
	var v deadlineView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not a deadline object: %v\n%s", err, out)
	}
	return v
}

func fastGrace(t *testing.T) {
	t.Helper()
	old := escalateGrace
	escalateGrace = time.Second
	t.Cleanup(func() { escalateGrace = old })
}

func TestReplyByWithNobodyReadingItIsExitEightAndNotConsent(t *testing.T) {
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	start := time.Now()
	code, out, e := run(t, "", "send", "--to", sa, "deploying to prod, reply if you are mid-change", "--reply-by", "2s")
	if code != exitUnseen || exitUnseen != 8 {
		t.Fatalf("exit %d, want 8\nout: %s\nerr: %s", code, out, e)
	}
	if time.Since(start) < 2*time.Second {
		t.Errorf("it did not wait for the deadline: %v", time.Since(start))
	}
	for _, want := range []string{"read by 0 of 1, replies 0", "nobody read it, so silence is not consent", "aivoicebus status"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	id, _ := sentID(t, out[strings.Index(out, "sent "):])
	if d, ok := outbox.DeadlineFor("desk", id); !ok || d.Reported.IsZero() || d.Kind != msg.DeadlineReply {
		t.Errorf("the waiting send reports the deadline it remembered: %+v %v", d, ok)
	}
	code, out, _ = run(t, "", "send", "--to", sa, "a second one", "--reply-by", "1s", "--json")
	v := deadlineJSON(t, out)
	if code != exitUnseen || v.Outcome != msg.OutcomeNobodyRead || v.Exit != 8 || v.Read != 0 || v.Of == nil || *v.Of != 1 || v.Deadline != "reply-by" || v.Line == "" {
		t.Errorf("json: %d %+v", code, v)
	}
}

func TestReplyByReadAndNotAnsweredIsExitZeroAndSaysNobodyObjected(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	readerOf(t, c, sa, boss, nil)
	code, out, e := run(t, "", "send", "--to", sa, "deploying at 15:40Z, reply if you are mid-change", "--reply-by", "3s")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, e)
	}
	for _, want := range []string{"read by 1 of 1, replies 0", "it was read and nobody objected"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
}

func TestAReplyEndsTheWaitAtOnceAndIsAnObjection(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	readerOf(t, c, sa, boss, func(m msg.Message) {
		_, _ = c.Send(msg.Message{ID: msg.NewID(), From: sa, To: boss, Re: m.ID, Thread: m.Thread, Body: "stop, I am mid-change"})
	})
	start := time.Now()
	code, out, e := run(t, "", "send", "--to", sa, "deploying now", "--reply-by", "30s")
	if code != exitFailure || time.Since(start) > 20*time.Second {
		t.Fatalf("exit %d after %v\n%s\n%s", code, time.Since(start), out, e)
	}
	for _, want := range []string{"stop, I am mid-change", "UNTRUSTED", "somebody replied"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
}

func TestEscalateMachineRemindsTheOtherLiveSessionsOnly(t *testing.T) {
	mesh(t)
	fastGrace(t)
	c, _ := client.FromEnv()
	present(t, sa) // the one it was sent to, who does not read it
	present(t, sb) // another session on the same machine
	present(t, "far/claude.f1")
	t.Setenv("AIVOICEBUS_ME", boss)
	go func() { // sb reads the reminder: the escalation sees it worked
		for i := 0; i < 200; i++ {
			ms, _ := c.Inbox(sb, false)
			for _, m := range ms {
				if strings.HasPrefix(m.Body, "[reminder]") {
					_ = c.AckVia(m.ID, sb, msg.ViaHook, 0)
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	code, out, e := run(t, "", "send", "--to", sa, "force-pushing branch x, tell me now if not", "--reply-by", "2s", "--escalate", "machine,person", "--escalate-wait", "1s", "--json")
	v := deadlineJSON(t, out)
	if code != exitUnseen || v.Outcome != msg.OutcomeNobodyRead {
		t.Fatalf("exit %d: %+v\n%s", code, v, e)
	}
	if v.Escalation == nil || len(v.Escalation.Reminded) != 1 || v.Escalation.Reminded[0] != sb || v.Escalation.ReadAfter != 1 {
		t.Fatalf("reminded sb, who read it: %+v", v.Escalation)
	}
	if v.Escalation.Person != nil {
		t.Errorf("somebody read the reminder: the person is not asked: %+v", v.Escalation.Person)
	}
	ms, _ := c.Thread(v.Thread) // sb read it, so it is no longer in an unread listing
	var rem msg.Message
	for _, m := range ms {
		if strings.HasPrefix(m.Body, "[reminder]") {
			rem = m
		}
	}
	if rem.ID == "" || rem.Priority() != msg.PrioUrgent || rem.Re != v.ID || rem.Thread != v.Thread || !strings.Contains(rem.Body, "force-pushing branch x") || !strings.Contains(rem.Body, "nobody has read it") {
		t.Errorf("the reminder is urgent, in the thread, and says what it was: %+v", rem)
	}
	if ms, _ := c.Inbox(sa, false); len(ms) != 1 {
		t.Errorf("the session it was sent to is not reminded: %d messages", len(ms))
	}
	if ms, _ := c.Inbox("far/claude.f1", false); len(ms) != 0 {
		t.Errorf("another machine's session is not reminded: %d", len(ms))
	}
	if ms, _ := c.Inbox(msg.HumanOf(boss), false); len(ms) != 0 {
		t.Errorf("the person is not asked: %d", len(ms))
	}
	if d, _ := outbox.DeadlineFor("desk", v.ID); len(d.Reminders) != 1 || d.Reminders[0] != rem.ID {
		t.Errorf("the reminder is remembered with the deadline: %+v", d)
	}
}

func TestEscalatePersonAsksAndReportsTheirWord(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	var q msg.Message
	var mu sync.Mutex
	go func() { // the person answers in a terminal
		for i := 0; i < 300; i++ {
			ms, _ := c.Inbox(msg.HumanOf(boss), false)
			for _, m := range ms {
				if strings.HasPrefix(m.Body, msg.AskPrefix) {
					mu.Lock()
					q = m
					mu.Unlock()
					_, _ = c.Send(msg.Message{ID: msg.NewID(), From: msg.HumanOf(boss), To: m.From, Re: m.ID, Thread: m.Thread, Kind: msg.KindAnswer, Body: msg.HumanAnswerPrefix + "hold"})
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	code, out, e := run(t, "", "send", "--to", sa, "force-pushing", "--reply-by", "1s", "--escalate", "person", "--escalate-wait", "10s", "--json")
	v := deadlineJSON(t, out)
	if code != exitUnseen || v.Escalation == nil || v.Escalation.Person == nil || !v.Escalation.Person.Answered || v.Escalation.Person.Answer != "hold" || v.Escalation.Person.Via != "terminal" {
		t.Fatalf("exit %d: %+v\n%s", code, v.Escalation, e)
	}
	if len(v.Escalation.Reminded) != 0 {
		t.Errorf("machine was not asked for: %+v", v.Escalation)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(q.Body, v.ID) || !strings.Contains(q.Body, "Proceed anyway") || !strings.Contains(q.Body, "go|hold") || q.Prio != msg.PrioUrgent {
		t.Errorf("the question names the message and offers go|hold: %+v", q)
	}
}

func TestEscalateWithNobodyElseLiveSaysSoAndAsksThePersonWithoutWaiting(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "send", "--to", sa, "force-pushing", "--reply-by", "1s", "--escalate", "person,machine", "--escalate-wait", "0", "--json")
	v := deadlineJSON(t, out)
	if code != exitUnseen || v.Escalation == nil || len(v.Escalation.Reminded) != 0 || v.Escalation.Person == nil || v.Escalation.Person.Answered {
		t.Fatalf("exit %d: %+v\n%s", code, v.Escalation, e)
	}
	joined := strings.Join(v.Escalation.Notes, "\n")
	if !strings.Contains(joined, "nobody to remind") || !strings.Contains(joined, "asked the person") {
		t.Errorf("notes: %s", joined)
	}
	if ms, _ := c.Inbox(msg.HumanOf(boss), false); len(ms) != 1 || !strings.HasPrefix(ms[0].Body, msg.AskPrefix) {
		t.Errorf("the person was asked once: %+v", ms)
	}
}

func TestADetachedDeadlineIsReportedByWatchOnceAndShownByStatus(t *testing.T) {
	mesh(t)
	var mu sync.Mutex
	var posts []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts = append(posts, string(b))
		mu.Unlock()
	}))
	defer hook.Close()
	t.Setenv("AIVOICEBUS_NOTIFY_URL", hook.URL)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "send", "--to", sa, "the secret plan: deploy at once", "--reply-by", "2s", "--detach")
	if code != 0 || !strings.Contains(out, "deadline recorded") {
		t.Fatalf("detach: %d %q %q", code, out, e)
	}
	id, _ := sentID(t, out[strings.Index(out, "sent "):])
	if _, st, _ := run(t, "", "status", id); !strings.Contains(st, "reply-by") || !strings.Contains(st, "away") {
		t.Errorf("status before the deadline:\n%s", st)
	}
	if code, out, _ := run(t, "", "watch", "--once"); code != 0 || strings.Contains(out, "nobody read it") {
		t.Errorf("nothing is due yet: %d %q", code, out)
	}
	time.Sleep(2300 * time.Millisecond)
	code, out, _ = run(t, "", "watch", "--once")
	if code != exitFailure || !strings.Contains(out, id) || !strings.Contains(out, "read by 0 of 1, replies 0") || !strings.Contains(out, "nobody read it") {
		t.Fatalf("watch --once: %d\n%s", code, out)
	}
	mu.Lock()
	if len(posts) != 1 || !strings.Contains(posts[0], id) || strings.Contains(posts[0], "secret plan") {
		t.Errorf("one push that names the message and never its text: %q", posts)
	}
	mu.Unlock()
	if code, out, _ := run(t, "", "watch", "--once"); code != 0 || !strings.Contains(out, "nothing new") {
		t.Errorf("told once: %d %q", code, out)
	}
	if d, _ := outbox.DeadlineFor("desk", id); d.Reported.IsZero() {
		t.Error("watch marks it reported")
	}
	if _, st, _ := run(t, "", "status", id); !strings.Contains(st, "reply-by") || !strings.Contains(st, "passed") || !strings.Contains(st, "nobody read it, so silence is not consent") {
		t.Errorf("status after the deadline:\n%s", st)
	}
	if code, out, _ := run(t, "", "status", "--json", id); code != 0 || !json.Valid([]byte(out)) {
		t.Errorf("status --json stays JSON: %d %q", code, out)
	}
}

func TestWatchSaysNothingOfAGoodDeadlineButSettlesIt(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "send", "--to", sa, "heads up", "--reply-by", "1s", "--detach")
	id, _ := sentID(t, out[strings.Index(out, "sent "):])
	if err := c.AckVia(id, sa, msg.ViaHook, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if code, out, _ := run(t, "", "watch", "--once"); code != 0 || strings.Contains(out, id) {
		t.Errorf("read and unanswered is good news: %d %q", code, out)
	}
	if d, _ := outbox.DeadlineFor("desk", id); d.Reported.IsZero() {
		t.Error("it is settled all the same")
	}
	if _, st, _ := run(t, "", "status", id); !strings.Contains(st, "it was read and nobody objected") {
		t.Errorf("status still says it:\n%s", st)
	}
}

func TestAnOfferIsTakenOrLapsesAndItsTTLIsTheDeadline(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	present(t, sa)
	present(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "task", "lap", "take the 5pm backup", "--until", "1m", "--detach", "--json")
	v := deadlineJSON(t, out)
	if code != 0 || !v.Detached || v.Deadline != "until" {
		t.Fatalf("%d %+v\n%s", code, v, e)
	}
	ms, _ := c.Thread(v.ID)
	if len(ms) != 1 || ms[0].Exp == nil {
		t.Fatalf("the offer is on the nodes with an expiry: %+v", ms)
	}
	if life := ms[0].Exp.Sub(ms[0].At); life < 59*time.Second || life > 61*time.Second {
		t.Errorf("the nodes stop delivering it at the deadline (ttl = until): %v", life)
	}
	d, ok := outbox.DeadlineFor("desk", v.ID)
	if !ok || d.Kind != msg.DeadlineTake {
		t.Errorf("remembered as an offer: %+v", d)
	}
	var buf strings.Builder
	if n, err := watchOnce(c, 0, 0, &buf); n != 0 || err != nil {
		t.Errorf("watch's untaken-task alert leaves an offer with its own deadline alone: %d %v\n%s", n, err, buf.String())
	}
	// taken: a claim ends the wait at once, exit 0
	go func() {
		for i := 0; i < 200; i++ {
			ms, _ := c.Inbox(sa, false)
			for _, m := range ms {
				if m.Kind == msg.KindTask && strings.Contains(m.Body, "second offer") {
					_, _ = c.Send(msg.Message{ID: msg.NewID(), From: sa, To: boss, Re: m.ID, Thread: m.Thread, Kind: msg.KindClaim, Body: "taking this"})
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	code, out, e = run(t, "", "task", sa, "second offer", "--until", "1m", "--json")
	v = deadlineJSON(t, out)
	if code != 0 || v.Outcome != msg.OutcomeTaken || v.TakenBy != sa {
		t.Fatalf("taken: %d %+v\n%s", code, v, e)
	}
}

func TestAnOfferNobodyTakesTurnsIntoNoTakerNotDoingIt(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "task", sa, "take the 5pm backup")
	id, thread := sentID(t, out)
	now := time.Now()
	r := &deadlineReq{kind: msg.DeadlineTake, sent: now, by: now.Add(2 * time.Second), span: 2 * time.Second}
	var o, e strings.Builder
	err := runDeadline(c, r, outbox.Plan{Msg: msg.Message{From: boss, To: sa}}, "take the 5pm backup", msg.Message{ID: id, Thread: thread, From: boss, To: sa}, false, Streams{In: strings.NewReader(""), Out: &o, Err: &e})
	if exitCode(err) != exitNotFound {
		t.Fatalf("exit %d: %v\n%s\n%s", exitCode(err), err, o.String(), e.String())
	}
	if !strings.Contains(o.String(), "no taker") || !strings.Contains(o.String(), "not doing it") {
		t.Errorf("stdout:\n%s", o.String())
	}
	if d, _ := outbox.DeadlineFor("desk", id); d.Reported.IsZero() {
		t.Error("reported")
	}
}

func TestDeadlineFlagsAreRefusedWhereTheyDoNotMakeSense(t *testing.T) {
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"two deadlines", []string{"--reply-by", "5m", "--until", "5m"}, "one deadline"},
		{"until on a plain message", []string{"--until", "5m"}, "is for a task"},
		{"escalate with no deadline", []string{"--escalate", "person"}, "go with a deadline"},
		{"detach with no deadline", []string{"--detach"}, "go with a deadline"},
		{"escalate while detached", []string{"--reply-by", "5m", "--escalate", "person", "--detach"}, "needs the sending command to wait"},
		{"unknown escalation", []string{"--reply-by", "5m", "--escalate", "phone"}, "machine, person or machine,person"},
		{"too long", []string{"--reply-by", "25h"}, "24h"},
		{"too short", []string{"--reply-by", "1ms"}, "24h"},
		{"need-by in the past", []string{"--need-by", "2020-01-01T00:00:00Z"}, "not in the future"},
		{"need-by nonsense", []string{"--need-by", "teatime"}, "--need-by is a time"},
		{"waits two ways", []string{"--reply-by", "5m", "--wait-reply", "5m"}, "ways to wait"},
		{"ttl shorter than the deadline", []string{"--reply-by", "15m", "--ttl", "5m"}, "ends before the reply-by"},
		{"a file", []string{"--reply-by", "5m", "--file", "x"}, "does not combine"},
		{"a test", []string{"--reply-by", "5m", "--test"}, "does not combine"},
	}
	for _, c := range cases {
		args := append([]string{"send", "--to", sa, "hello", "--dry-run"}, c.args...) // a refusal comes before the dry run; an acceptance would not wait out a deadline
		code, out, e := run(t, "", args...)
		if code != exitUsage || !strings.Contains(e, c.want) || strings.Contains(out, "sent ") {
			t.Errorf("%s: exit %d, want 2 with %q\nout: %s\nerr: %s", c.name, code, c.want, out, e)
		}
	}
	for _, to := range []string{"all", "@repo=x", "*/claude"} {
		code, out, e := run(t, "", "send", "--to", to, "--queue", "hello", "--reply-by", "5m", "--escalate", "machine", "--dry-run")
		if code != exitUsage || !strings.Contains(e, "needs a target on one machine") || strings.Contains(out, "sent ") {
			t.Errorf("--escalate machine to %s: %d\n%s\n%s", to, code, out, e)
		}
	}
	if code, _, e := run(t, "", "send", "--to", "all", "--queue", "hello", "--reply-by", "1s", "--escalate", "person", "--escalate-wait", "0", "--dry-run"); code != 0 {
		t.Errorf("--escalate person has no need of one machine: %d %s", code, e)
	}
	code, _, e := run(t, "", "task", "lap", "x", "--until", "5m", "--ttl", "1h", "--dry-run")
	if code != exitUsage || !strings.Contains(e, "outlives --until") {
		t.Errorf("a ttl longer than the offer: %d %s", code, e)
	}
	code, _, e = run(t, "", "task", "lap", "x", "--until", "30s", "--dry-run")
	if code != exitUsage || !strings.Contains(e, "1 minute to 24h") {
		t.Errorf("an offer of 30s: %d %s", code, e)
	}
	code, _, e = run(t, "", "send", "--to", sa, "x", "--reply-by", "5m", "--escalate", "machine", "--wait-state", "taken", "5m", "--dry-run")
	if code != exitUsage {
		t.Errorf("wait-state: %d %s", code, e)
	}
}

func TestADryRunSaysTheDeadlineAndRemembersNothing(t *testing.T) {
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "send", "--to", sa, "deploying", "--reply-by", "15m", "--escalate", "person,machine", "--dry-run")
	if code != 0 || !strings.Contains(out, "deadline: reply-by") || !strings.Contains(out, "escalate: machine then person") {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	code, out, _ = run(t, "", "send", "--to", sa, "deploying", "--reply-by", "15m", "--dry-run", "--json")
	var j struct {
		Deadline string `json:"deadline"`
	}
	if err := json.Unmarshal([]byte(out), &j); code != 0 || err != nil || !strings.HasPrefix(j.Deadline, "reply-by ") {
		t.Errorf("dry-run json: %d %v %s", code, err, out)
	}
	if got := outbox.DeadlinesFrom("desk"); len(got) != 0 {
		t.Errorf("a dry run remembers nothing: %+v", got)
	}
	if ms, _ := mustClient(t).Inbox(sa, false); len(ms) != 0 {
		t.Errorf("and sends nothing: %d", len(ms))
	}
}

func mustClient(t *testing.T) *client.Client {
	t.Helper()
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A node that cannot report receipts could never say who read a message, so a deadline is refused
// before anything is sent, with what to do.
func TestADeadlineIsRefusedByANodeThatCannotReportReceipts(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	inner := server.NewHandler(s, "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			inner.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		var h msg.Health
		_ = json.Unmarshal(rec.Body.Bytes(), &h)
		var keep []string
		for _, f := range h.Features {
			if f != msg.FeatureReceipts {
				keep = append(keep, f)
			}
		}
		h.Features = keep
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h)
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_AGENT", "")
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "send", "--to", sa, "hello", "--reply-by", "5m")
	if code == 0 || !strings.Contains(e, "cannot report who read a message") || !strings.Contains(e, "aivoicebus mesh") || strings.Contains(out+e, "sent ") {
		t.Errorf("exit %d\n%s\n%s", code, out, e)
	}
	if code, _, e := run(t, "", "send", "--to", sa, "hello"); code != 0 {
		t.Errorf("a send with no deadline is unaffected: %d %s", code, e)
	}
}

func TestParseDeadlineAndNeedBy(t *testing.T) {
	now := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC)
	ok := func(f deadlineFlags, kind string) *deadlineReq {
		t.Helper()
		r, err := parseDeadline(f, kind, now)
		if err != nil {
			t.Fatalf("%+v: %v", f, err)
		}
		return r
	}
	if r := ok(deadlineFlags{}, ""); r != nil {
		t.Errorf("no deadline asked: %+v", r)
	}
	r := ok(deadlineFlags{replyBy: 15 * time.Minute, escalate: "person, MACHINE,person"}, "")
	if r.kind != msg.DeadlineReply || !r.by.Equal(now.Add(15*time.Minute)) || strings.Join(r.escalate, ",") != "machine,person" {
		t.Errorf("reply-by: %+v", r)
	}
	if r := ok(deadlineFlags{until: 20 * time.Minute}, msg.KindTask); r.kind != msg.DeadlineTake || r.span != 20*time.Minute {
		t.Errorf("until: %+v", r)
	}
	for in, want := range map[string]time.Time{
		"15:40Z":                    time.Date(2026, 10, 22, 15, 40, 0, 0, time.UTC),
		"13:00Z":                    time.Date(2026, 10, 23, 13, 0, 0, 0, time.UTC), // already past today: the next one
		"2026-10-22T17:00:00+02:00": time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC),
	} {
		got, err := parseNeedBy(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseNeedBy(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if r := ok(deadlineFlags{needBy: "15:40Z"}, ""); !r.by.Equal(time.Date(2026, 10, 22, 15, 40, 0, 0, time.UTC)) || r.span != 100*time.Minute {
		t.Errorf("need-by: %+v", r)
	}
	local := time.FixedZone("EEST", 3*3600)
	if got, err := parseNeedBy("17:30", now.In(local)); err != nil || !got.Equal(time.Date(2026, 10, 22, 14, 30, 0, 0, time.UTC)) {
		t.Errorf("a clock time with no Z is this machine's: %v %v", got, err)
	}
}

func TestDeadlineTTL(t *testing.T) {
	now := time.Now()
	offer := &deadlineReq{kind: msg.DeadlineTake, span: 20 * time.Minute, by: now.Add(20 * time.Minute)}
	reply := &deadlineReq{kind: msg.DeadlineReply, span: 15 * time.Minute, by: now.Add(15 * time.Minute)}
	if got, err := deadlineTTL("", offer); err != nil || got != "20m0s" {
		t.Errorf("an offer lives as long as it is offered: %q %v", got, err)
	}
	if got, err := deadlineTTL("", reply); err != nil || got != "" {
		t.Errorf("a reply-by message keeps its default ttl: %q %v", got, err)
	}
	if _, err := deadlineTTL("1h", offer); err == nil {
		t.Error("a ttl that outlives the offer")
	}
	if got, err := deadlineTTL("10m", offer); err != nil || got != "10m" {
		t.Errorf("a shorter ttl is fine: %q %v", got, err)
	}
	if _, err := deadlineTTL("5m", reply); err == nil {
		t.Error("a ttl that ends before the reply-by")
	}
	if got, err := deadlineTTL("1d", reply); err != nil || got != "1d" {
		t.Errorf("a longer ttl is fine: %q %v", got, err)
	}
}

// A --private message's words are never repeated in the clear: the question to the person leaves them out.
func TestEscalatingAPrivateMessageDoesNotQuoteIt(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "send", "--to", sa, "sealed words")
	id, thread := sentID(t, out)
	now := time.Now()
	rn := &deadlineRun{c: c, r: &deadlineReq{kind: msg.DeadlineReply, escalate: []string{stepPerson}}, said: "",
		d: outbox.Deadline{ID: id, Thread: thread, From: boss, To: sa, Kind: msg.DeadlineReply, Sent: now, By: now}, s: Streams{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}}
	rn.escalate(msg.Judgement{Outcome: msg.OutcomeNobodyRead})
	ms, _ := c.Inbox(msg.HumanOf(boss), false)
	if len(ms) != 1 || !strings.Contains(ms[0].Body, id) || strings.Contains(ms[0].Body, "It said") || strings.Contains(ms[0].Body, "sealed words") {
		t.Errorf("the question names the message and repeats nothing: %+v", ms)
	}
}

// A deadline whose message the nodes forgot is looked for on each look for a day past its deadline, then given up
// quietly, so `watch` does not ask about it for ever.
func TestWatchGivesUpOnADeadlineTheNodesForgot(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", boss)
	c := mustClient(t)
	now := time.Now()
	fresh, old := msg.NewID(), msg.NewID()
	for id, by := range map[string]time.Time{fresh: now.Add(-time.Minute), old: now.Add(-48 * time.Hour)} {
		if !outbox.RememberDeadline(outbox.Deadline{ID: id, Thread: id, From: boss, To: sa, Kind: msg.DeadlineReply, Sent: by.Add(-10 * time.Minute), By: by}) {
			t.Fatal("not remembered")
		}
	}
	var out strings.Builder
	if n, err := watchOnce(c, time.Hour, 0, &out); n != 0 || err != nil {
		t.Fatalf("nothing can be said about either: %d %v\n%s", n, err, out.String())
	}
	if d, _ := outbox.DeadlineFor("desk", fresh); !d.Reported.IsZero() {
		t.Error("a deadline from a minute ago is looked for again")
	}
	if d, _ := outbox.DeadlineFor("desk", old); d.Reported.IsZero() {
		t.Error("one from two days ago is given up")
	}
}
