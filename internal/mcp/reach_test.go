package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// reachMesh: repo graph's one session, lap/claude.f324, last read 3h ago but sent a note just now (so it
// counts as seen); lap/claude.near is live in repo gateway.
func reachMesh(t *testing.T) (*session, *store.Store) {
	t.Helper()
	s, st := start(t)
	st.SetClock(func() time.Time { return time.Now().Add(-3 * time.Hour) })
	st.InboxFor("lap/claude.f324", false, msg.ParseLabels("repo=graph"))
	st.SetClock(time.Now)
	st.InboxFor("lap/claude.near", false, msg.ParseLabels("repo=gateway"))
	return s, st
}

// sentBy makes from send a note now, which moves its last-seen but not its last read.
func sentBy(t *testing.T, from string) {
	t.Helper()
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(msg.Message{ID: msg.NewID(), From: from, To: "lap/claude.near", Body: "step 4 of 9"}); err != nil {
		t.Fatal(err)
	}
}

func heldTo(t *testing.T, st *store.Store, text string) string {
	t.Helper()
	i := strings.Index(text, "sent ")
	if i < 0 || len(text) < i+15 {
		t.Fatalf("not sent: %q", text)
	}
	id := text[i+5 : i+15]
	for _, m := range st.Thread(id) {
		if m.ID == id {
			return m.To
		}
	}
	t.Fatalf("the node does not hold %s", id)
	return ""
}

func TestSendToolRefusesASelectorNoLiveSessionMatches(t *testing.T) {
	s, _ := reachMesh(t)
	text, isErr := s.call("aivoicebus_send", map[string]any{"to": "@repo=graph", "body": "hold the deploy"})
	if !isErr || !strings.Contains(text, "refused: none of the sessions @repo=graph matches is live") || !strings.Contains(text, "lap/claude.f324 (seen 3h ago)") {
		t.Fatalf("%v %q", isErr, text)
	}
	if text, isErr := s.call("aivoicebus_send", map[string]any{"to": "@repo=graph", "body": "hold the deploy", "queue": true}); isErr || !strings.Contains(text, "sent ") {
		t.Errorf("queue: %v %q", isErr, text)
	}
}

func TestSendToolResolvesASessionNamedWithoutItsMachine(t *testing.T) {
	s, st := reachMesh(t)
	text, isErr := s.call("aivoicebus_send", map[string]any{"to": "claude.near", "body": "is the gateway up?"})
	if isErr || !strings.Contains(text, "claude.near -> lap/claude.near: the one live session of that name") || heldTo(t, st, text) != "lap/claude.near" {
		t.Fatalf("%v %q", isErr, text)
	}
	if text, isErr := s.call("aivoicebus_send", map[string]any{"to": "claude.zz00", "body": "x", "queue": true}); !isErr || !strings.Contains(text, "no session of that name has been seen") {
		t.Errorf("nobody: %v %q", isErr, text)
	}
}

func TestSendToolWarnsWhenTheSessionNamedHasReadNothingForHours(t *testing.T) {
	s, st := reachMesh(t)
	sentBy(t, "lap/claude.f324")
	text, isErr := s.call("aivoicebus_send", map[string]any{"to": "lap/claude.f324", "body": "pause before the deploy"})
	if isErr || heldTo(t, st, text) != "lap/claude.f324" || !strings.Contains(text, "warning: lap/claude.f324 has read nothing for 3h") {
		t.Errorf("%v %q", isErr, text)
	}
}

func TestAskToolResolvesABareNameAndSaysWhenItsSessionReadsNothing(t *testing.T) {
	s, _ := reachMesh(t)
	sentBy(t, "lap/claude.f324")
	s.wait = 15 * time.Second
	text, isErr := s.call("aivoicebus_ask", map[string]any{"target": "claude.f324", "body": "are you deploying?", "claim_seconds": 5})
	for _, want := range []string{"claude.f324 -> lap/claude.f324: the one live session of that name", "asked lap/claude.f324", "warning: lap/claude.f324 has read nothing for 3h"} {
		if isErr || !strings.Contains(text, want) {
			t.Errorf("missing %q: %v %q", want, isErr, text)
		}
	}
	if text, isErr := s.call("aivoicebus_ask", map[string]any{"target": "claude.zz00", "body": "anyone?"}); !isErr || !strings.Contains(text, "no session of that name has been seen") {
		t.Errorf("nobody: %v %q", isErr, text)
	}
}

func TestTaskToolResolvesASessionNamedWithoutItsMachine(t *testing.T) {
	s, st := reachMesh(t)
	text, isErr := s.call("aivoicebus_task", map[string]any{"to": "claude.near", "body": "check the gateway"})
	f := strings.Fields(text)
	if isErr || len(f) < 2 || f[0] != "task" || !strings.Contains(text, "claude.near -> lap/claude.near: the one live session of that name") {
		t.Fatalf("%v %q", isErr, text)
	}
	if to := heldTo(t, st, "sent "+f[1]); to != "lap/claude.near" {
		t.Errorf("held to %q", to)
	}
}
