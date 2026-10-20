package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// A conversation started with a character budget takes no message that would pass it; an end, a
// claim and a state that closes it still go, and the budget survives a restart.
func TestACharacterBudgetBoundsAConversation(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	a, b := "lap/claude.a1", "box/codex.b2"
	root, err := s.Send(msg.Message{From: a, To: b, Kind: msg.KindTask, Body: strings.Repeat("r", 40), BudgetChars: 100})
	if err != nil || root.BudgetChars != 100 {
		t.Fatalf("%v %+v", err, root)
	}
	if _, err := s.Send(msg.Message{From: b, To: a, Re: root.ID, Kind: msg.KindClaim, Body: "mine"}); err != nil {
		t.Fatal(err)
	}
	r1, err := s.Send(msg.Message{From: b, To: a, Re: root.ID, Body: strings.Repeat("é", 50), BudgetChars: 1000})
	if err != nil || r1.BudgetChars != 0 {
		t.Fatalf("a reply within the budget: %v (a reply sets no budget of its own: %d)", err, r1.BudgetChars)
	}
	_, err = s.Send(msg.Message{From: a, To: b, Re: root.ID, Body: "seven more"}) // 40+4+50 = 94 used, 6 left
	if !errors.Is(err, ErrTurnLimit) || !strings.Contains(err.Error(), "character budget") || !strings.Contains(err.Error(), "has 6 left") {
		t.Fatalf("past the budget: %v", err)
	}
	if _, err := s.Send(msg.Message{From: a, To: b, Re: root.ID, Body: "ok 6"}); err != nil {
		t.Errorf("what fits still goes: %v", err)
	}
	if st := s.Stats(); st.RefusedBy["budget"] != 1 {
		t.Errorf("refused by: %v", st.RefusedBy)
	}
	r, _ := Open(dir, "n1")
	if _, err := r.Send(msg.Message{From: b, To: a, Re: root.ID, Body: "after a restart"}); !errors.Is(err, ErrTurnLimit) {
		t.Errorf("a restart forgot the budget: %v", err)
	}
	if _, err := r.Send(msg.Message{From: b, To: a, Re: root.ID, Kind: msg.KindProgress, Body: msg.StateBody(msg.StateDone, "all of it is done, and this note is long")}); err != nil {
		t.Errorf("a closing state goes: %v", err)
	}
	if _, err := r.Send(msg.Message{From: a, To: b, Re: root.ID, Kind: msg.KindEnd, Body: "closing this one, it used its budget"}); err != nil {
		t.Errorf("an end goes: %v", err)
	}
}
