package hook

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func promptHook(t *testing.T, stdin string) string {
	t.Helper()
	var out strings.Builder
	Run([]string{"--tool", "claude", "--event", "prompt"}, strings.NewReader(stdin), &out)
	return out.String()
}

// everyPrompt makes the throttle let every prompt through, for the duration of the test.
func everyPrompt(t *testing.T) {
	t.Helper()
	old := promptEvery
	promptEvery = 0
	t.Cleanup(func() { promptEvery = old })
}

// slowAckMesh is a node whose reads are fast and whose acknowledgements take `delay`.
func slowAckMesh(t *testing.T, delay time.Duration) *store.Store {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	h := server.NewHandler(s, "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ack" {
			time.Sleep(delay)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_OFF", "")
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	t.Chdir(t.TempDir())
	return s
}

// An urgent message is acknowledged only once it has been written out: a slow node costs the
// acknowledgement (the message is shown again), never a message nobody saw.
func TestASlowAckNeverSwallowsAnUrgentMessage(t *testing.T) {
	s := slowAckMesh(t, 3*time.Second)
	everyPrompt(t)
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "prod is down", Prio: msg.PrioUrgent})

	start := time.Now()
	got := promptHook(t, "")
	if !strings.Contains(got, "prod is down") {
		t.Fatalf("the message must be printed even though the ack is slow: %q", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the hook must not wait for a slow acknowledgement: %v", d)
	}
	if left := s.Inbox("air/codex", false); len(left) != 1 {
		t.Errorf("an unconfirmed ack leaves the message unread, to be shown again: %d", len(left))
	}
}

func TestSessionStartPrintsFirstAndOnlyThenAcknowledgesABacklog(t *testing.T) {
	s := slowAckMesh(t, 250*time.Millisecond)
	s.SetSendLimit(1 << 30)
	for i := 0; i < 30; i++ {
		s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: fmt.Sprintf("low value %d", i), Prio: msg.PrioFYI})
	}
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "please review", Prio: msg.PrioAsk})
	start := time.Now()
	got := runHook(t, "")
	if !strings.Contains(got, "please review") || !strings.Contains(got, "30 low-priority (fyi)") {
		t.Fatalf("%q", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("30 acks at 250ms each must run in parallel, within the budget: %v", d)
	}
	if left := s.Inbox("air/codex", false); len(left) > 22 { // most were confirmed within the budget; the rest are shown next time
		t.Errorf("acknowledgements are sent in parallel: %d left unread", len(left))
	}
}

func TestThePromptHookIsSilentUnlessSomethingIsUrgent(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "please look when you can", Prio: msg.PrioAsk})
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "lunch at noon", Prio: msg.PrioFYI})
	if got := promptHook(t, ""); got != "" {
		t.Fatalf("ask and fyi messages wait for the next session start: %q", got)
	}
	if left := s.Inbox("air/codex", false); len(left) != 2 {
		t.Errorf("and the check must not acknowledge them: %d left unread", len(left))
	}
}

func TestAnUrgentMessageShowsOnceAndIsAcknowledgedWithAReceipt(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "ask first", Prio: msg.PrioAsk})
	u, _ := s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "CI is red on main, hold merges", Prio: msg.PrioUrgent})

	got := promptHook(t, "")
	for _, want := range []string{"1 URGENT message(s) arrived while you were working", "UNTRUSTED DATA", "CI is red on main, hold merges"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ask first") {
		t.Error("only the urgent message is shown mid-session")
	}
	if again := promptHook(t, ""); again != "" {
		t.Errorf("an acknowledged urgent message is not shown again: %q", again)
	}
	var ack msg.Ack
	for _, r := range s.Since(map[string]uint64{}, 100) {
		if r.Ack != nil && r.Ack.ID == u.ID {
			ack = *r.Ack
		}
	}
	if ack.Via != msg.ViaHook || ack.Chars <= len("CI is red on main, hold merges") {
		t.Errorf("a receipt records what the check put into the context: %+v", ack)
	}
	if left := s.Inbox("air/codex", false); len(left) != 1 || left[0].Body != "ask first" {
		t.Errorf("the ask message is still waiting: %+v", left)
	}
}

func TestAPromptBurstCostsOneLook(t *testing.T) {
	s, _ := mesh(t)
	if got := promptHook(t, ""); got != "" {
		t.Fatal(got)
	}
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "urgent news", Prio: msg.PrioUrgent})
	if got := promptHook(t, ""); got != "" {
		t.Errorf("a second prompt within the window must not look again: %q", got)
	}
	promptEvery = 0
	t.Cleanup(func() { promptEvery = 15 * time.Second })
	if got := promptHook(t, ""); !strings.Contains(got, "urgent news") {
		t.Errorf("once the window has passed the next prompt looks: %q", got)
	}
}

func TestThePromptHookWorksOutTheIdentityFromTheSessionID(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_HOST", "lab")
	s.Send(msg.Message{From: "other/agent", To: "lab/claude.a1b2", Body: "for this session", Prio: msg.PrioUrgent})
	s.Send(msg.Message{From: "other/agent", To: "lab/claude.c3d4", Body: "for another session", Prio: msg.PrioUrgent})
	s.Send(msg.Message{From: "other/agent", To: "@tool=claude", Body: "for every claude", Prio: msg.PrioUrgent})
	got := promptHook(t, `{"session_id":"A1B2-zzz","cwd":""}`)
	if !strings.Contains(got, "for this session") || !strings.Contains(got, "for every claude") || strings.Contains(got, "for another session") {
		t.Errorf("hooks do not inherit the shell's identity, so it is derived from the session:\n%s", got)
	}
}

func TestThePromptHookGivesUpQuicklyWhenTheMeshIsSlowAndHonoursOptOut(t *testing.T) {
	mesh(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(3 * time.Second) }))
	defer slow.Close()
	t.Setenv("AIVOICEBUS_URL", slow.URL)
	everyPrompt(t)
	old := PromptDeadline
	PromptDeadline = 200 * time.Millisecond
	defer func() { PromptDeadline = old }()
	start := time.Now()
	if got := promptHook(t, ""); got != "" {
		t.Errorf("%q", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the check must never hold up a prompt: %v", d)
	}

	s, _ := mesh(t)
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "urgent", Prio: msg.PrioUrgent})
	t.Setenv("AIVOICEBUS_OFF", "1")
	if got := promptHook(t, ""); got != "" {
		t.Errorf("AIVOICEBUS_OFF: %q", got)
	}
}

func TestTheServerAndTheNodeAgreeAnUrgentCheckRegistersThePresence(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, "tok"))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_OFF", "")
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_HOST", "lab")
	t.Chdir(t.TempDir())
	everyPrompt(t)
	promptHook(t, `{"session_id":"zz99-x"}`)
	found := false
	for _, e := range s.Roster() {
		found = found || e.Me == "lab/claude.zz99"
	}
	if !found {
		t.Error("each look refreshes who is around, so presence stays accurate in a long session")
	}
}
