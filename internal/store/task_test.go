package store

import (
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// A task to a machine is offered to one session there at a time (offer_test.go) until one takes it,
// then shown only to that one. Any session may claim it, offered or not.
func TestATaskGoesToOneSessionOnTheMachineAtATimeUntilOneTakesIt(t *testing.T) {
	s, clock := retractStore(t)
	const a, b, boss = "lap/claude.a1", "lap/codex.b2", "desk/claude.z9"
	task, _ := s.Send(msg.Message{From: boss, To: "lap", Body: "send me the screenshot", Kind: msg.KindTask})
	if !containsID(s.InboxFor(a, false, nil), task.ID) {
		t.Fatalf("%s, the first to read, is not offered the task", a)
	}
	if containsID(s.InboxFor(b, false, nil), task.ID) {
		t.Fatalf("%s sees the task while %s holds its offer", b, a)
	}
	// The sender's own claim takes nothing.
	s.Send(msg.Message{From: boss, To: "lap", Body: "still mine", Kind: msg.KindClaim, Re: task.ID})
	if !containsID(s.InboxFor(a, false, nil), task.ID) {
		t.Fatal("the sender's claim hid the task")
	}
	*clock = clock.Add(1)
	cb, _ := s.Send(msg.Message{From: b, To: boss, Body: "taking it", Kind: msg.KindClaim, Re: task.ID})
	*clock = clock.Add(1)
	s.Send(msg.Message{From: a, To: boss, Body: "me too", Kind: msg.KindClaim, Re: task.ID}) // later: loses
	if containsID(s.InboxFor(a, false, nil), task.ID) {
		t.Error("the task is still shown to a session that lost the claim")
	}
	if !containsID(s.InboxFor(b, false, nil), task.ID) {
		t.Error("the owner no longer sees its task")
	}
	if o, ok := s.TaskOwner(task.ID); !ok || o.From != b {
		t.Errorf("owner %+v, want %s", o, b)
	}
	// The owner withdraws its claim: the next claim stands.
	if _, err := s.Retract(cb.ID, b, false); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.TaskOwner(task.ID); o.From != a {
		t.Errorf("after the withdrawal the owner is %s, want %s", o.From, a)
	}
}

// A claim is not an answer: it does not close a question, and a question's answers do not make
// anyone own it.
func TestAClaimDoesNotCloseAQuestion(t *testing.T) {
	s, _ := retractStore(t)
	q, _ := s.Send(msg.Message{From: "desk/claude.z9", To: "all", Body: "who knows?", Kind: msg.KindQuestion})
	s.Send(msg.Message{From: "lap/claude.a1", To: "desk/claude.z9", Body: "looking", Kind: msg.KindClaim, Re: q.ID})
	if !containsID(s.InboxFor("lap/codex.b2", false, nil), q.ID) {
		t.Error("a claim closed a question")
	}
	if _, ok := s.TaskOwner(q.ID); ok {
		t.Error("a question has an owner")
	}
}

// A task to one session has nobody else to hide from.
func TestATaskToOneSessionStaysWithIt(t *testing.T) {
	s, _ := retractStore(t)
	task, _ := s.Send(msg.Message{From: "desk/claude.z9", To: "lap/claude.a1", Body: "x", Kind: msg.KindTask})
	s.Send(msg.Message{From: "other/codex.q", To: "desk/claude.z9", Body: "mine", Kind: msg.KindClaim, Re: task.ID})
	if !containsID(s.InboxFor("lap/claude.a1", false, nil), task.ID) {
		t.Error("a stranger's claim took a task sent to one session")
	}
}

// A peek shows what waits for an identity without making it a session: an agent's standby looks at
// what waits for its machine's person, and no person shows up on the roster for it.
func TestAPeekShowsWhatWaitsWithoutMakingItASession(t *testing.T) {
	s, _ := retractStore(t)
	task, _ := s.Send(msg.Message{From: "desk/claude.z9", To: "lap/human", Body: msg.PersonTag + " swap the disk", Kind: msg.KindTask})
	if !containsID(s.Peek("lap/human"), task.ID) {
		t.Fatal("the peek does not show the person's task")
	}
	for _, e := range s.RosterSince(time.Time{}) {
		if e.Me == "lap/human" {
			t.Errorf("a peek put lap/human on the roster: %+v", e)
		}
	}
	if !containsID(s.InboxFor("lap/human", false, nil), task.ID) {
		t.Fatal("the person's own read does not show it")
	}
	s.Ack(task.ID, "lap/human")
	if containsID(s.Peek("lap/human"), task.ID) {
		t.Error("a peek shows what the person read")
	}
}
