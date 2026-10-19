package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// A conversation state (msg/state.go) is part of a handshake, not a loop: the pair brake neither
// counts it nor refuses it, on the node that took it or on a node it replicated to.
func TestAStateMessageIsOutsideThePairBrake(t *testing.T) {
	na, _ := Open(t.TempDir(), "a")
	nb, _ := Open(t.TempDir(), "b")
	sync := func() {
		na.Merge(nb.Since(na.VV(), 1000))
		nb.Merge(na.Since(nb.VV(), 1000))
	}
	a, b := "lap/claude.a1", "box/codex.b2"
	first, _ := na.Send(msg.Message{From: a, To: b, Kind: msg.KindTask, Body: "start"})
	sync()
	// a conversation that used most of the brake
	n := 0
	for i := 0; n < pairLimit-2; i++ {
		from, to := a, b
		if i%2 == 1 {
			from, to = b, a
		}
		if _, err := na.Send(msg.Message{From: from, To: to, Re: first.ID, Body: fmt.Sprintf("turn %s", strings.Repeat("q", i+1))}); err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
		n++
	}
	sync()
	// many state messages through both nodes: none counts, none is refused
	states := []string{msg.StateTaken, msg.StateReady, msg.StateDoing, msg.StateStuck, msg.StateDoing, msg.StateDone}
	for round := 0; round < 3; round++ {
		for i, st := range states {
			node := na
			if i%2 == 1 {
				node = nb
			}
			m := msg.Message{From: b, To: a, Re: first.ID, Kind: msg.KindProgress, Body: msg.StateBody(st, fmt.Sprintf("round %d step %d", round, i))}
			if _, err := node.Send(m); err != nil {
				t.Fatalf("state %s (round %d) refused: %v", st, round, err)
			}
			sync()
		}
	}
	// the two replies left under the brake still go, and the next one is refused
	var err error
	sent := 0
	for i := 0; i < 4 && err == nil; i++ {
		from, to := a, b
		if i%2 == 1 {
			from, to = b, a
		}
		_, err = na.Send(msg.Message{From: from, To: to, Re: first.ID, Body: fmt.Sprintf("after the states %s", strings.Repeat("w", i+1))})
		if err == nil {
			sent++
		}
		sync()
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Scope != "pair" || sent != 2 {
		t.Errorf("after the states, %d replies went before %v: the states counted toward the brake", sent, err)
	}
	// a plain progress message that is no state still counts, as before
	if _, err := na.Send(msg.Message{From: b, To: a, Re: first.ID, Kind: msg.KindProgress, Body: "step 3/5"}); !errors.As(err, &rl) {
		t.Errorf("a progress message passed the brake: %v", err)
	}
}

// States do not count toward max-turns; done, stuck and declined close a used-up conversation like
// an [end], and taken, ready or doing are refused there. The same for a passed deadline.
func TestStatesAndTheTurnLimitAndDeadline(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { now = now.Add(time.Second); return now })
	a, b := "lap/claude.a1", "box/codex.b2"
	root, err := s.Send(msg.Message{From: a, To: b, Kind: msg.KindTask, Body: "a small job", MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	state := func(st, note string) error {
		_, err := s.Send(msg.Message{From: b, To: a, Re: root.ID, Kind: msg.KindProgress, Body: msg.StateBody(st, note)})
		return err
	}
	for _, st := range []string{msg.StateTaken, msg.StateReady, msg.StateDoing} { // not counted
		if err := state(st, ""); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
	for i := 0; i < 2; i++ { // the root and these two are the three turns
		if _, err := s.Send(msg.Message{From: b, To: a, Re: root.ID, Body: fmt.Sprintf("message %s", strings.Repeat("m", i+1))}); err != nil {
			t.Fatalf("turn %d: %v (the states counted)", i+2, err)
		}
	}
	if _, err := s.Send(msg.Message{From: b, To: a, Re: root.ID, Body: "one too many"}); !errors.Is(err, ErrTurnLimit) {
		t.Fatalf("a fourth message: %v", err)
	}
	if err := state(msg.StateDoing, "again"); !errors.Is(err, ErrTurnLimit) || !strings.Contains(err.Error(), "done, stuck, declined") {
		t.Errorf("doing after the limit: %v", err)
	}
	for _, st := range []string{msg.StateStuck, msg.StateDone} {
		if err := state(st, "closing"); err != nil {
			t.Errorf("%s after the limit: %v", st, err)
		}
	}

	d, _ := s.Send(msg.Message{From: a, To: b, Kind: msg.KindTask, Body: "with a deadline", Deadline: 600})
	now = now.Add(11 * time.Minute)
	if _, err := s.Send(msg.Message{From: b, To: a, Re: d.ID, Kind: msg.KindProgress, Body: msg.StateBody(msg.StateReady, "")}); !errors.Is(err, ErrTurnLimit) {
		t.Errorf("ready after the deadline: %v", err)
	}
	if _, err := s.Send(msg.Message{From: b, To: a, Re: d.ID, Kind: msg.KindProgress, Body: msg.StateBody(msg.StateDeclined, "too late")}); err != nil {
		t.Errorf("declined after the deadline: %v", err)
	}
}
