package store

import (
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestReceiptsSayWhoReadItWhenAndWhoTookTheTask(t *testing.T) {
	s, clock := retractStore(t)
	const boss, a, b = "desk/claude.z9", "lap/claude.a1", "lap/codex.b2"
	task, _ := s.Send(msg.Message{From: boss, To: "lap", Body: "x", Kind: msg.KindTask})
	if r, ok := s.Receipts(task.ID); !ok || len(r.Reads) != 0 || r.Owner != nil || r.Replies != 0 {
		t.Fatalf("fresh: %+v %v", r, ok)
	}
	t0 := *clock
	ackAt := func(by, via string, after time.Duration) {
		*clock = t0.Add(after)
		s.AckWith(msg.Ack{ID: task.ID, By: by, Via: via})
	}
	ackAt(b, msg.ViaHook, time.Second)
	ackAt(a, msg.ViaHookCount, 2*time.Second)
	ackAt(boss, "", 3*time.Second)        // the sender is no reader
	ackAt(a, msg.ViaHook, 5*time.Second)  // read in full later: that counts, at its own time
	ackAt(b, msg.ViaInbox, 6*time.Second) // a second read changes nothing
	*clock = t0.Add(7 * time.Second)
	s.Send(msg.Message{From: b, To: boss, Body: "mine", Kind: msg.KindClaim, Re: task.ID})
	s.Send(msg.Message{From: b, To: boss, Body: "done", Re: task.ID, Thread: task.Thread})
	r, _ := s.Receipts(task.ID)
	if len(r.Reads) != 2 || r.Reads[0].By != b || r.Reads[1].By != a || r.Reads[1].Via != msg.ViaHook || !r.Reads[1].At.Equal(t0.Add(5*time.Second)) {
		t.Errorf("reads %+v", r.Reads)
	}
	if r.Owner == nil || r.Owner.By != b || r.Replies != 2 {
		t.Errorf("owner %+v replies %d", r.Owner, r.Replies)
	}
	// A session that only answered (no ack) got it all the same.
	q, _ := s.Send(msg.Message{From: boss, To: "lap/claude.c3", Body: "ping?"})
	*clock = t0.Add(9 * time.Second)
	s.Send(msg.Message{From: "lap/claude.c3", To: boss, Body: "pong", Re: q.ID, Thread: q.Thread})
	if r, _ := s.Receipts(q.ID); len(r.Reads) != 1 || r.Reads[0].By != "lap/claude.c3" || r.Reads[0].Via != msg.ViaReply || r.Replies != 1 {
		t.Errorf("an answer without an ack: %+v", r)
	}
	if _, ok := s.Receipts("nothere"); ok {
		t.Error("receipts for an unknown id")
	}
}

func TestGenChangesOnEveryRecordAndWakesWaiters(t *testing.T) {
	s, _ := retractStore(t)
	g0, ch := s.Gen()
	select {
	case <-ch:
		t.Fatal("woken before anything arrived")
	default:
	}
	s.Send(msg.Message{From: "a/b.1", To: "c/d.2", Body: "x"})
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("a waiter was not woken by a new message")
	}
	if g1, _ := s.Gen(); g1 <= g0 {
		t.Errorf("gen %d -> %d", g0, g1)
	}
}

func TestASessionInAWaitIsListening(t *testing.T) {
	s, clock := retractStore(t)
	s.InboxFor("lap/claude.a1", false, nil)
	s.NoteWaiting("lap/claude.a1", clock.Add(20*time.Second))
	listening := func() bool {
		for _, e := range s.Roster() {
			if e.Me == "lap/claude.a1" {
				return e.Listening
			}
		}
		return false
	}
	if !listening() {
		t.Error("not listening while waiting")
	}
	s.InboxFor("lap/claude.a1", false, nil) // a plain read meanwhile does not end the wait
	if !listening() {
		t.Error("an inbox read cleared the wait")
	}
	s.NoteWaiting("lap/claude.a1", time.Time{})
	if listening() {
		t.Error("still listening after the wait")
	}
}

// A held read that says it waits for one thread makes the session busy, not listening; one that
// takes anything (standing by, maybe at the same time) makes it listening; one that does not say
// (an older client) makes it listening without a scope, which `who` shows as unverified.
func TestAHeldReadForOneThreadIsBusyNotListening(t *testing.T) {
	s, clock := retractStore(t)
	me := "lap/claude.a1"
	s.InboxFor(me, false, nil)
	entry := func() msg.RosterEntry {
		for _, e := range s.Roster() {
			if e.Me == me {
				return e
			}
		}
		return msg.RosterEntry{}
	}
	endThread := s.HoldRead(me, clock.Add(20*time.Second), &msg.WaitScope{Thread: "t1"})
	if e := entry(); e.Listening || e.Busy() != "waiting for a reply in t1" {
		t.Fatalf("in a thread wait: %+v", e)
	}
	endStandby := s.HoldRead(me, clock.Add(20*time.Second), &msg.WaitScope{})
	if e := entry(); !e.Listening || e.Waits == nil || !e.Waits.Open() || e.Unverified() {
		t.Fatalf("standing by as well: %+v", e)
	}
	endStandby()
	if e := entry(); e.Listening || e.Busy() == "" {
		t.Fatalf("the standby ended, the thread wait goes on: %+v", e)
	}
	endThread()
	if e := entry(); e.Listening || e.Waits != nil {
		t.Fatalf("no wait: %+v", e)
	}
	endOld := s.HoldRead(me, clock.Add(20*time.Second), nil)
	if e := entry(); !e.Listening || !e.Unverified() {
		t.Fatalf("a client that does not say: %+v", e)
	}
	endOld()
	s.HoldRead(me, clock.Add(-time.Second), &msg.WaitScope{}) // a read whose wait is over counts for nothing
	if e := entry(); e.Listening || e.Waits != nil {
		t.Fatalf("an expired wait: %+v", e)
	}
}
