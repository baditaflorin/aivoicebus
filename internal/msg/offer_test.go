package msg

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOnlyATaskToAnAudienceIsOffered(t *testing.T) {
	for _, c := range []struct {
		m    Message
		want bool
	}{
		{Message{Kind: KindTask, To: "lap", Body: "x"}, true},
		{Message{Kind: KindTask, To: "lap/claude", Body: "x"}, true},
		{Message{Kind: KindTask, To: "lap/*", Body: "x"}, true},
		{Message{Kind: KindTask, To: "*/codex", Body: "x"}, true},
		{Message{Kind: KindTask, To: "@repo=x", Body: "x"}, true},
		{Message{Kind: KindTask, To: "all", Body: "x"}, true},
		{Message{Kind: KindTask, To: "lap/claude.a1", Body: "x"}, false},                // one session
		{Message{Kind: KindTask, To: "lap/build1", Body: "x"}, false},                   // a long-lived name
		{Message{Kind: KindTask, To: "lap/human", Body: "x"}, false},                    // the person
		{Message{Kind: KindTask, To: "lap", Body: PersonTag + " swap the disk"}, false}, // for the person
		{Message{Kind: KindQuestion, To: "lap", Body: "x"}, false},
		{Message{Kind: "", To: "lap", Body: "x"}, false},
	} {
		if got := Offered(c.m); got != c.want {
			t.Errorf("Offered(%s to %s, %q) = %v, want %v", c.m.Kind, c.m.To, c.m.Body, got, c.want)
		}
	}
	exp := time.Now().Add(TestLife)
	if Offered(Message{ID: NewTestID(), Kind: KindTask, To: "lap", Body: "x", Exp: &exp, At: time.Now()}) {
		t.Error("a test task is offered")
	}
}

func TestOfferTierPutsListeningThenInteractiveThenAnyLiveSession(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	task := Message{ID: "0123456789", Kind: KindTask, From: "desk/claude.z9", To: "lap", Thread: "0123456789"}
	for _, c := range []struct {
		name string
		e    RosterEntry
		want int
	}{
		{"listening", RosterEntry{Listening: true, Seen: now.Add(-time.Hour)}, 0},
		{"standing by for tasks", RosterEntry{Waits: &WaitScope{Only: WaitOnlyTasks}, Seen: now}, 0},
		{"waiting for one reply", RosterEntry{Waits: &WaitScope{Thread: "ffffffffff"}, Title: "t", Seen: now}, 1},
		{"titled, active", RosterEntry{Title: "fix it", Seen: now.Add(-OfferActiveWithin)}, 1},
		{"titled, idle", RosterEntry{Title: "fix it", Seen: now.Add(-OfferActiveWithin - time.Second)}, 2},
		{"untitled, live", RosterEntry{Seen: now.Add(-OfferLiveWithin)}, 2},
		{"gone", RosterEntry{Title: "fix it", Seen: now.Add(-OfferLiveWithin - time.Second)}, -1},
	} {
		if got := OfferTier(c.e, task, now); got != c.want {
			t.Errorf("%s: tier %d, want %d", c.name, got, c.want)
		}
	}
}

func TestADeclineIsAProgressMessageThatStartsWithTheTag(t *testing.T) {
	d := Message{Kind: KindProgress, Re: "0123456789", Body: DeclineBody("  not my repo ")}
	if d.Body != "[decline] not my repo" || !IsDecline(d) || DeclineNote(d) != "not my repo" {
		t.Errorf("decline %q: IsDecline %v, note %q", d.Body, IsDecline(d), DeclineNote(d))
	}
	if b := DeclineBody(""); b != DeclineTag || !IsDecline(Message{Kind: KindProgress, Re: "0123456789", Body: b}) {
		t.Errorf("a decline with no reason: %q", b)
	}
	if !IsDecline(Message{Kind: KindProgress, Re: "0123456789", Body: "[decline]\nbusy with the release"}) {
		t.Error("a reason on the next line is not a decline")
	}
	for _, m := range []Message{
		{Kind: KindProgress, Body: DeclineBody("x")},                                // answers nothing
		{Kind: "", Re: "0123456789", Body: DeclineBody("x")},                        // not progress
		{Kind: KindClaim, Re: "0123456789", Body: DeclineBody("x")},                 // a claim takes it
		{Kind: KindProgress, Re: "0123456789", Body: "[declined] x"},                // another word
		{Kind: KindProgress, Re: "0123456789", Body: "[decline]d by the user"},      // the tag, then more of a word
		{Kind: KindProgress, Re: "0123456789", Body: StateBody(StateDeclined, "x")}, // the state, which ends the work
		{Kind: KindProgress, Re: "0123456789", Body: "I will [decline] it"},         // not at the start
	} {
		if IsDecline(m) {
			t.Errorf("%s %q is taken for a decline", m.Kind, m.Body)
		}
	}
}

func TestTheEscalationIdIsTheSameForEveryClientAndNoMarkedShape(t *testing.T) {
	a, b := EscalationID("0123456789"), EscalationID("0123456789")
	if a != b || !ValidID(a) || IsTestID(a) || IsSuccessorID(a) {
		t.Fatalf("EscalationID = %q then %q", a, b)
	}
	if EscalationID("0123456788") == a {
		t.Error("two tasks share an escalation id")
	}
	seen := map[string]bool{}
	for i := 0; i < 5000; i++ {
		id := EscalationID(NewID())
		if IsTestID(id) || IsSuccessorID(id) || !ValidID(id) || seen[id] {
			t.Fatalf("escalation id %q: test %v, successor %v, repeated %v", id, IsTestID(id), IsSuccessorID(id), seen[id])
		}
		seen[id] = true
	}
	e := Message{ID: a, To: "lap/human", Re: "0123456789"}
	if !IsEscalation(e) {
		t.Error("the escalation is not recognised")
	}
	for _, m := range []Message{
		{ID: a, To: "lap", Re: "0123456789"},             // not to the person
		{ID: a, To: "lap/human", Re: "0123456788"},       // answers another task
		{ID: NewID(), To: "lap/human", Re: "0123456789"}, // another id
	} {
		if IsEscalation(m) {
			t.Errorf("%+v is taken for an escalation", m)
		}
	}
}

func TestTheEscalationGoesToThePersonOnTheTargetMachineOrTheSenders(t *testing.T) {
	for to, want := range map[string]string{
		"lap":        "lap/human",
		"lap/claude": "lap/human",
		"lap/*":      "lap/human",
		"*/codex":    "desk/human",
		"all":        "desk/human",
		"@repo=x":    "desk/human",
		"Lap/claude": "lap/human",
	} {
		if got := EscalationTo(Message{From: "desk/claude.z9", To: to}); got != want {
			t.Errorf("EscalationTo(%s) = %s, want %s", to, got, want)
		}
	}
}

// The offer a node tells a reader is a response field; with none set it is not in the JSON at all, so
// every message as stored or replicated keeps the shape the vectors pin.
func TestAnOfferIsLeftOutOfTheJSONWhenThereIsNone(t *testing.T) {
	b, _ := json.Marshal(Message{ID: "0123456789", From: "a/b", To: "c", Body: "x"})
	if strings.Contains(string(b), "offer") {
		t.Errorf("a message with no offer says %s", b)
	}
	b, _ = json.Marshal(Message{ID: "0123456789", Offer: &Offer{Left: 2}})
	if !strings.Contains(string(b), `"offer":{`) {
		t.Errorf("an offer is not in %s", b)
	}
	r, _ := json.Marshal(Receipts{ID: "0123456789"})
	if strings.Contains(string(r), "offers") || strings.Contains(string(r), "escalated") {
		t.Errorf("receipts with no offers say %s", r)
	}
}
