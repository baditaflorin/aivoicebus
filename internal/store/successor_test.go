package store

import (
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

var apiRepo = msg.Labels{"repo": "api", "tool": "claude"}

// succStore is a store with a clock the test moves, and a session 7430 in repo api that was last seen at t0.
func succStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, now := presenceStore(t, t.TempDir())
	s.InboxFor("mac/claude.7430", false, apiRepo)
	return s, now
}

func sendMarked(t *testing.T, s *Store, from, to, body string) msg.Message {
	t.Helper()
	m, err := s.Send(msg.Message{ID: msg.NewSuccessorID(), From: from, To: to, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func has(ms []msg.Message, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

func TestAMarkedMessageToAGoneSessionIsInTheInboxOfItsSuccessor(t *testing.T) {
	s, now := succStore(t)
	t0 := *now
	m := sendMarked(t, s, "mac/claude.asker", "mac/claude.7430", "the answer you asked for")

	*now = t0.Add(msg.GoneAfter - time.Second)
	s.InboxFor("mac/claude.8494", false, apiRepo)
	if got := s.InboxFor("mac/claude.8494", false, apiRepo); has(got, m.ID) {
		t.Fatal("7430 was seen a moment ago: it is not gone, and 8494 is not given its mail")
	}
	*now = t0.Add(msg.GoneAfter)
	s.InboxFor("mac/claude.8494", false, apiRepo)
	got := s.InboxFor("mac/claude.8494", false, apiRepo)
	if !has(got, m.ID) {
		t.Fatalf("7430 has been gone for %s and 8494 works in its repo: want %s in %v", msg.GoneAfter, m.ID, idsOf(got))
	}
	for _, g := range got {
		if g.ID == m.ID && g.To != "mac/claude.7430" {
			t.Errorf("the message is shown as it was written, to 7430: %+v", g)
		}
	}
}

func TestTheMessageStaysForTheSessionItNamesAndTheSuccessorsReadIsTheRecord(t *testing.T) {
	s, now := succStore(t)
	t0 := *now
	m := sendMarked(t, s, "mac/claude.asker", "mac/claude.7430", "answer")
	*now = t0.Add(time.Hour)
	s.InboxFor("mac/claude.8494", false, apiRepo)
	if !has(s.InboxFor("mac/claude.8494", false, apiRepo), m.ID) {
		t.Fatal("not handed over")
	}
	// reading again before acknowledging gives it again: it was shown, nothing says it was read
	if !has(s.InboxFor("mac/claude.8494", false, apiRepo), m.ID) {
		t.Error("an unacknowledged message is shown again, as any unread one is")
	}
	if err := s.AckWith(msg.Ack{ID: m.ID, By: "mac/claude.8494", Via: msg.ViaHook}); err != nil {
		t.Fatal(err)
	}
	if has(s.InboxFor("mac/claude.8494", false, apiRepo), m.ID) {
		t.Error("the successor read it: not unread any more")
	}
	// a newer session of the same repo that arrives later is not handed what 8494 already read
	*now = t0.Add(2 * time.Hour)
	s.InboxFor("mac/claude.8494", false, apiRepo)
	if has(s.InboxFor("mac/claude.c0de", false, apiRepo), m.ID) {
		t.Error("a message the first successor read is not handed on again")
	}
	// ... and the session it names still has its own copy if it ever comes back
	if !has(s.InboxFor("mac/claude.7430", false, apiRepo), m.ID) {
		t.Error("the session it was written to keeps its own copy")
	}
	if got := s.Readers(m.ID); got != 1 {
		t.Errorf("readers %d: only 8494 acknowledged it", got)
	}
}

func TestOnlyTheNewestLiveSessionOfTheRepoIsTheSuccessor(t *testing.T) {
	s, now := succStore(t)
	t0 := *now
	m := sendMarked(t, s, "mac/claude.asker", "mac/claude.7430", "answer")
	*now = t0.Add(time.Hour)
	s.InboxFor("mac/claude.old1", false, apiRepo) // first heard of an hour after 7430
	*now = t0.Add(time.Hour + 10*time.Minute)
	s.InboxFor("mac/claude.new1", false, apiRepo)
	*now = t0.Add(time.Hour + 11*time.Minute)
	if has(s.InboxFor("mac/claude.old1", false, apiRepo), m.ID) {
		t.Error("an older live session of the repo is not given what its newer sibling will be")
	}
	if !has(s.InboxFor("mac/claude.new1", false, apiRepo), m.ID) {
		t.Error("the newest live session of the repo is")
	}
	// if the newest goes quiet, the next one takes over
	*now = t0.Add(time.Hour + 40*time.Minute)
	if !has(s.InboxFor("mac/claude.old1", false, apiRepo), m.ID) {
		t.Error("with the newest gone, the other live session is the successor")
	}
}

func TestWhoIsNotAnInheritor(t *testing.T) {
	cases := []struct {
		name   string
		reader string
		labels msg.Labels
	}{
		{"another repo", "mac/claude.8494", msg.Labels{"repo": "web", "tool": "claude"}},
		{"another tool", "mac/codex.8494", msg.Labels{"repo": "api", "tool": "codex"}},
		{"another machine", "air/claude.8494", apiRepo},
		{"a session that declared no labels", "mac/claude.8494", nil},
	}
	for _, c := range cases {
		s, now := succStore(t)
		t0 := *now
		m := sendMarked(t, s, "mac/claude.asker", "mac/claude.7430", "answer")
		*now = t0.Add(time.Hour)
		s.InboxFor(c.reader, false, c.labels)
		if has(s.InboxFor(c.reader, false, c.labels), m.ID) {
			t.Errorf("%s: handed the mail of a session it does not replace", c.name)
		}
	}
}

func TestOnlyWhatTheSenderMarkedAndOnlyNotesAndQuestionsAreHandedOn(t *testing.T) {
	s, now := succStore(t)
	t0 := *now
	plain, _ := s.Send(msg.Message{From: "mac/claude.asker", To: "mac/claude.7430", Body: "not marked"})
	task, _ := s.Send(msg.Message{ID: msg.NewSuccessorID(), From: "mac/claude.asker", To: "mac/claude.7430", Kind: msg.KindTask, Body: "a task"})
	person, _ := s.Send(msg.Message{ID: msg.NewSuccessorID(), From: "mac/claude.asker", To: "mac/claude.7430", Body: msg.ApprovalTag + " run it"})
	q, _ := s.Send(msg.Message{ID: msg.NewSuccessorID(), From: "mac/claude.asker", To: "mac/claude.7430", Kind: msg.KindQuestion, Body: "which one?"})
	note := sendMarked(t, s, "mac/claude.asker", "mac/claude.7430", "plain marked note")
	name, _ := s.Send(msg.Message{ID: msg.NewSuccessorID(), From: "mac/claude.asker", To: "mac/codex.build1", Body: "to a name"})
	*now = t0.Add(time.Hour)
	s.InboxFor("mac/claude.8494", false, apiRepo)
	got := s.InboxFor("mac/claude.8494", false, apiRepo)
	for _, c := range []struct {
		m    msg.Message
		want bool
		name string
	}{{plain, false, "unmarked"}, {task, false, "a task"}, {person, false, "for the person"}, {q, true, "a question"}, {note, true, "a note"}, {name, false, "to a persistent name"}} {
		if has(got, c.m.ID) != c.want {
			t.Errorf("%s: in the successor's inbox = %v, want %v", c.name, !c.want, c.want)
		}
	}
}

func TestAWithdrawnOrExpiredOrReadMessageIsNotHandedOn(t *testing.T) {
	s, now := succStore(t)
	t0 := *now
	read := sendMarked(t, s, "mac/claude.asker", "mac/claude.7430", "read by the session itself")
	if err := s.AckWith(msg.Ack{ID: read.ID, By: "mac/claude.7430"}); err != nil {
		t.Fatal(err)
	}
	taken := sendMarked(t, s, "mac/claude.asker", "mac/claude.7430", "taken back")
	if _, err := s.Retract(taken.ID, "mac/claude.asker", false); err != nil {
		t.Fatal(err)
	}
	short, err := s.Send(msg.Message{ID: msg.NewSuccessorID(), From: "mac/claude.asker", To: "mac/claude.7430", Body: "short lived", TTL: 120})
	if err != nil {
		t.Fatal(err)
	}
	*now = t0.Add(time.Hour)
	s.InboxFor("mac/claude.8494", false, apiRepo)
	got := s.InboxFor("mac/claude.8494", false, apiRepo)
	for name, id := range map[string]string{"read by its addressee": read.ID, "withdrawn": taken.ID, "expired": short.ID} {
		if has(got, id) {
			t.Errorf("%s: handed on", name)
		}
	}
}

func TestInboxAllListsWhatTheSuccessorWasHandedEvenAfterReadingIt(t *testing.T) {
	s, now := succStore(t)
	t0 := *now
	m := sendMarked(t, s, "mac/claude.asker", "mac/claude.7430", "answer")
	*now = t0.Add(time.Hour)
	s.InboxFor("mac/claude.8494", false, apiRepo)
	if err := s.AckWith(msg.Ack{ID: m.ID, By: "mac/claude.8494"}); err != nil {
		t.Fatal(err)
	}
	if !has(s.InboxFor("mac/claude.8494", true, apiRepo), m.ID) {
		t.Error("inbox --all lists what 8494 inherited")
	}
	if has(s.InboxFor("mac/claude.c0de", true, apiRepo), m.ID) {
		t.Error("a session that is not the successor does not list it")
	}
}

func TestASessionNeverInheritsWhatItSentOrNamed(t *testing.T) {
	s, now := succStore(t)
	t0 := *now
	mine := sendMarked(t, s, "mac/claude.8494", "mac/claude.7430", "from the successor itself")
	*now = t0.Add(time.Hour)
	s.InboxFor("mac/claude.8494", false, apiRepo)
	if has(s.InboxFor("mac/claude.8494", false, apiRepo), mine.ID) {
		t.Error("a session does not inherit its own message")
	}
}

func TestSinceIsKeptAcrossARestartAndFallsBackToTheLastSightingForAnOldFile(t *testing.T) {
	dir := t.TempDir()
	s, now := presenceStore(t, dir)
	t0 := *now
	s.InboxFor("mac/claude.a", false, apiRepo)
	*now = t0.Add(time.Hour)
	s.InboxFor("mac/claude.a", false, apiRepo)
	s.FlushPresence()
	s2, err := Open(dir, "n1", WithClock(func() time.Time { return t0.Add(2 * time.Hour) }))
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.seen["mac/claude.a"].since; !got.Equal(t0) {
		t.Errorf("first heard of at %v, want %v", got, t0)
	}
	// a presence file an older release wrote has no since: the last sighting stands in
	old := s2.seen["mac/claude.a"]
	old.since = time.Time{}
	s2.seen["mac/claude.a"] = old
	s2.presenceDirty = true
	s2.FlushPresence()
	s3, err := Open(dir, "n1", WithClock(func() time.Time { return t0.Add(2 * time.Hour) }))
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.seen["mac/claude.a"].since; !got.Equal(t0.Add(time.Hour)) {
		t.Errorf("from an older file: %v, want the last sighting %v", got, t0.Add(time.Hour))
	}
}

func TestTheMarkAndTheSuccessorsReceiptTravelBetweenNodes(t *testing.T) {
	a, now := succStore(t) // node n1: 7430 last seen at t0
	b, err := Open(t.TempDir(), "n2", WithClock(func() time.Time { return *now }))
	if err != nil {
		t.Fatal(err)
	}
	t0 := *now
	m := sendMarked(t, a, "mac/claude.asker", "mac/claude.7430", "answer")
	if _, err := b.Merge(a.Since(b.VV(), 100)); err != nil {
		t.Fatal(err)
	}
	b.MergePresence(a.Roster()) // what the presence gossip carries
	*now = t0.Add(time.Hour)
	b.InboxFor("mac/claude.8494", false, apiRepo)
	if !has(b.InboxFor("mac/claude.8494", false, apiRepo), m.ID) {
		t.Fatal("the second node hands on a message the first took in: the mark is in the id, so it replicates as it is")
	}
	if err := b.AckWith(msg.Ack{ID: m.ID, By: "mac/claude.8494", Via: msg.ViaHook}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Merge(b.Since(a.VV(), 100)); err != nil {
		t.Fatal(err)
	}
	a.InboxFor("mac/claude.c0de", false, apiRepo)
	if has(a.InboxFor("mac/claude.c0de", false, apiRepo), m.ID) {
		t.Error("the first node saw the successor's receipt, so it does not hand the message to another session")
	}
}

func TestTheSenderIsNeverTheSuccessorAndTheNextOneTakesOver(t *testing.T) {
	s, now := succStore(t)
	t0 := *now
	*now = t0.Add(time.Hour)
	s.InboxFor("mac/claude.sib1", false, apiRepo)
	*now = t0.Add(time.Hour + time.Minute)
	s.InboxFor("mac/claude.asker", false, apiRepo) // the newest live session of the repo is the sender itself
	m := sendMarked(t, s, "mac/claude.asker", "mac/claude.7430", "answer")
	if has(s.InboxFor("mac/claude.asker", false, apiRepo), m.ID) {
		t.Error("the sender is given its own message")
	}
	if !has(s.InboxFor("mac/claude.sib1", false, apiRepo), m.ID) {
		t.Error("with the sender set aside, the next newest session is the successor")
	}
}

func TestInboxReadsStayCheapWhenNothingIsMarked(t *testing.T) {
	s, _ := succStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Send(msg.Message{From: "mac/claude.asker", To: "mac/claude.7430", Body: "plain " + string(rune('a'+i)), Thread: "t" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.succ) != 0 {
		t.Errorf("unmarked mail is not indexed for a successor: %v", s.succ)
	}
}
