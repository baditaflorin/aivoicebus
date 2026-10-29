package store

import (
	"errors"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func inboxHas(s *Store, me, id string) bool {
	for _, m := range s.Inbox(me, false) {
		if m.ID == id {
			return true
		}
	}
	return false
}

// The observed bug: a message to a machine is shown to every session that starts there. A settle by
// the machine's person hides it from all of them, the ones that start later too; other machines and
// the full list (--all) and the thread are untouched.
func TestASettleHidesAMachineMessageFromEverySessionThereIncludingNewOnes(t *testing.T) {
	s := open(t)
	m, _ := s.Send(msg.Message{From: "desk/claude.s1", To: "air", Kind: msg.KindTask, Body: "collect the agent history"})
	if !inboxHas(s, "air/claude.a1", m.ID) {
		t.Fatal("a session of the machine is shown it before the settle")
	}
	if err := s.Settle(m.ID, "air/human"); err != nil {
		t.Fatal(err)
	}
	for _, me := range []string{"air/claude.a1", "air/codex.b2", "air/claude.brand-new", "AIR/claude.upper"} {
		if inboxHas(s, me, m.ID) {
			t.Errorf("%s is still shown a settled message", me)
		}
	}
	if len(s.Inbox("air/claude.c3", true)) != 1 {
		t.Error("--all still lists it")
	}
	if th := s.Thread(m.ID); len(th) != 1 || th[0].ID != m.ID || th[0].Body != "collect the agent history" {
		t.Errorf("a thread still reads it: %+v", th)
	}
	if got := s.Settled(m.ID); len(got) != 1 {
		t.Errorf("who settled it: %v", got)
	}
}

func TestASettleIsForThePersonsOwnMachineOnly(t *testing.T) {
	s := open(t)
	m, _ := s.Send(msg.Message{From: "desk/claude.s1", To: "all", Body: "the broker moves tonight"})
	if err := s.Settle(m.ID, "air/human"); err != nil {
		t.Fatal(err)
	}
	if inboxHas(s, "air/claude.a1", m.ID) {
		t.Error("air settled it for air")
	}
	if !inboxHas(s, "mac/claude.a1", m.ID) {
		t.Error("another machine's sessions are still shown it")
	}
}

func TestOnlyTheMachinesPersonSettlesAndNeverWhatMustBeAnswered(t *testing.T) {
	s := open(t)
	m, _ := s.Send(msg.Message{From: "desk/claude.s1", To: "air", Body: "hello"})
	if err := s.Settle(m.ID, "air/claude.a1"); !errors.Is(err, ErrNotSettler) {
		t.Errorf("an agent is no person: %v", err)
	}
	if inboxHas(s, "air/codex.b2", m.ID) != true {
		t.Error("a refused settle hid nothing")
	}
	for _, body := range []string{msg.TokenHandTag + " air\nx", msg.ApprovalTag + " run it?", msg.AskPrefix + "which?"} {
		q, _ := s.Send(msg.Message{From: "desk/claude.s1", To: "air/human", Kind: msg.KindTask, Body: body})
		if err := s.Settle(q.ID, "air/human"); !errors.Is(err, ErrMustAnswer) {
			t.Errorf("%q: %v", body, err)
		}
		if !inboxHas(s, "air/human", q.ID) {
			t.Errorf("%q must still wait for the person", body)
		}
	}
	if err := s.Settle("a1b2c3d4e5", "air/human"); !errors.Is(err, ErrUnknown) {
		t.Errorf("a message the node does not hold: %v", err)
	}
	if err := s.Settle("nope", "air/human"); !errors.Is(err, ErrBadSettle) {
		t.Errorf("a bad id: %v", err)
	}
}

// A settle written by the log directly (an old client, a peer) is judged when mail is read: a claim by
// an agent identity, or one on a message that must be answered, hides nothing.
func TestAForgedSettleHidesNothing(t *testing.T) {
	s := open(t)
	m, _ := s.Send(msg.Message{From: "desk/claude.s1", To: "air", Body: "hello"})
	q, _ := s.Send(msg.Message{From: "desk/claude.s1", To: "air/human", Kind: msg.KindTask, Body: msg.ApprovalTag + " run it?"})
	for _, id := range []string{m.ID, q.ID} {
		if err := s.AckWith(msg.Ack{ID: msg.SettlePrefix + id, By: "air/claude.a1"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AckWith(msg.Ack{ID: msg.SettlePrefix + q.ID, By: "air/human"}); err != nil {
		t.Fatal(err)
	}
	if !inboxHas(s, "air/codex.b2", m.ID) || !inboxHas(s, "air/human", q.ID) {
		t.Error("an agent's claim, or a claim on an approval, must not hide a message")
	}
}

func TestASettleSurvivesARestartAndReplication(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	m, _ := s.Send(msg.Message{From: "desk/claude.s1", To: "air", Body: "hello"})
	if err := s.Settle(m.ID, "air/human"); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(m.ID, "air/human"); err != nil {
		t.Errorf("a second settle is a no-op: %v", err)
	}
	s2, _ := Open(dir, "n1")
	if inboxHas(s2, "air/claude.new", m.ID) {
		t.Error("the settle is in the log: a restart keeps it")
	}
	other := open(t) // another node, fed the records in order
	if _, err := other.Merge(s.Since(map[string]uint64{}, 100)); err != nil {
		t.Fatal(err)
	}
	if inboxHas(other, "air/claude.new", m.ID) {
		t.Error("a node that replicated the settle applies it")
	}
}
