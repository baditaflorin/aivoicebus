package store

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// laneStore is a store with a clock the test moves.
func laneStore(t *testing.T) (*Store, *time.Time, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, "n1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	return s, &now, dir
}

var optedIn = msg.Labels{msg.TestLabel: msg.TestOn}

// sendTest sends a test message (a test id, a ttl of at most ten minutes).
func sendTest(t *testing.T, s *Store, from, to, body string, ttl int64) msg.Message {
	t.Helper()
	m, err := s.Send(msg.Message{ID: msg.NewTestID(), From: from, To: to, Body: body, TTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsTest() {
		t.Fatalf("what the node stamped is not a test message: %+v", m)
	}
	return m
}

func idsOf(ms []msg.Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestATestMessageReachesOnlyAnOptedInReaderThatAsksForTheLane(t *testing.T) {
	s, _, _ := laneStore(t)
	m := sendTest(t, s, "mac/claude.a", "all", "calc 17*23", 600)
	real, _ := s.Send(msg.Message{From: "mac/claude.a", To: "all", Body: "an ordinary note"})

	cases := []struct {
		name   string
		labels msg.Labels
		lane   bool
		all    bool
		want   bool
	}{
		{"opted in and asks for the lane", optedIn, true, false, true},
		{"opted in, asks for the lane, lists all", optedIn, true, true, true},
		{"opted in but a read that does not ask (the hook, plain inbox, wait)", optedIn, false, false, false},
		{"opted in, does not ask, lists all", optedIn, false, true, false},
		{"asks for the lane but did not opt in", nil, true, false, false},
		{"asks for the lane, another label", msg.Labels{"tool": "claude"}, true, false, false},
		{"opted in off", msg.Labels{msg.TestLabel: "off"}, true, false, false},
	}
	for _, c := range cases {
		got := idsOf(s.InboxLane("air/codex.b", c.all, c.labels, c.lane))
		if has := contains(got, m.ID); has != c.want {
			t.Errorf("%s: the test message shown = %v, want %v (%v)", c.name, has, c.want, got)
		}
		if !contains(got, real.ID) {
			t.Errorf("%s: an ordinary message must be there whatever the lane says (%v)", c.name, got)
		}
	}
	if got := idsOf(s.Peek("air/codex.b")); contains(got, m.ID) {
		t.Errorf("a peek never shows a test message: %v", got)
	}
	if got := idsOf(s.Inbox("air/codex.b", true)); contains(got, m.ID) {
		t.Errorf("Inbox and InboxFor are plain reads: %v", got)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func TestATestThreadAndTheBoardViewsLeaveTheLaneOut(t *testing.T) {
	s, _, _ := laneStore(t)
	m := sendTest(t, s, "mac/claude.a", "air/codex.b", "root", 600)
	r, err := s.Send(msg.Message{ID: msg.NewTestID(), From: "air/codex.b", To: "mac/claude.a", Re: m.ID, Body: "answer", TTL: 600})
	if err != nil {
		t.Fatal(err)
	}
	if r.Thread != m.Thread {
		t.Fatalf("a reply stays in its thread: %q %q", r.Thread, m.Thread)
	}
	if got := s.Thread(m.ID); len(got) != 0 {
		t.Errorf("a plain thread read shows a test thread: %v", idsOf(got))
	}
	if got := idsOf(s.ThreadLane(m.ID, true)); len(got) != 2 {
		t.Errorf("a read that asks for the lane shows both: %v", got)
	}
	ord, _ := s.Send(msg.Message{From: "mac/claude.a", To: "all", Body: "an ordinary note"})
	if got := idsOf(s.Messages()); len(got) != 1 || got[0] != ord.ID {
		t.Errorf("the board (what the viewer shows) holds only the ordinary message: %v", got)
	}
	if got := idsOf(s.ArrivedSince(time.Time{})); len(got) != 1 || got[0] != ord.ID {
		t.Errorf("ArrivedSince (the unsigned report) leaves the lane out: %v", got)
	}
}

func TestStatsLeaveTheLaneOutMessagesAcksArrivalsAndRetractions(t *testing.T) {
	s, _, _ := laneStore(t)
	before := s.Stats()
	m := sendTest(t, s, "mac/claude.a", "air/codex.b", "root", 600)
	_ = s.Ack(m.ID, "air/codex.b")
	if _, err := s.Retract(m.ID, "mac/claude.a", false); err != nil {
		t.Fatalf("a cancelled test task is a retraction like any: %v", err)
	}
	if after := s.Stats(); after.Messages != before.Messages || after.Acks != before.Acks || after.LastHour != before.LastHour || after.Retracted != before.Retracted || after.RetractionsIneffective != before.RetractionsIneffective {
		t.Errorf("stats moved for a test message: before %+v after %+v", before, after)
	}
	// an inert claim on a test message does not count either
	if err := s.AckWith(msg.Ack{ID: msg.RetractPrefix + sendTest(t, s, "mac/claude.a", "air/codex.b", "root two", 600).ID, By: "other/claude.x"}); err != nil {
		t.Fatal(err)
	}
	if s.Stats().RetractionsIneffective != 0 {
		t.Errorf("an ineffective claim on a test message is counted: %+v", s.Stats())
	}
	// an ordinary message still counts, so the check above is not blind
	o, _ := s.Send(msg.Message{From: "mac/claude.a", To: "all", Body: "ordinary"})
	_ = s.Ack(o.ID, "air/codex.b")
	if st := s.Stats(); st.Messages != 1 || st.Acks != 1 || st.LastHour != 1 {
		t.Errorf("an ordinary message counts: %+v", st)
	}
}

func TestATestMessageCanBeCancelledAndNoNoticeIsMadeForIt(t *testing.T) {
	s, _, _ := laneStore(t)
	m := sendTest(t, s, "mac/claude.a", "air/codex.b", "sleep 120", 600)
	if got := s.InboxLane("air/codex.b", false, optedIn, true); len(got) != 1 {
		t.Fatalf("the worker's lane read: %v", idsOf(got))
	}
	_ = s.Ack(m.ID, "air/codex.b")
	if _, err := s.Retract(m.ID, "mac/claude.a", false); err != nil {
		t.Fatal(err)
	}
	th := s.ThreadLane(m.ID, true)
	if len(th) != 1 || th[0].Retracted == nil {
		t.Fatalf("the retraction shows in the lane's thread, as a tombstone: %+v", th)
	}
	for _, lane := range []bool{false, true} {
		for _, x := range s.InboxLane("air/codex.b", false, optedIn, lane) {
			if x.From == msg.NoticeFrom {
				t.Errorf("a retracted test message makes no notice (lane %v): %+v", lane, x)
			}
		}
	}
}

func TestATestMessageHasRateLimitsOfItsOwn(t *testing.T) {
	s, _, _ := laneStore(t)
	s.SetSendLimit(3)
	for i := 0; i < 3; i++ { // a brake test uses up the test bucket ...
		sendTest(t, s, "mac/claude.a", "air/codex.b", strings.Repeat("x", i+1), 600)
	}
	if _, err := s.Send(msg.Message{ID: msg.NewTestID(), From: "mac/claude.a", To: "air/codex.b", Body: "one more test", TTL: 600}); err == nil {
		t.Error("the test bucket has a limit too: the brake being tested is the real one")
	}
	for i := 0; i < 3; i++ { // ... and the real session still has all of its own
		if _, err := s.Send(msg.Message{From: "mac/claude.a", To: "air/codex.b", Body: strings.Repeat("y", i+1)}); err != nil {
			t.Fatalf("a brake test used up the real session's allowance: %v", err)
		}
	}
	if _, err := s.Send(msg.Message{From: "mac/claude.a", To: "air/codex.b", Body: "a fourth real one"}); err == nil {
		t.Error("the real bucket still limits")
	}
	if st := s.Stats(); st.RateLimited != 1 || st.RefusedBy["rate_sender"] != 1 {
		t.Errorf("only the real refusal is a statistic: %+v", st)
	}
}

func TestAMessageWithATestIdButNoShortLifeIsAnOrdinaryOne(t *testing.T) {
	s, _, _ := laneStore(t)
	for name, m := range map[string]msg.Message{
		"no ttl":      {ID: msg.NewTestID(), From: "mac/claude.a", To: "all", Body: "looks like a test id by chance"},
		"a long ttl":  {ID: msg.NewTestID(), From: "mac/claude.a", To: "all", Body: "a long one", TTL: 3600},
		"for a human": {ID: msg.NewTestID(), From: "mac/claude.a", To: "air/human", Body: "needs you", TTL: 300},
		"a lease":     {ID: msg.NewTestID(), From: "mac/claude.a", To: "mac/claude.a", Kind: msg.KindLease, Body: "lease x", TTL: 300},
	} {
		out, err := s.Send(m)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out.IsTest() {
			t.Errorf("%s: stored as a test message", name)
		}
	}
	if got := idsOf(s.Inbox("air/codex.b", false)); len(got) != 2 {
		t.Errorf("the two bulletins are ordinary and delivered: %v", got)
	}
	if s.Stats().Messages != 4 {
		t.Errorf("all four count: %+v", s.Stats())
	}
}

func TestAPeerSendsATestRecordAndItIsHiddenAndPurgedHereToo(t *testing.T) {
	a, now, _ := laneStore(t)
	m := sendTest(t, a, "mac/claude.a", "air/codex.b", "from a peer", 600)
	b, err := Open(t.TempDir(), "n2")
	if err != nil {
		t.Fatal(err)
	}
	b.SetClock(func() time.Time { return *now })
	if n, err := b.Merge(a.Since(map[string]uint64{}, 100)); err != nil || n != 1 {
		t.Fatalf("merge: %d %v", n, err)
	}
	if got := idsOf(b.Inbox("air/codex.b", true)); contains(got, m.ID) {
		t.Errorf("a replicated test message is hidden on the peer: %v", got)
	}
	if got := idsOf(b.InboxLane("air/codex.b", false, optedIn, true)); !contains(got, m.ID) {
		t.Errorf("and shown to a lane read of an opted-in reader: %v", got)
	}
	if b.Stats().Messages != 0 {
		t.Errorf("not counted on the peer: %+v", b.Stats())
	}
	*now = now.Add(11 * time.Minute)
	b.Stats() // any read sweeps
	for _, r := range b.Since(map[string]uint64{}, 10) {
		if r.Msg != nil && r.Msg.Body != "" {
			t.Errorf("the peer still holds the body after the purge: %+v", r.Msg)
		}
	}
}

func TestThePurgeReplacesABodyByItsStubInMemoryAndOnTheFeed(t *testing.T) {
	s, now, dir := laneStore(t)
	ord, _ := s.Send(msg.Message{From: "mac/claude.a", To: "all", Body: "an ordinary note that stays"})
	m := sendTest(t, s, "mac/claude.a", "air/codex.b", "secret words of a test", 600)
	_ = s.Ack(m.ID, "air/codex.b")
	holds := func(s *Store) bool {
		for _, r := range s.Since(map[string]uint64{}, 100) {
			if r.Msg != nil && r.Msg.ID == m.ID && r.Msg.Body != "" {
				return true
			}
		}
		return false
	}
	if !holds(s) {
		t.Fatal("the feed carries the test message while it lives")
	}

	*now = now.Add(9 * time.Minute) // not yet expired
	s.Stats()
	if !holds(s) {
		t.Fatal("purged before it expired")
	}

	*now = now.Add(2 * time.Minute) // expired (ten minutes and a minute)
	s.Stats()
	if holds(s) {
		t.Fatal("the feed still serves the body after the purge")
	}
	for _, r := range s.Since(map[string]uint64{}, 100) {
		if r.Msg != nil && r.Msg.ID == m.ID {
			if !msg.TestPurged(*r.Msg) || r.Msg.From != "mac/claude.a" || r.Msg.To != "air/codex.b" || r.Msg.Thread != m.Thread {
				t.Errorf("the feed serves the stub, which keeps who, to whom and when: %+v", r.Msg)
			}
		}
		if r.Msg != nil && r.Msg.ID == ord.ID && r.Msg.Body != "an ordinary note that stays" {
			t.Errorf("an ordinary record was touched: %+v", r.Msg)
		}
	}
	if got := s.ThreadLane(m.ID, true); len(got) != 0 {
		t.Errorf("a purged message is in no read, the lane's included: %v", idsOf(got))
	}
	if got := s.InboxLane("air/codex.b", true, optedIn, true); contains(idsOf(got), m.ID) {
		t.Errorf("nor in an inbox: %v", idsOf(got))
	}
	if ord2 := idsOf(s.Inbox("air/codex.b", true)); !contains(ord2, ord.ID) {
		t.Errorf("the ordinary message is still there: %v", ord2)
	}

	// the numbering is intact, and a node that starts from this disk agrees: it replays the record as it was
	// written and the sweep at Open replaces it by its stub again
	if vv := s.VV()["n1"]; vv != 3 {
		t.Errorf("records: %d, want 3 (ordinary, test, ack)", vv)
	}
	re, err := Open(dir, "n1")
	if err != nil || re.VV()["n1"] != 3 {
		t.Fatalf("a restart reads the log: %v %v", re.VV(), err)
	}
	if holds(re) {
		t.Error("a restarted node serves the body of a test message that expired")
	}
	if st := re.Stats(); st.Messages != 1 || st.Quarantined != 0 {
		t.Errorf("after a restart: %+v", st)
	}
	if _, err := s.Send(msg.Message{From: "mac/claude.a", To: "all", Body: "after the purge"}); err != nil {
		t.Fatal(err)
	}
	re2, _ := Open(dir, "n1")
	if re2.VV()["n1"] != 4 {
		t.Errorf("a record appended after the purge is kept: %v", re2.VV())
	}
}

func TestATestMessageThatExpiredWhileTheNodeWasDownIsPurgedAtStart(t *testing.T) {
	s, _, dir := laneStore(t)
	m := sendTest(t, s, "mac/claude.a", "air/codex.b", "was there when the node stopped", 60)
	// the node starts again later: Open sweeps with the real clock, long after the stamp (the stamp was
	// made with the test's clock, which is in the past)
	re, err := Open(dir, "n1")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range re.Since(map[string]uint64{}, 10) {
		if r.Msg != nil && (r.Msg.ID != m.ID || r.Msg.Body != "") {
			t.Errorf("a message that expired while the node was down is served with its body after a start: %+v", r.Msg)
		}
	}
	if re.VV()["n1"] != 1 || re.Stats().Messages != 0 {
		t.Errorf("%v %+v", re.VV(), re.Stats())
	}
}

func TestTwoRecordsOfOneTestMessageAreBothPurged(t *testing.T) {
	// a client that writes one message to two nodes makes a record on each, and a node that has both holds one message
	a, now, _ := laneStore(t)
	m := sendTest(t, a, "mac/claude.a", "air/codex.b", "written to two nodes", 60)
	b, _ := Open(t.TempDir(), "n2")
	b.SetClock(func() time.Time { return *now })
	if _, err := b.Send(m); err != nil { // the same id, as that client sends it to the second node
		t.Fatal(err)
	}
	if _, err := a.Merge(b.Since(map[string]uint64{}, 10)); err != nil {
		t.Fatal(err)
	}
	if a.Stats().Messages != 0 || len(a.VV()) != 2 {
		t.Fatalf("a holds two records of one message: %v %+v", a.VV(), a.Stats())
	}
	*now = now.Add(2 * time.Minute)
	a.Stats()
	for _, r := range a.Since(map[string]uint64{}, 10) {
		if r.Msg != nil && r.Msg.Body != "" {
			t.Errorf("a record of the message kept its body: %+v", r.Msg)
		}
	}
}
