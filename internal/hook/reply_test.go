package hook

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// asked sends, through the outbox as the CLI does (so this session remembers having sent it),
// the question this session (air/codex) is waiting on an answer to.
func asked(t *testing.T, s *store.Store) msg.Message {
	t.Helper()
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	c.Warn = io.Discard
	p, err := outbox.Prepare(c, outbox.Request{To: "other/agent", Body: "which port is free?", Kind: msg.KindQuestion}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	q, err := p.Send(c)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestAReplyToThisSessionShowsOnTheNextPromptOnceThenIsSilent(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	q := asked(t, s)
	r, _ := s.Send(msg.Message{From: "other/agent", To: "air/codex", Re: q.ID, Body: "18321 is free", Kind: msg.KindAnswer})

	got := promptHook(t, "")
	for _, want := range []string{"1 reply message(s) to this session arrived while you were working", "one-line summary", "UNTRUSTED DATA", "18321 is free", "[answer]"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "URGENT") {
		t.Errorf("a reply is shown at its own priority, not as urgent:\n%s", got)
	}
	if again := promptHook(t, ""); again != "" {
		t.Errorf("an acknowledged reply is not shown again: %q", again)
	}
	var ack msg.Ack
	for _, rec := range s.Since(map[string]uint64{}, 100) {
		if rec.Ack != nil && rec.Ack.ID == r.ID {
			ack = *rec.Ack
		}
	}
	if ack.Via != msg.ViaHook || ack.Chars <= len("18321 is free") {
		t.Errorf("a receipt records what the check put into the context: %+v", ack)
	}
}

// Only a message to exactly this session that answers something counts: broadcasts and
// selectors wait for the next session start even when they are replies, and so does a plain
// ask, a fyi reply and a reply to another session.
func TestThePromptHookShowsNoBroadcastSelectorOrPlainMessage(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	q := asked(t, s)
	s.Send(msg.Message{From: "other/agent", To: "all", Re: q.ID, Body: "reply to all"})
	s.Send(msg.Message{From: "other/agent", To: "air/*", Re: q.ID, Body: "reply to the host"})
	s.Send(msg.Message{From: "other/agent", To: "*/codex", Re: q.ID, Body: "reply to every codex"})
	s.Send(msg.Message{From: "other/agent", To: "@tool=codex", Re: q.ID, Body: "reply to a selector"})
	s.Send(msg.Message{From: "other/agent", To: "lab/codex.zz11", Re: q.ID, Body: "reply to another session"})
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "a plain ask"})
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Re: q.ID, Body: "a fyi reply", Prio: msg.PrioFYI})
	if got := promptHook(t, `{"cwd":""}`); got != "" {
		t.Fatalf("none of these is shown mid-session:\n%s", got)
	}
	// all but the one to another session; the selector one is delivered only to a reader that
	// declares labels, which this unlabelled read does not, but the hook's own read did
	if left := s.InboxFor("air/codex", false, msg.Labels{"tool": "codex"}); len(left) != 6 {
		t.Errorf("and none of them is acknowledged: %d left unread", len(left))
	}
}

// What the sender of a reply says is not proof it is one: only a reply to a message this session
// sent is shown mid-session. The rest stays unread for the normal inbox.
func TestOnlyARepliesToWhatThisSessionSentIsShownMidSession(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	asked(t, s)
	// someone else on this machine sent this one: it is not this session's question
	other, _ := s.Send(msg.Message{From: "air/claude.zz11", To: "other/agent", Body: "not mine?", Kind: msg.KindQuestion})
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Re: "00000000aa", Body: "a reply to nothing that exists"})
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Re: other.ID, Body: "a reply to a sibling session"})
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Re: "00000000bb", Body: "an answer to nothing", Kind: msg.KindAnswer})
	if got := promptHook(t, ""); got != "" {
		t.Fatalf("nothing here answers this session:\n%s", got)
	}
	if left := s.Inbox("air/codex", false); len(left) != 3 {
		t.Errorf("they all stay unread for the session start: %d", len(left))
	}
}

// A node's notice is never a reply, whatever its Re names, even one that names this session's own message.
func TestANodeNoticeIsNeverAReplyCandidate(t *testing.T) {
	n := msg.Message{From: noticeFrom, To: "air/codex", Re: "00aa11bb22"}
	if isReplyCandidate(n, "air/codex") {
		t.Error("a notice waits for the session start unless it is urgent")
	}
	n.From = "other/agent"
	if !isReplyCandidate(n, "air/codex") {
		t.Error("the same message from anyone else is a candidate")
	}
}

// A plain retraction notice (ask priority, which a node makes up when a session that read a message
// reads its inbox) waits for the session start; a leak notice is urgent and arrives like any urgent
// one. Builds without notices (before 0.8) have nothing to test.
func TestAPlainNoticeWaitsAndALeakNoticeComesThroughTheUrgentPath(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	var skew atomic.Int64 // the node's clock runs ahead of the test's by this many nanoseconds
	s.SetClock(func() time.Time { return time.Now().Add(time.Duration(skew.Load())) })
	retracted := func(body string, leak bool) {
		m, _ := s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: body})
		s.AckWith(msg.Ack{ID: m.ID, By: "air/codex", Via: msg.ViaInbox})
		if _, err := s.Retract(m.ID, "other/agent", leak); err != nil {
			t.Fatal(err)
		}
	}
	notices := func() (n []msg.Message) {
		for _, m := range s.InboxFor("air/codex", false, nil) {
			if m.From == noticeFrom {
				n = append(n, m)
			}
		}
		return n
	}
	retracted("a plain mistake", false)
	skew.Store(int64(time.Hour)) // however long a notice is held back
	if len(notices()) != 1 {
		t.Skip("this build makes no retraction notices")
	}
	if got := promptHook(t, ""); got != "" {
		t.Fatalf("a plain notice waits: %q", got)
	}
	if len(notices()) != 1 {
		t.Fatal("and stays unread")
	}
	retracted("a leaked key", true)
	skew.Store(int64(2 * time.Hour))
	got := promptHook(t, "")
	if !strings.Contains(got, "1 URGENT message(s)") || !strings.Contains(got, noticeFrom) || strings.Contains(got, "plain mistake") {
		t.Errorf("only the leak notice, and as urgent:\n%s", got)
	}
	if n := notices(); len(n) != 1 || strings.Contains(n[0].Body, "leaked key") {
		t.Errorf("the plain one is still waiting and the leak one was acknowledged: %+v", n)
	}
}

func TestUrgentComesFirstAndRepliesShareTheCap(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	q := asked(t, s)
	for _, b := range []string{"reply one", "reply two", "reply three"} {
		s.Send(msg.Message{From: "other/agent", To: "air/codex", Re: q.ID, Body: b})
	}
	s.Send(msg.Message{From: "other/agent", To: "all", Body: "the build is red", Prio: msg.PrioUrgent})
	got := promptHook(t, "")
	if !strings.Contains(got, "1 URGENT message(s) and 3 reply message(s) to this session") || !strings.Contains(got, "Tell the user now") {
		t.Errorf("header:\n%s", got)
	}
	if i, j := strings.Index(got, "the build is red"), strings.Index(got, "reply one"); i < 0 || j < 0 || i > j {
		t.Errorf("urgent first:\n%s", got)
	}
	if strings.Contains(got, "reply three") || !strings.Contains(got, "+1 more unread") {
		t.Errorf("at most %d are shown and the rest counted:\n%s", maxPromptShown, got)
	}
	if left := s.Inbox("air/codex", false); len(left) != 1 || left[0].Body != "reply three" {
		t.Errorf("only what was shown is acknowledged: %+v", left)
	}
}

func TestQuietHoursHoldRepliesBackMidSessionButNeverUrgent(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	q := asked(t, s)
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Re: q.ID, Body: "a late answer"})
	qh, _ := config.ParseQuiet("22:00-07:00")
	if err := config.SaveQuiet(qh); err != nil {
		t.Fatal(err)
	}
	defer setClock(func() time.Time { return time.Date(2026, 10, 14, 23, 30, 0, 0, time.Local) })()
	if got := promptHook(t, ""); got != "" {
		t.Errorf("a reply waits out quiet hours like any ask: %q", got)
	}
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "prod is down", Prio: msg.PrioUrgent})
	if got := promptHook(t, ""); !strings.Contains(got, "prod is down") || strings.Contains(got, "a late answer") {
		t.Errorf("urgent still arrives:\n%s", got)
	}
}

// ackSpy is an output that records whether a message was still unread when the hook wrote.
type ackSpy struct {
	s       *store.Store
	unread  int
	written bool
}

func (a *ackSpy) Write(p []byte) (int, error) {
	if !a.written {
		a.written, a.unread = true, len(a.s.Inbox("air/codex", false))
	}
	return len(p), nil
}

func TestAReplyIsAcknowledgedOnlyAfterItWasWrittenOut(t *testing.T) {
	s := slowAckMesh(t, 0)
	everyPrompt(t)
	q := asked(t, s)
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Re: q.ID, Body: "done"})
	spy := &ackSpy{s: s}
	Run([]string{"--tool", "codex", "--event", "prompt"}, nil, spy)
	if !spy.written || spy.unread != 1 {
		t.Fatalf("the reply must still be unread when the hook writes it: written=%v unread=%d", spy.written, spy.unread)
	}
	if left := s.Inbox("air/codex", false); len(left) != 0 {
		t.Errorf("and acknowledged afterwards: %d left", len(left))
	}
}

func TestASlowAckOrASlowMeshNeverCostsAReply(t *testing.T) {
	s := slowAckMesh(t, 3*time.Second)
	everyPrompt(t)
	q := asked(t, s)
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Re: q.ID, Body: "slow but here"})
	start := time.Now()
	if got := promptHook(t, ""); !strings.Contains(got, "slow but here") {
		t.Fatalf("printed even though the ack is slow: %q", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the prompt never waits for a slow acknowledgement: %v", d)
	}
	if left := s.Inbox("air/codex", false); len(left) != 1 {
		t.Errorf("an unconfirmed ack leaves the reply unread, to be shown again: %d", len(left))
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(3 * time.Second) }))
	defer slow.Close()
	t.Setenv("AIVOICEBUS_URL", slow.URL)
	old := PromptDeadline
	PromptDeadline = 200 * time.Millisecond
	defer func() { PromptDeadline = old }()
	start = time.Now()
	if got := promptHook(t, ""); got != "" {
		t.Errorf("a slow mesh is silent: %q", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("within the deadline: %v", d)
	}
}
