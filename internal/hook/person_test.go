package hook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// personMesh is mesh with a node that says it runs release v, counting the peeks it answers.
func personMesh(t *testing.T, v string) (*store.Store, *atomic.Int32) {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	h := server.NewHandler(s, "tok", server.WithVersion(v))
	var peeks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/inbox" && r.URL.Query().Get("peek") == "1" {
			peeks.Add(1)
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
	return s, &peeks
}

const updateTask = msg.PersonTag + " Update aivoicebus on air: the nodes run 0.11.5. In a terminal there: aivoicebus update (it checks the signature before installing), then aivoicebus version."

// acked reports whether anyone acknowledged id.
func acked(s *store.Store, id string) bool {
	for _, r := range s.Since(map[string]uint64{}, 1000) {
		if r.Ack != nil && r.Ack.ID == id {
			return true
		}
	}
	return false
}

// What waits for the person on this machine is told at every session start, in one line, and stays
// theirs: nothing is acknowledged, and the person is not made a session on the roster.
func TestSessionStartTellsWhatWaitsForThePersonAndLeavesItUnread(t *testing.T) {
	s, peeks := personMesh(t, "0.11.4")
	task, _ := s.Send(msg.Message{From: "deploy/claude", To: "air/human", Kind: msg.KindTask, Body: updateTask})
	hand, _ := s.Send(msg.Message{From: "deploy/claude", To: "air/human", Kind: msg.KindTask, Body: msg.TokenHandTag + " sealed-token-XYZ"})
	s.Send(msg.Message{From: "deploy/claude", To: "air", Body: msg.PersonTag + " for the sessions' own inboxes"})

	got := runHook(t, "")
	for _, want := range []string{
		"[aivoicebus] for the user: task \"Update aivoicebus",
		"…\". already done or not needed: aivoicebus done " + task.ID + " | to do it: aivoicebus claim " + task.ID,
		"[aivoicebus] for the user: token hand from deploy/claude. aivoicebus token install " + hand.ID,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if peeks.Load() != 1 {
		t.Errorf("one peek per session start: %d", peeks.Load())
	}
	if acked(s, task.ID) || acked(s, hand.ID) {
		t.Fatal("the person's tasks are theirs: a session start never acknowledges them")
	}
	if left := s.Peek("air/human"); len(left) != 3 { // the two tasks and the machine-wide note
		t.Fatalf("still unread for the person: %d", len(left))
	}
	for _, e := range s.Roster() {
		if e.Me == "air/human" {
			t.Fatal("a peek never makes the person a session")
		}
	}
	if again := runHook(t, ""); strings.Count(again, "[aivoicebus] for the user:") != 2 {
		t.Errorf("every session start says it while it waits:\n%s", again)
	}

	// once claimed (acknowledged) by the person, it is no longer named
	if err := s.Ack(task.ID, "air/human"); err != nil {
		t.Fatal(err)
	}
	got = runHook(t, "")
	if strings.Count(got, "[aivoicebus] for the user:") != 1 || !strings.Contains(got, "aivoicebus token install "+hand.ID) {
		t.Errorf("the token hand is next:\n%s", got)
	}
	if strings.Contains(got, "XYZ") || !strings.Contains(got, "token hand from deploy/claude") {
		t.Errorf("a token hand is named by its kind, never quoted:\n%s", got)
	}
}

// A node before inbox-peek would list the person as present, so it is not asked at all.
func TestNoPeekAtANodeTooOldToPeek(t *testing.T) {
	s, peeks := personMesh(t, "0.10.2")
	s.Send(msg.Message{From: "deploy/claude", To: "air/human", Kind: msg.KindTask, Body: updateTask})
	if got := runHook(t, ""); strings.Contains(got, "wait for you") || peeks.Load() != 0 {
		t.Fatalf("no peek at a 0.10 node (%d):\n%s", peeks.Load(), got)
	}
	for v, want := range map[string]bool{"0.11.0": true, "0.12.1": true, "dev": true, "0.10.9": false, "": false, "junk": false} {
		if nodePeeks(v) != want {
			t.Errorf("nodePeeks(%q) = %v", v, !want)
		}
	}
}

// Quiet hours hold the person's line back as they hold asks; an urgent one (a stuck agent) still shows.
func TestQuietHoursHoldThePersonsLineButNotAnUrgentOne(t *testing.T) {
	s, _ := personMesh(t, "0.11.4")
	s.Send(msg.Message{From: "deploy/claude", To: "air/human", Kind: msg.KindTask, Body: updateTask})
	q, _ := config.ParseQuiet("22:00-07:00")
	if err := config.SaveQuiet(q); err != nil {
		t.Fatal(err)
	}
	defer setClock(func() time.Time { return time.Date(2026, 10, 14, 23, 30, 0, 0, time.Local) })()
	if got := runHook(t, ""); strings.Contains(got, "for the user:") {
		t.Fatalf("at night the person's task waits:\n%s", got)
	}
	everyPrompt(t)
	if got := promptHook(t, ""); strings.Contains(got, "for the user:") {
		t.Fatalf("and the prompt hook holds it too:\n%s", got)
	}
	s.Send(msg.Message{From: "air/codex.k3", To: "air/human", Prio: msg.PrioUrgent, Body: msg.StuckPrefix + "the build host refuses my key"})
	if got := runHook(t, ""); strings.Count(got, "[aivoicebus] for the user:") != 1 || !strings.Contains(got, `stuck agent "the build host refuses my key"`) {
		t.Fatalf("an urgent one shows at night:\n%s", got)
	}

	setClock(func() time.Time { return time.Date(2026, 10, 15, 9, 0, 0, 0, time.Local) })
	if got := runHook(t, ""); strings.Count(got, "[aivoicebus] for the user:") != 2 || strings.Index(got, "stuck agent") > strings.Index(got, "user: task") {
		t.Errorf("in the morning both, the urgent one first:\n%s", got)
	}
}

// What starts waiting for the person while a session runs is told by the prompt hook once per
// message; what the session start already said is not repeated.
func TestThePromptHookTellsANewPersonTaskOnce(t *testing.T) {
	s, _ := personMesh(t, "0.11.4")
	everyPrompt(t)
	old, _ := s.Send(msg.Message{From: "deploy/claude", To: "air/human", Kind: msg.KindTask, Body: updateTask})
	if got := runHook(t, ""); strings.Count(got, "[aivoicebus] for the user:") != 1 {
		t.Fatalf("session start:\n%s", got)
	}
	if got := promptHook(t, ""); got != "" {
		t.Fatalf("the session start said it already: %q", got)
	}
	hand, _ := s.Send(msg.Message{From: "deploy/claude", To: "air/human", Kind: msg.KindTask, Body: msg.TokenHandTag + " sealed"})
	got := promptHook(t, "")
	if strings.Count(got, "[aivoicebus] for the user:") != 1 || !strings.Contains(got, "aivoicebus token install "+hand.ID) || strings.Contains(got, old.ID) {
		t.Fatalf("the new one, alone:\n%s", got)
	}
	if again := promptHook(t, ""); again != "" {
		t.Errorf("once per message per session: %q", again)
	}
	if acked(s, hand.ID) || acked(s, old.ID) {
		t.Error("the prompt hook never acknowledges the person's tasks")
	}
	// an urgent message and a person task in one look: both said, the person's line first
	s.Send(msg.Message{From: "deploy/claude", To: "air/human", Kind: msg.KindTask, Body: msg.ApprovalTag + " worker on imac wants to write"})
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "prod is down", Prio: msg.PrioUrgent})
	got = promptHook(t, "")
	if i, j := strings.Index(got, "approval request from"), strings.Index(got, "1 URGENT message(s)"); i < 0 || j < i || !strings.Contains(got, "prod is down") {
		t.Errorf("both, the person's line first:\n%s", got)
	}
}
