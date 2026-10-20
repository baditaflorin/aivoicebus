package msg

import (
	"strings"
	"testing"
	"time"
)

// stateThread builds a thread: a task from boss to lap, then each step in order, a second apart.
type stateStep struct{ from, kind, body string }

func stateThread(steps ...stateStep) []Message {
	t0 := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	ms := []Message{{ID: "aaaaaaaa00", Thread: "aaaaaaaa00", From: "boss/claude.a", To: "lap", Kind: KindTask, Body: "do it", At: t0}}
	for i, s := range steps {
		ms = append(ms, Message{ID: "aaaaaaaa" + string(rune('a'+i)) + "1", Thread: "aaaaaaaa00", Re: "aaaaaaaa00", From: s.from, To: "boss/claude.a", Kind: s.kind, Body: s.body, At: t0.Add(time.Duration(i+1) * time.Second)})
	}
	return ms
}

const (
	boss  = "boss/claude.a"
	taker = "lap/codex.x"
	other = "lap/claude.y"
)

func TestStateBodyRoundTrip(t *testing.T) {
	for _, st := range States {
		m := Message{Kind: KindProgress, Body: StateBody(st, "  a note  ")}
		got, note, ok := StateOf(m)
		if !ok || got != st || note != "a note" {
			t.Errorf("%s: %q %q %v", st, got, note, ok)
		}
		if !IsStateBody(m.Body) {
			t.Errorf("%s: IsStateBody", st)
		}
	}
	if StateBody(StateDone, "") != "[state done]" {
		t.Error(StateBody(StateDone, ""))
	}
	for _, m := range []Message{
		{Kind: KindProgress, Body: "[state finished] no such state"},
		{Kind: KindProgress, Body: "step 3/5 [state done]"}, // the tag starts the body or it is no state
		{Kind: KindProgress, Body: " [state done]"},
		{Kind: KindAnswer, Body: "[state done]"}, // only a progress message carries a state
		{Body: "[state ready]"},
		{Kind: KindProgress, Body: "[state ready"},
	} {
		if _, _, ok := StateOf(m); ok {
			t.Errorf("%+v read as a state", m)
		}
	}
}

func TestThreadStateFollowsTheHandshake(t *testing.T) {
	ms := stateThread(
		stateStep{taker, KindClaim, "taking this"},
		stateStep{taker, KindProgress, "[state ready] approved by the person"},
		stateStep{taker, KindProgress, "[state doing] building"},
		stateStep{taker, KindProgress, "[state done] all green"},
	)
	v := ThreadState(ms)
	if v.State != StateDone || v.By != taker || v.Note != "all green" || v.Offerer != boss || v.Taker != taker || !v.Final() || len(v.Ignored) != 0 {
		t.Fatalf("%+v", v)
	}
	if v.Path() != "offered -> taken -> ready -> doing -> done" {
		t.Errorf("path %q", v.Path())
	}
	for _, want := range []string{StateOffered, StateTaken, StateReady, StateDoing, StateDone} {
		if !v.Reached(want) {
			t.Errorf("done has not reached %s", want)
		}
	}
	if v.Reached(StateStuck) || v.CanReach(StateStuck) || v.Reached(StateDeclined) {
		t.Error("done reaches stuck or declined")
	}
	// the order the messages arrive in does not matter
	rev := append([]Message(nil), ms...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	if w := ThreadState(rev); w.Path() != v.Path() || w.State != v.State {
		t.Errorf("arrival order changed the state: %+v", w)
	}
}

func TestThreadStateSkipsForwardAndTakesOnTheFirstMove(t *testing.T) {
	// offered -> ready -> done with no claim: the first move takes it
	v := ThreadState(stateThread(
		stateStep{taker, KindProgress, "[state ready]"},
		stateStep{taker, KindProgress, "[state done]"},
	))
	if v.State != StateDone || v.Taker != taker || v.Path() != "offered -> ready -> done" {
		t.Fatalf("%+v %q", v, v.Path())
	}
	// a plain thread (not a task) has no state until someone says one
	plain := []Message{{ID: "bbbbbbbb00", Thread: "bbbbbbbb00", From: boss, To: taker, Body: "can you?"}}
	if v := ThreadState(plain); v.State != "" || v.Offerer != boss {
		t.Errorf("%+v", v)
	}
	if err := ThreadState(plain).CheckMove(boss, StateOffered); err != nil {
		t.Errorf("the sender cannot offer its own thread: %v", err)
	}
}

func TestInvalidMovesAreRefusedWithAClearMessage(t *testing.T) {
	ready := ThreadState(stateThread(stateStep{taker, KindClaim, ""}, stateStep{taker, KindProgress, "[state ready]"}))
	stuck := ThreadState(stateThread(stateStep{taker, KindClaim, ""}, stateStep{taker, KindProgress, "[state stuck] no disk"}))
	done := ThreadState(stateThread(stateStep{taker, KindProgress, "[state done]"}))
	declined := ThreadState(stateThread(stateStep{taker, KindProgress, "[state declined] not mine"}))
	offered := ThreadState(stateThread())
	for _, c := range []struct {
		v           StateView
		who, to     string
		wantErrPart string // "" means allowed
	}{
		{ready, taker, StateDoing, ""},
		{ready, taker, StateDone, ""},
		{ready, taker, StateStuck, ""},
		{ready, taker, StateDeclined, ""},
		{ready, taker, StateTaken, "only moves forward"},
		{ready, taker, StateReady, "it is ready already"},
		{ready, taker, "finished", "not a state"},
		{ready, other, StateDone, "lap/codex.x took it: only they move it on"},
		{ready, boss, StateDone, "you offered it"},
		{ready, boss, StateOffered, "offered is how a conversation starts"},
		{ready, taker, StateOffered, "only boss/claude.a, who started the conversation, offers it"},
		{stuck, taker, StateDoing, ""},
		{stuck, taker, StateReady, ""},
		{stuck, taker, StateDone, ""},
		{stuck, taker, StateTaken, "it is taken already"},
		{done, taker, StateDoing, "is done already"},
		{done, taker, StateStuck, "is done already"},
		{declined, taker, StateReady, "is declined already"},
		{offered, other, StateDeclined, ""},
		{offered, taker, StateTaken, ""},
		{offered, boss, StateTaken, "you offered it"},
	} {
		err := c.v.CheckMove(c.who, c.to)
		switch {
		case c.wantErrPart == "" && err != nil:
			t.Errorf("%s -> %s by %s refused: %v", c.v.State, c.to, c.who, err)
		case c.wantErrPart != "" && (err == nil || !strings.Contains(err.Error(), c.wantErrPart)):
			t.Errorf("%s -> %s by %s: %v, want it refused with %q", c.v.State, c.to, c.who, err, c.wantErrPart)
		}
	}
}

func TestThreadStateIgnoresWhatTheRulesRefuse(t *testing.T) {
	v := ThreadState(stateThread(
		stateStep{taker, KindClaim, ""},
		stateStep{other, KindProgress, "[state done] I did it instead"},     // not the taker
		stateStep{boss, KindProgress, "[state done] my own task is done"},   // the offerer
		stateStep{taker, KindProgress, "[state doing]"},                     // fine
		stateStep{taker, KindProgress, "[state ready] back to ready"},       // backwards
		stateStep{taker, KindProgress, "[state done]"},                      // fine
		stateStep{taker, KindProgress, "[state stuck] after done, nothing"}, // final already
	))
	if v.State != StateDone || v.Path() != "offered -> taken -> doing -> done" || len(v.Ignored) != 4 {
		t.Fatalf("%+v %q", v, v.Path())
	}
	for _, ig := range v.Ignored {
		if ig.Why == "" || ig.ID == "" {
			t.Errorf("%+v", ig)
		}
	}
	// a withdrawn state message counts for nothing
	ms := stateThread(stateStep{taker, KindProgress, "[state ready]"})
	ms[1].Retracted = &Retraction{}
	if v := ThreadState(ms); v.State != StateOffered || v.Taker != "" {
		t.Errorf("%+v", v)
	}
	// so does a withdrawn task
	ms = stateThread()
	ms[0].Retracted = &Retraction{}
	if v := ThreadState(ms); v.State != "" {
		t.Errorf("%+v", v)
	}
}

func TestTheFirstClaimTakesItLikeTaskOwner(t *testing.T) {
	ms := stateThread(stateStep{taker, KindClaim, ""}, stateStep{other, KindClaim, ""}, stateStep{other, KindProgress, "[state ready]"})
	v := ThreadState(ms)
	if v.Taker != taker || v.State != StateTaken || len(v.Ignored) != 1 {
		t.Fatalf("%+v", v)
	}
	// an answer takes an untaken task too (msg.Takes)
	if v := ThreadState(stateThread(stateStep{taker, KindAnswer, "done already"})); v.State != StateTaken || v.Taker != taker {
		t.Errorf("%+v", v)
	}
}

func TestClosingStates(t *testing.T) {
	for st, want := range map[string]bool{StateDone: true, StateStuck: true, StateDeclined: true, StateOffered: false, StateTaken: false, StateReady: false, StateDoing: false} {
		if ClosingState(st) != want {
			t.Errorf("ClosingState(%s) = %v", st, !want)
		}
	}
	if !FinalState(StateDone) || !FinalState(StateDeclined) || FinalState(StateStuck) {
		t.Error("FinalState")
	}
}

// A state message is an ordinary progress message to every check a node or an older release runs.
func TestAStateMessageIsAnOrdinaryProgressMessage(t *testing.T) {
	m := Message{ID: "cccccccc01", From: taker, To: boss, Thread: "cccccccc00", Re: "cccccccc00", Kind: KindProgress, Body: StateBody(StateReady, "approved")}
	if err := (Record{Origin: "n1", Seq: 1, Msg: &m}).ValidateReplicated(); err != nil {
		t.Error(err)
	}
	if err := m.ValidateOptions(); err != nil {
		t.Error(err)
	}
	if err := m.ValidateShape(); err != nil {
		t.Error(err)
	}
	if r := HoldReason(m); r != "" {
		t.Errorf("held: %s", r)
	}
}
