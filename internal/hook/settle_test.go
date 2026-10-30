package hook

import (
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// The observed bug, end to end: a message to the machine, and a task for its person, are told to every
// session that starts. After the person settles them a NEW session on that machine is told nothing.
func TestAfterTheSettleANewSessionOnTheMachineIsToldNothing(t *testing.T) {
	s, _ := personMesh(t, "0.14.0")
	machine, _ := s.Send(msg.Message{From: "deploy/claude", To: "air", Kind: msg.KindTask, Body: "collect the agent history"})
	person, _ := s.Send(msg.Message{From: "deploy/claude", To: "air/human", Kind: msg.KindTask, Body: updateTask})

	t.Setenv("AIVOICEBUS_ME", "air/codex.first")
	got := runHook(t, "")
	if !strings.Contains(got, machine.ID) || !strings.Contains(got, person.ID) {
		t.Fatalf("before the settle a session is told both:\n%s", got)
	}
	for _, id := range []string{machine.ID, person.ID} {
		if err := s.Settle(id, "air/human"); err != nil {
			t.Fatal(err)
		}
	}
	for _, me := range []string{"air/codex.first", "air/claude.second", "air/claude.third"} {
		t.Setenv("AIVOICEBUS_ME", me)
		if got := runHook(t, ""); strings.TrimSpace(got) != "" {
			t.Errorf("%s: a settled message costs nothing, got:\n%s", me, got)
		}
	}
}
