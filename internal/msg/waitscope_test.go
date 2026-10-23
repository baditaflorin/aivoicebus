package msg

import (
	"net/url"
	"testing"
)

// What a held read says it waits for comes back as the node reads it; a client that says nothing,
// or something no wait means, leaves the node unable to vouch for it (nil).
func TestWaitScopeTravelsInTheQuery(t *testing.T) {
	for _, w := range []WaitScope{{}, {Thread: "25b8d841b3"}, {From: "lap/claude.a1"}, {Only: WaitOnlyTasks}, {Thread: "t1", From: "lap", Only: WaitOnlyReplies}} {
		q, err := url.ParseQuery(w.Query()[1:])
		if err != nil {
			t.Fatal(err)
		}
		if got := ParseWaitScope(q); got == nil || *got != w {
			t.Errorf("%+v came back as %+v", w, got)
		}
	}
	for _, raw := range []string{"", "wthread=t1", "wscope=1&wonly=sometimes", "wscope=1&wthread=a%0Ab"} {
		q, _ := url.ParseQuery(raw)
		if got := ParseWaitScope(q); got != nil {
			t.Errorf("%q: %+v, want nil", raw, got)
		}
	}
	if q, _ := url.ParseQuery("wscope=1&wonly=all"); ParseWaitScope(q).Open() != true {
		t.Error("only=all is a wait for anything")
	}
}

// A wait accepts what wait --thread/--from/--only would end on, and says what it waits for.
func TestWaitScopeAcceptsAndSaysWhatItWaitsFor(t *testing.T) {
	m := Message{From: "lap/claude.a1", Thread: "t1", Kind: KindTask}
	cases := []struct {
		w    WaitScope
		ok   bool
		busy string
	}{
		{WaitScope{}, true, ""},
		{WaitScope{Thread: "t1"}, true, "waiting for a reply in t1"},
		{WaitScope{Thread: "t2"}, false, "waiting for a reply in t2"},
		{WaitScope{From: "lap"}, true, "waiting for lap"},
		{WaitScope{From: "lap/claude"}, true, "waiting for lap/claude"},
		{WaitScope{From: "desk"}, false, "waiting for desk"},
		{WaitScope{Only: WaitOnlyTasks}, true, "waiting for tasks only"},
		{WaitScope{Only: WaitOnlyReplies}, false, "waiting for a reply"},
	}
	for _, c := range cases {
		if got := c.w.Accepts(m); got != c.ok {
			t.Errorf("%+v accepts: %v", c.w, got)
		}
		if got := c.w.Busy(); got != c.busy {
			t.Errorf("%+v busy: %q", c.w, got)
		}
	}
}

// A roster entry listens (any message), listens unverified (a node that does not say), or is busy
// in a narrower wait; merging keeps the wait that listens.
func TestARosterEntrySaysWhetherItsWaitTakesAMessage(t *testing.T) {
	m := Message{From: "desk/claude.z9", Thread: "new1"}
	open := RosterEntry{Me: "lap/codex.l1", Listening: true, Waits: &WaitScope{}}
	old := RosterEntry{Me: "lap/codex.o1", Listening: true}
	busy := RosterEntry{Me: "lap/claude.w1", Waits: &WaitScope{Thread: "t1"}}
	idle := RosterEntry{Me: "lap/claude.i1"}
	if !open.Accepts(m) || open.Unverified() || open.Busy() != "" {
		t.Errorf("open: %+v", open)
	}
	if !old.Accepts(m) || !old.Unverified() {
		t.Errorf("an older node's listening session is taken at its word, unverified: %+v", old)
	}
	if busy.Accepts(m) || busy.Busy() != "waiting for a reply in t1" || !busy.Accepts(Message{From: "desk/claude.z9", Thread: "t1"}) {
		t.Errorf("busy: %+v", busy)
	}
	if idle.Accepts(m) || idle.Busy() != "" || idle.Unverified() {
		t.Errorf("idle: %+v", idle)
	}
	got := MergeRosters([]RosterEntry{busy}, []RosterEntry{{Me: busy.Me, Listening: true, Waits: &WaitScope{}}}, []RosterEntry{{Me: busy.Me}})
	if len(got) != 1 || !got[0].Listening || got[0].Waits == nil || !got[0].Waits.Open() {
		t.Errorf("merge: %+v", got)
	}
	if got := MergeRosters([]RosterEntry{{Me: busy.Me}}, []RosterEntry{busy}); got[0].Listening || got[0].Busy() == "" {
		t.Errorf("merge keeps a busy wait: %+v", got)
	}
}
