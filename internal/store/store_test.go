package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestTheSameWordsWithAnotherPriorityOrKindAreAnotherMessage(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	a, _ := s.Send(msg.Message{From: "a/b", To: "all", Body: "deploy finished", Prio: msg.PrioFYI})
	b, _ := s.Send(msg.Message{From: "a/b", To: "all", Body: "deploy finished", Prio: msg.PrioUrgent})
	if a.ID == b.ID {
		t.Error("an fyi repeated as urgent must be stored, not swallowed as a duplicate")
	}
	q, _ := s.Send(msg.Message{From: "x/y", To: "all", Body: "who owns this?", Kind: msg.KindQuestion})
	n, _ := s.Send(msg.Message{From: "c/d", To: "x/y", Body: "mine", Re: q.ID})
	k, _ := s.Send(msg.Message{From: "c/d", To: "x/y", Body: "mine", Re: q.ID, Kind: msg.KindAnswer})
	if n.ID == k.ID {
		t.Error("the same words as an answer must not be swallowed by the earlier note")
	}
	again, _ := s.Send(msg.Message{From: "a/b", To: "all", Body: "deploy finished", Prio: msg.PrioFYI})
	if again.ID != a.ID {
		t.Error("an identical message is still one message")
	}
}

func TestATornLastLineIsSetAsideSoLaterRecordsStillApply(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	for i := 0; i < 3; i++ {
		s.Send(msg.Message{From: "a/b", To: "c/d", Body: fmt.Sprint("m", i)})
	}
	log := filepath.Join(dir, "log.jsonl")
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"origin":"n1","seq":4,"msg":{"id":"0123456789","from":"a/b","to":"c/`) // power lost mid-write
	f.Close()

	s2, err := Open(dir, "n1")
	if err != nil || s2.VV()["n1"] != 3 {
		t.Fatalf("the cut line is not a record: %v %v", s2.VV(), err)
	}
	if _, err := s2.Send(msg.Message{From: "a/b", To: "c/d", Body: "after the crash"}); err != nil {
		t.Fatal(err)
	}
	s3, _ := Open(dir, "n1")
	if s3.VV()["n1"] != 4 {
		t.Fatalf("the next record must not be glued onto the cut line: %v", s3.VV())
	}
	found := false
	for _, m := range s3.Inbox("c/d", true) {
		found = found || m.Body == "after the crash"
	}
	if !found {
		t.Error("the record written after the crash is lost")
	}
	if _, err := os.Stat(log + ".torn"); err != nil {
		t.Error("what was cut is kept aside")
	}
}

func TestACompleteLastRecordThatOnlyLacksItsNewlineIsKept(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	s.Send(msg.Message{From: "a/b", To: "c/d", Body: "one"})
	log := filepath.Join(dir, "log.jsonl")
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"origin":"n1","seq":2,"msg":{"id":"0123456789","from":"a/b","to":"c/d","thread":"0123456789","body":"two","at":"2026-10-14T00:00:00Z"}}`)
	f.Close()
	s2, _ := Open(dir, "n1")
	if s2.VV()["n1"] != 2 {
		t.Fatalf("%v", s2.VV())
	}
	s2.Send(msg.Message{From: "a/b", To: "c/d", Body: "three"})
	if s3, _ := Open(dir, "n1"); s3.VV()["n1"] != 3 {
		t.Errorf("%v", s3.VV())
	}
}

func TestFlowAndPersistence(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	m1, _ := s.Send(msg.Message{From: "imac/claude", To: "air/codex", Body: "hi"})
	m2, _ := s.Send(msg.Message{From: "air/codex", To: "imac/claude", Re: m1.ID, Body: "yo"})
	if m2.Thread != m1.Thread {
		t.Fatal("reply not threaded")
	}
	if len(s.Inbox("air/codex", false)) != 1 {
		t.Fatal("inbox")
	}
	s.Ack(m1.ID, "air/codex")
	if len(s.Inbox("air/codex", false)) != 0 || len(s.Inbox("air/codex", true)) != 1 {
		t.Fatal("ack")
	}
	s2, _ := Open(dir, "n1") // reload from disk
	if len(s2.Thread(m1.ID)) != 2 || len(s2.Inbox("air/codex", false)) != 0 {
		t.Fatal("persistence")
	}
}

func TestSendIsIdempotentOnClientChosenID(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	m := msg.Message{ID: "00aa11bb22", From: "x/y", To: "all", Body: "once"}
	s.Send(m)
	s.Send(m)
	if got := len(s.Inbox("z/z", false)); got != 1 {
		t.Fatalf("message duplicated: %d", got)
	}
}

func TestMergeRejectsGapsAndReplays(t *testing.T) {
	a, _ := Open(t.TempDir(), "a")
	b, _ := Open(t.TempDir(), "b")
	a.Send(msg.Message{From: "x/y", To: "all", Body: "1"})
	a.Send(msg.Message{From: "x/y", To: "all", Body: "2"})
	recs := a.Since(map[string]uint64{}, 10)
	if n, _ := b.Merge(recs[1:]); n != 0 { // seq 2 without seq 1: a gap
		t.Fatalf("applied a gap: %d", n)
	}
	if n, _ := b.Merge(recs); n != 2 {
		t.Fatalf("applied %d, want 2", n)
	}
	if n, _ := b.Merge(recs); n != 0 {
		t.Fatalf("replay applied %d", n)
	}
}

func TestSelectorMessagesReachOnlySessionsWhoseLabelsMatch(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	s.Send(msg.Message{From: "mac/claude.aaaa", To: "@repo=alpha", Body: "for alpha"})
	s.Send(msg.Message{From: "mac/claude.aaaa", To: "@repo=alpha,tool=codex", Body: "alpha codex only"})
	s.Send(msg.Message{From: "mac/claude.aaaa", To: "all", Body: "everyone"})

	bodies := func(ms []msg.Message) string {
		var b []string
		for _, m := range ms {
			b = append(b, m.Body)
		}
		return strings.Join(b, "|")
	}
	if got := bodies(s.InboxFor("air/claude.bbbb", false, msg.Labels{"repo": "alpha", "tool": "claude"})); got != "for alpha|everyone" {
		t.Errorf("alpha/claude got %q", got)
	}
	if got := bodies(s.InboxFor("air/codex.cccc", false, msg.Labels{"repo": "alpha", "tool": "codex"})); got != "for alpha|alpha codex only|everyone" {
		t.Errorf("alpha/codex got %q", got)
	}
	if got := bodies(s.InboxFor("imac/claude.dddd", false, msg.Labels{"repo": "beta", "tool": "claude"})); got != "everyone" {
		t.Errorf("beta got %q", got)
	}
	// a session that declares nothing (an older client) never receives selector messages
	if got := bodies(s.Inbox("old/agent", false)); got != "everyone" {
		t.Errorf("unlabelled session got %q", got)
	}
	// the sender is never delivered its own message
	if got := bodies(s.InboxFor("mac/claude.aaaa", false, msg.Labels{"repo": "alpha", "tool": "codex"})); got != "" {
		t.Errorf("sender got %q", got)
	}
}

func TestSelectorMessageIsDeliveredToAFreshSessionLaterAndAckedPerSession(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	m, _ := s.Send(msg.Message{From: "mac/claude.aaaa", To: "@repo=alpha", Body: "hello"})
	later := msg.Labels{"repo": "alpha"} // a session that starts working on alpha after the send
	if len(s.InboxFor("air/claude.bbbb", false, later)) != 1 {
		t.Fatal("a session that starts on the repo later should still get it")
	}
	s.Ack(m.ID, "air/claude.bbbb")
	if len(s.InboxFor("air/claude.bbbb", false, later)) != 0 {
		t.Error("acked for that session")
	}
	if len(s.InboxFor("air/claude.eeee", false, later)) != 1 {
		t.Error("another session on the same repo must still receive it: acks are per session")
	}
}

func TestRosterCarriesLabelsAndFallsBackToSenders(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	s.InboxFor("air/claude.bbbb", false, msg.Labels{"repo": "alpha"})
	s.Inbox("old/agent", false)
	s.Send(msg.Message{From: "silent/sender", To: "all", Body: "x"})
	r := s.Roster()
	if len(r) != 3 || r[0].Me != "air/claude.bbbb" || r[1].Me != "old/agent" || r[2].Me != "silent/sender" {
		t.Fatalf("roster order/content: %+v", r)
	}
	if r[0].Labels["repo"] != "alpha" || len(r[1].Labels) != 0 || len(r[2].Labels) != 0 {
		t.Errorf("labels: %+v", r)
	}
	// declaring new labels replaces the old ones
	s.InboxFor("air/claude.bbbb", false, msg.Labels{"repo": "beta"})
	if got := s.Roster()[0].Labels["repo"]; got != "beta" {
		t.Errorf("labels not refreshed: %q", got)
	}
	if len(s.Who()) != 3 {
		t.Errorf("Who() must keep reporting the same identities, got %v", s.Who())
	}
}

func TestTTLBecomesAnExpiryOnTheNodesClockAndExpiredMessagesAreNotDelivered(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })

	sent, _ := s.Send(msg.Message{From: "a/b", To: "all", Body: "short lived", TTL: 600, Prio: "urgent"})
	if sent.Exp == nil || !sent.Exp.Equal(clock.Add(10*time.Minute)) || sent.TTL != 0 || sent.Prio != "urgent" {
		t.Fatalf("stored message: %+v", sent)
	}
	s.Send(msg.Message{From: "a/b", To: "all", Body: "forever"})

	if got := len(s.Inbox("x/y", false)); got != 2 {
		t.Fatalf("before expiry: %d", got)
	}
	clock = clock.Add(11 * time.Minute)
	in := s.Inbox("x/y", false)
	if len(in) != 1 || in[0].Body != "forever" {
		t.Fatalf("after expiry only the permanent message remains: %+v", in)
	}
	if all := s.Inbox("x/y", true); len(all) != 2 {
		t.Errorf("--all still lists the expired message: %d", len(all))
	}
	if th := s.Thread(sent.ID); len(th) != 1 {
		t.Error("a thread keeps its expired messages")
	}
}

func TestExpiryAndPriorityPersistAndReplicate(t *testing.T) {
	dir := t.TempDir()
	a, _ := Open(dir, "a")
	m, _ := a.Send(msg.Message{From: "a/b", To: "all", Body: "x", TTL: 3600, Prio: "fyi"})
	reopened, _ := Open(dir, "a")
	if got := reopened.Inbox("z/z", false); len(got) != 1 || got[0].Exp == nil || !got[0].Exp.Equal(*m.Exp) || got[0].Prio != "fyi" {
		t.Fatalf("after reload: %+v", got)
	}
	b, _ := Open(t.TempDir(), "b")
	if n, _ := b.Merge(a.Since(map[string]uint64{}, 10)); n != 1 {
		t.Fatalf("merge: %d", n)
	}
	if got := b.Inbox("z/z", false); len(got) != 1 || got[0].Exp == nil || got[0].Prio != "fyi" {
		t.Fatalf("after replication: %+v", got)
	}
}

func TestAMessageWithoutOptionsIsStoredExactlyAsBefore(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	m, _ := s.Send(msg.Message{From: "a/b", To: "all", Body: "plain"})
	if m.Exp != nil || m.Prio != "" || m.TTL != 0 {
		t.Errorf("plain message gained fields: %+v", m)
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir(), "log.jsonl"))
	for _, field := range []string{`"prio"`, `"ttl"`, `"exp"`} {
		if strings.Contains(string(b), field) {
			t.Errorf("the log of a plain message must not mention %s: %s", field, b)
		}
	}
}

func TestSeenEntriesExpireAfterAWeekButSendersStayInTheLog(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	clock := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })

	s.Inbox("old/one", false)
	s.Send(msg.Message{From: "old/sender", To: "all", Body: "x"})
	clock = clock.Add(3 * 24 * time.Hour)
	s.Inbox("mid/two", false)
	clock = clock.Add(5 * 24 * time.Hour) // old/one was last heard 8 days ago, mid/two 5
	s.Inbox("new/three", false)

	who := s.Who()
	if _, ok := who["old/one"]; ok {
		t.Error("a session not heard from for over a week must leave the roster")
	}
	for _, want := range []string{"mid/two", "new/three", "old/sender"} {
		if _, ok := who[want]; !ok {
			t.Errorf("%s should still be listed: %v", want, who)
		}
	}
}

func TestSeenIsCappedSoInventedIdentitiesCannotGrowMemory(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	clock := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })
	var first, last string
	for i := 0; i < maxSeen+300; i++ {
		clock = clock.Add(time.Second)
		me := fmt.Sprintf("flood/%05d", i)
		if i == 0 {
			first = me
		}
		last = me
		s.Inbox(me, false)
	}
	if len(s.seen) > maxSeen {
		t.Fatalf("seen grew to %d, the cap is %d", len(s.seen), maxSeen)
	}
	if _, ok := s.seen[last]; !ok {
		t.Error("the newest session must survive eviction")
	}
	if _, ok := s.seen[first]; ok {
		t.Error("the oldest session should have been evicted first")
	}
	if len(s.Roster()) > maxSeen {
		t.Error("the roster must respect the cap too")
	}
}

func TestBulletinsGoStaleButMessagesToOneSessionWait(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })
	s.Send(msg.Message{From: "a/b", To: "all", Body: "news", Prio: msg.PrioFYI})
	s.Send(msg.Message{From: "a/b", To: "all", Body: "please look"})
	s.Send(msg.Message{From: "a/b", To: "x/claude.s1", Body: "for you alone"})
	bodies := func(me string) string {
		var b []string
		for _, m := range s.Inbox(me, false) {
			b = append(b, m.Body)
		}
		return strings.Join(b, "|")
	}

	if got := bodies("x/claude.s1"); got != "news|please look|for you alone" {
		t.Fatalf("fresh: %q", got)
	}
	clock = clock.Add(25 * time.Hour)
	if got := bodies("x/claude.s2"); got != "please look" { // a session that starts a day later
		t.Fatalf("after 25h a new session gets no fyi news: %q", got)
	}
	clock = clock.Add(48 * time.Hour) // 73h
	if got := bodies("x/claude.s3"); got != "" {
		t.Fatalf("after 3 days a new session gets no old bulletin: %q", got)
	}
	if got := bodies("x/claude.s1"); got != "for you alone" {
		t.Fatalf("a message to one session waits for it: %q", got)
	}
	if all := s.Inbox("x/claude.s3", true); len(all) != 2 {
		t.Errorf("--all still lists the stale bulletins: %d", len(all))
	}
}

func TestAnIdenticalMessageSentAgainWithinTenMinutesIsTheSameMessage(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })
	m := msg.Message{From: "a/b", To: "all", Body: "build is red"}
	first, _ := s.Send(m)
	clock = clock.Add(5 * time.Minute)
	again, err := s.Send(m)
	if err != nil || again.ID != first.ID {
		t.Fatalf("a repeat within 10 minutes is the same message: %v %+v vs %+v", err, again, first)
	}
	reply := m
	reply.Re = first.ID // the same words as an answer to something are a different message
	if r, _ := s.Send(reply); r.ID == first.ID {
		t.Error("a reply is not a duplicate")
	}
	other := m
	other.To = "imac"
	if o, _ := s.Send(other); o.ID == first.ID {
		t.Error("another target is not a duplicate")
	}
	clock = clock.Add(6 * time.Minute) // 11 minutes after the first
	if later, _ := s.Send(m); later.ID == first.ID {
		t.Error("after the window the same words are a new message")
	}
	if got := len(s.Inbox("imac/claude.zz", true)); got != 4 {
		t.Errorf("stored %d messages, want 4 (first, reply, other target, late repeat)", got)
	}
}

func TestASenderOverItsHourlyLimitIsToldWhenToTryAgain(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })
	s.SetSendLimit(3)
	send := func(from, body string) error {
		_, err := s.Send(msg.Message{From: from, To: "all", Body: body})
		return err
	}
	for i := 0; i < 3; i++ {
		if err := send("loop/agent", fmt.Sprintf("msg %d", i)); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(time.Minute)
	}
	err := send("loop/agent", "one too many")
	var limited *RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("want a rate limit, got %v", err)
	}
	// the first send was 3 minutes ago, so it falls out of the window in 57 minutes
	if limited.RetryAfter != 57*time.Minute || !strings.Contains(err.Error(), "57m") {
		t.Errorf("retry after %v: %v", limited.RetryAfter, err)
	}
	if err := send("calm/agent", "someone else is unaffected"); err != nil {
		t.Errorf("limits are per sender: %v", err)
	}
	if _, err := s.Send(msg.Message{From: "loop/agent", To: "all", Body: "msg 0"}); err != nil {
		t.Errorf("a duplicate returns the original and costs nothing: %v", err)
	}
	if n := len(s.Inbox("z/z", true)); n != 4 {
		t.Errorf("the refused message was stored: %d messages", n)
	}
	clock = clock.Add(58 * time.Minute) // the oldest send is now over an hour old
	if err := send("loop/agent", "after the wait"); err != nil {
		t.Errorf("the window slides: %v", err)
	}
}

func TestAnAckKeepsItsDeliveryReceiptThroughRestartAndReplication(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	m, _ := s.Send(msg.Message{From: "a/b", To: "all", Body: "hi"})
	if err := s.AckWith(msg.Ack{ID: m.ID, By: "x/y", Via: msg.ViaHook, Chars: 321}); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(m.ID, "p/q"); err != nil { // an ack without a receipt, as older clients send
		t.Fatal(err)
	}
	receipts := func(st *Store) map[string]msg.Ack {
		out := map[string]msg.Ack{}
		for _, r := range st.Since(map[string]uint64{}, 100) {
			if r.Ack != nil {
				out[r.Ack.By] = *r.Ack
			}
		}
		return out
	}
	check := func(where string, got map[string]msg.Ack) {
		t.Helper()
		if a := got["x/y"]; a.Via != msg.ViaHook || a.Chars != 321 || a.At.IsZero() {
			t.Errorf("%s: receipt lost: %+v", where, a)
		}
		if a := got["p/q"]; a.Via != "" || a.Chars != 0 {
			t.Errorf("%s: a plain ack gained a receipt: %+v", where, a)
		}
	}
	check("live", receipts(s))
	again, _ := Open(dir, "n1")
	check("after restart", receipts(again))
	peer, _ := Open(t.TempDir(), "n2")
	if _, err := peer.Merge(s.Since(map[string]uint64{}, 100)); err != nil {
		t.Fatal(err)
	}
	check("on a peer", receipts(peer))

	raw, _ := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if n := strings.Count(string(raw), `"via"`); n != 1 {
		t.Errorf("only the ack that has a receipt writes the fields, found %d \"via\" in the log", n)
	}
}

func TestAnAnsweredQuestionToSeveralSessionsStopsBeingDelivered(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	q, _ := s.Send(msg.Message{From: "a/claude.1", To: "@repo=x", Body: "who owns the flaky test?", Kind: msg.KindQuestion})
	direct, _ := s.Send(msg.Message{From: "a/claude.1", To: "b/claude.2", Body: "private question", Kind: msg.KindQuestion})
	l := msg.Labels{"repo": "x"}

	if got := s.InboxFor("c/codex.3", false, l); len(got) != 1 || got[0].ID != q.ID {
		t.Fatalf("before an answer everyone sees the question: %+v", got)
	}
	// the questioner answering themselves does not close it
	s.Send(msg.Message{From: "a/claude.1", To: "@repo=x", Body: "never mind, found it", Kind: msg.KindAnswer, Re: q.ID})
	if len(s.InboxFor("c/codex.3", false, l)) != 2 { // the question and the self-answer, which is a bulletin too
		t.Fatalf("an answer from the sender closes nothing: %+v", s.InboxFor("c/codex.3", false, l))
	}
	s.Send(msg.Message{From: "d/claude.4", To: "a/claude.1", Body: "it is mine, fixing", Kind: msg.KindAnswer, Re: q.ID})
	for _, m := range s.InboxFor("c/codex.3", false, l) {
		if m.ID == q.ID {
			t.Errorf("an answered question must not be handed to another session: %+v", m)
		}
	}
	if got := s.InboxFor("c/codex.3", true, l); !containsID(got, q.ID) {
		t.Error("--all still lists it")
	}
	if got := s.InboxFor("b/claude.2", false, nil); !containsID(got, direct.ID) {
		t.Error("a question to one session is theirs to read whatever else happens")
	}
	if th := s.Thread(q.ID); len(th) != 3 {
		t.Errorf("the thread keeps the whole conversation: %d", len(th))
	}
}

func TestTheAnswerIndexSurvivesARestartAndArrivesInAnyOrder(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	q, _ := s.Send(msg.Message{From: "a/claude.1", To: "all", Body: "anyone?", Kind: msg.KindQuestion})
	s.Send(msg.Message{From: "d/claude.4", To: "a/claude.1", Body: "me", Kind: msg.KindAnswer, Re: q.ID})
	s2, err := Open(dir, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if containsID(s2.InboxFor("c/codex.3", false, nil), q.ID) {
		t.Error("the closed state is rebuilt from the log")
	}

	// the answer, written on another node, replicated before the question it answers
	asker, _ := Open(t.TempDir(), "n5")
	q2, _ := asker.Send(msg.Message{From: "a/claude.1", To: "all", Body: "second question", Kind: msg.KindQuestion})
	answerer, _ := Open(t.TempDir(), "n6")
	answerer.Send(msg.Message{From: "d/claude.4", To: "a/claude.1", Body: "me", Kind: msg.KindAnswer, Re: q2.ID})
	reader, _ := Open(t.TempDir(), "n7")
	reader.Merge(answerer.Since(map[string]uint64{}, 100))
	reader.Merge(asker.Since(map[string]uint64{}, 100))
	if containsID(reader.InboxFor("c/codex.3", false, nil), q2.ID) {
		t.Error("the order the records arrive in must not matter")
	}
}

func containsID(ms []msg.Message, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

// The placeholder is part of what the dashboard reads; its words are pinned in the shared vectors.
func TestQuarantinePlaceholderIsWhatTheFeedVectorsSay(t *testing.T) {
	b, err := os.ReadFile("../msg/testdata/feed-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Vocabulary struct {
			Quarantine struct{ ID, By string }
		}
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if q := f.Vocabulary.Quarantine; q.ID != QuarantineID || q.By != QuarantineBy {
		t.Errorf("vectors say %+v, the store writes %q by %q", q, QuarantineID, QuarantineBy)
	}
}

// Two sessions replying to each other fast is stopped at pairLimit; the counts survive a restart.
func TestAFastReplyLoopIsStoppedAndRemembered(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "n1")
	if err != nil {
		t.Fatal(err)
	}
	a, b := "lap/claude.a1", "box/codex.b2"
	first, _ := s.Send(msg.Message{From: a, To: b, Body: "ping 0"})
	var err2 error
	n := 0
	for i := 1; i <= 20 && err2 == nil; i++ {
		from, to := a, b
		if i%2 == 1 {
			from, to = b, a
		}
		_, err2 = s.Send(msg.Message{From: from, To: to, Re: first.ID, Body: fmt.Sprintf("pong %s", strings.Repeat("z", i))})
		if err2 == nil {
			n++
		}
	}
	var rl *RateLimitError
	if !errors.As(err2, &rl) || rl.Scope != "pair" || n != pairLimit {
		t.Fatalf("after %d replies: %v", n, err2)
	}
	for i := 0; i < 10; i++ { // fresh notes to the same session are not a loop
		if _, err := s.Send(msg.Message{From: "ci/bot.c1", To: b, Body: fmt.Sprintf("build %d", i)}); err != nil {
			t.Fatalf("note %d: %v", i, err)
		}
	}
	s.FlushPresence()
	r, _ := Open(dir, "n1")
	if _, err := r.Send(msg.Message{From: a, To: b, Re: first.ID, Body: "after restart"}); !errors.As(err, &rl) {
		t.Errorf("a restart forgot the loop: %v", err)
	}
}

// The two sides of a loop may write through different nodes: replication brings each node the
// other side's replies, and the brake holds there too (found live, 0.10 work).
func TestAReplyLoopAcrossTwoNodesIsStopped(t *testing.T) {
	na, _ := Open(t.TempDir(), "a")
	nb, _ := Open(t.TempDir(), "b")
	sync := func() {
		na.Merge(nb.Since(na.VV(), 1000))
		nb.Merge(na.Since(nb.VV(), 1000))
	}
	a, b := "lap/claude.a1", "box/codex.b2"
	first, _ := na.Send(msg.Message{From: a, To: b, Body: "0"})
	sync()
	var err error
	n := 0
	for i := 1; i <= 20; i++ {
		if i%2 == 1 {
			_, err = nb.Send(msg.Message{From: b, To: a, Re: first.ID, Body: strings.Repeat("y", i)}) // b writes through node b
		} else {
			_, err = na.Send(msg.Message{From: a, To: b, Re: first.ID, Body: strings.Repeat("y", i)}) // a through node a
		}
		if err != nil {
			break
		}
		n++
		sync()
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Scope != "pair" || n > pairLimit {
		t.Fatalf("after %d replies across two nodes: %v", n, err)
	}
}

// A thread started with a deadline takes no message after it, except an end.
func TestADeadlineClosesAThread(t *testing.T) {
	s, err := Open(t.TempDir(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	root, _ := s.Send(msg.Message{From: "lap/claude.a1", To: "box/codex.b2", Body: "quick one", Deadline: 600})
	if _, err := s.Send(msg.Message{From: "box/codex.b2", To: "lap/claude.a1", Re: root.ID, Body: "in time"}); err != nil {
		t.Fatalf("before the deadline: %v", err)
	}
	now = now.Add(11 * time.Minute)
	if _, err := s.Send(msg.Message{From: "box/codex.b2", To: "lap/claude.a1", Re: root.ID, Body: "too late"}); !errors.Is(err, ErrTurnLimit) {
		t.Errorf("after the deadline: %v", err)
	}
	if _, err := s.Send(msg.Message{From: "box/codex.b2", To: "lap/claude.a1", Re: root.ID, Kind: msg.KindEnd, Body: "closing"}); err != nil {
		t.Errorf("an end must go: %v", err)
	}
}

// The same thing said a third time in a thread, numbers and wording aside, is refused as a loop.
func TestASlowRepeatingLoopIsRefused(t *testing.T) {
	s, err := Open(t.TempDir(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := s.Send(msg.Message{From: "lap/claude.a1", To: "box/codex.b2", Body: "count with me: 1"})
	var errs []error
	for i := 2; i <= 6; i++ {
		_, err := s.Send(msg.Message{From: "box/codex.b2", To: "lap/claude.a1", Re: root.ID, Body: fmt.Sprintf("%d!", i*10)})
		errs = append(errs, err)
	}
	if errs[0] != nil || errs[1] != nil || !errors.Is(errs[2], ErrTurnLimit) {
		t.Fatalf("%v", errs)
	}
	if _, err := s.Send(msg.Message{From: "box/codex.b2", To: "lap/claude.a1", Re: root.ID, Body: "something new to say"}); err != nil {
		t.Errorf("a new thing is not a loop: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := s.Send(msg.Message{From: "box/codex.b2", To: "lap/claude.a1", Re: root.ID, Kind: msg.KindProgress, Body: fmt.Sprintf("still running, %ds in", i)}); err != nil {
			t.Fatalf("progress heartbeats are not a loop: %v", err)
		}
	}
}
