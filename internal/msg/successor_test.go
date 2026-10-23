package msg

import (
	"strings"
	"testing"
	"time"
)

func TestASuccessorIdIsATenDigitHexIdWithItsOwnShape(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := NewSuccessorID()
		if !ValidID(id) || !IsSuccessorID(id) || IsTestID(id) || !strings.HasPrefix(id, SuccessorIDPrefix) {
			t.Fatalf("%q is not a valid successor id", id)
		}
	}
	for _, id := range []string{"", "5c", "0123456789", NewTestID(), "5c1234567", "5c123456789", "5C12345678"} {
		if IsSuccessorID(id) {
			t.Errorf("IsSuccessorID(%q) = true", id)
		}
	}
	id := NewSuccessorID()
	bad := id[:9] + string("0123456789abcdef"[(int(nibble(id[9]))+1)%16]) // the wrong check digit
	if IsSuccessorID(bad) {
		t.Errorf("a wrong check digit passed: %s", bad)
	}
}

func TestASuccessorIdKeepsTwentyEightRandomBitsBecauseANodeKeepsTheFirstOfTwoMessagesThatShareAnId(t *testing.T) {
	seen := make([]map[byte]bool, 9)
	for i := range seen {
		seen[i] = map[byte]bool{}
	}
	for i := 0; i < 64; i++ {
		id := NewSuccessorID()
		for p := 0; p < 9; p++ {
			seen[p][id[p]] = true
		}
	}
	for p := len(SuccessorIDPrefix); p < 9; p++ {
		if len(seen[p]) < 2 {
			t.Errorf("digit %d never varied in 64 draws: the mark takes more than its 12 bits", p)
		}
	}
	if len(SuccessorIDPrefix) != 2 {
		t.Errorf("the mark's prefix is %q: every digit it takes is 4 bits fewer that stay random", SuccessorIDPrefix)
	}
}

func TestNewIDNeverMakesASuccessorId(t *testing.T) {
	draws := [][]byte{mustHex(t, NewSuccessorID()), mustHex(t, "0123456789")}
	old := randRead
	defer func() { randRead = old }()
	randRead = func(b []byte) {
		copy(b, draws[0])
		if len(draws) > 1 {
			draws = draws[1:]
		}
	}
	if got := NewID(); got != "0123456789" {
		t.Fatalf("NewID() = %q after a draw with the mark, want it to draw again", got)
	}
}

func marked(to, kind, body string) Message {
	return Message{ID: NewSuccessorID(), From: "mac/claude.asker", To: to, Kind: kind, Body: body}
}

func TestWhatANodeMayHandToASuccessor(t *testing.T) {
	plain := Message{ID: "0123456789", From: "mac/claude.asker", To: "mac/claude.7430", Body: "x"}
	cases := []struct {
		name string
		m    Message
		want bool
	}{
		{"a marked note to one session", marked("mac/claude.7430", "", "x"), true},
		{"a marked question", marked("mac/claude.7430", KindQuestion, "x"), true},
		{"a marked answer", marked("mac/claude.7430", KindAnswer, "x"), true},
		{"the same note without the mark", plain, false},
		{"a task is one conversation's bookkeeping", marked("mac/claude.7430", KindTask, "x"), false},
		{"a claim", marked("mac/claude.7430", KindClaim, "x"), false},
		{"progress", marked("mac/claude.7430", KindProgress, "x"), false},
		{"an end", marked("mac/claude.7430", KindEnd, "x"), false},
		{"a lease", marked("mac/claude.7430", KindLease, "x"), false},
		{"to a machine", marked("mac", "", "x"), false},
		{"to every session of a tool", marked("mac/claude", "", "x"), false},
		{"to a selector", marked("@repo=x", "", "x"), false},
		{"to all", marked("all", "", "x"), false},
		{"to the person on the machine", marked("mac/human", "", "x"), false},
		{"to a persistent name, whose mail waits for the name", marked("mac/codex.build1", "", "x"), false},
		{"to a name that is no tool's", marked("mac/build1", "", "x"), false},
		{"to a join probe", marked("mac/join-reader", "", "x"), false},
		{"a token hand sent to a session by name", marked("mac/claude.7430", "", TokenHandTag+" aivoicebus token install"), false},
		{"an approval request sent to a session by name", marked("mac/claude.7430", "", ApprovalTag+" run it"), false},
		{"an ask-human question sent to a session by name", marked("mac/claude.7430", "", strings.TrimSpace(AskPrefix)+" which one?"), false},
		{"a task for the person", marked("mac/claude.7430", "", PersonTag+" do it"), false},
		{"a note to yourself", Message{ID: NewSuccessorID(), From: "mac/claude.7430", To: "mac/claude.7430", Body: "x"}, false},
		{"from the node's own notice sender", Message{ID: NewSuccessorID(), From: SystemHost + "/notice", To: "mac/claude.7430", Body: "x"}, false},
	}
	for _, c := range cases {
		if got := c.m.Inheritable(); got != c.want {
			t.Errorf("%s: Inheritable() = %v, want %v", c.name, got, c.want)
		}
	}
	m := marked("mac/claude.7430", "", "x")
	m.Exp, m.At = ptr(time.Unix(1000, 0)), time.Unix(1000-60, 0)
	if m.IsTest() || !m.Inheritable() {
		t.Errorf("a marked message with a lifetime is not a test and still inheritable")
	}
}

func ptr[T any](v T) *T { return &v }

func TestPersonTaggedIgnoresWhoItWasSentTo(t *testing.T) {
	for _, to := range []string{"mac/claude.7430", "mac/human", "mac"} {
		if !PersonTagged(Message{To: to, Body: "[approval needed] x\nmore"}) {
			t.Errorf("an approval request to %s is the person's", to)
		}
		if PersonTagged(Message{To: to, Body: "no tag [approval needed]"}) {
			t.Errorf("a tag that is not at the start of the first line is not a tag (%s)", to)
		}
	}
	// PersonOnly keeps its meaning: a tagged message sent to one session by name is that session's to take
	if PersonOnly(Message{To: "mac/claude.7430", Body: PersonTag + " x"}) {
		t.Error("PersonOnly changed for a tagged message sent to one session")
	}
	if !PersonOnly(Message{To: "mac/claude", Body: PersonTag + " x"}) || !PersonOnly(Message{To: "mac/human", Body: "x"}) {
		t.Error("PersonOnly changed for a tagged message to a tool, or one to the person")
	}
}

func TestGoneMeansUnheardForFifteenMinutesOutsideAWait(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		e    RosterEntry
		want bool
	}{
		{"seen now", RosterEntry{Seen: now}, false},
		{"seen 14m59s ago", RosterEntry{Seen: now.Add(-GoneAfter + time.Second)}, false},
		{"seen 15m ago", RosterEntry{Seen: now.Add(-GoneAfter)}, true},
		{"seen yesterday", RosterEntry{Seen: now.Add(-24 * time.Hour)}, true},
		{"in a wait, whatever its last sighting", RosterEntry{Seen: now.Add(-24 * time.Hour), Listening: true}, false},
		{"a sighting ahead of the clock", RosterEntry{Seen: now.Add(time.Hour)}, false},
	} {
		if got := Gone(c.e, now); got != c.want {
			t.Errorf("%s: Gone = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSuccessorChoice(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	live := now.Add(-time.Minute)
	old := now.Add(-3 * time.Hour)
	repo := Labels{"repo": "api", "tool": "claude"}
	gone := RosterEntry{Me: "mac/claude.7430", Seen: old, Labels: repo}
	started := map[string]time.Time{}
	since := func(me string) time.Time { return started[me] }
	e := func(me string, seen time.Time, l Labels) RosterEntry {
		return RosterEntry{Me: me, Seen: seen, Labels: l}
	}
	pick := func(roster ...RosterEntry) string {
		got, ok := Successor(gone, roster, since, now)
		if !ok {
			return ""
		}
		return got.Me
	}

	if got := pick(); got != "" {
		t.Errorf("an empty roster has no successor, got %q", got)
	}
	if got := pick(e("mac/claude.aaaa", live, repo)); got != "mac/claude.aaaa" {
		t.Errorf("the live session in the same repo, got %q", got)
	}
	started["mac/claude.old1"], started["mac/claude.new1"] = now.Add(-5*time.Hour), now.Add(-10*time.Minute)
	if got := pick(e("mac/claude.old1", live, repo), e("mac/claude.new1", live, repo)); got != "mac/claude.new1" {
		t.Errorf("the one first heard of most recently wins, got %q", got)
	}
	if got := pick(e("mac/claude.aaaa", live, repo), e("mac/claude.bbbb", live.Add(time.Second), repo)); got != "mac/claude.bbbb" {
		t.Errorf("with no first sightings the most recently seen wins, got %q", got)
	}
	if got := pick(e("mac/claude.bbbb", live, repo), e("mac/claude.aaaa", live, repo)); got != "mac/claude.aaaa" {
		t.Errorf("a full tie is settled by name, got %q", got)
	}
	for name, other := range map[string]RosterEntry{
		"another machine":                  e("air/claude.aaaa", live, repo),
		"another repo":                     e("mac/claude.aaaa", live, Labels{"repo": "web", "tool": "claude"}),
		"another tool":                     e("mac/codex.aaaa", live, Labels{"repo": "api", "tool": "codex"}),
		"no repo at all":                   e("mac/claude.aaaa", live, Labels{"tool": "claude"}),
		"gone itself":                      e("mac/claude.aaaa", old, repo),
		"the same repo name, other origin": e("mac/claude.aaaa", live, Labels{"repo": "api", "tool": "claude", "origin": "x/api"}),
		"a join probe":                     e("mac/join-reader", live, repo),
		"itself":                           e("mac/claude.7430", live, repo),
	} {
		g := gone
		if name == "the same repo name, other origin" {
			g.Labels = Labels{"repo": "api", "tool": "claude", "origin": "y/api"}
		}
		if got, ok := Successor(g, []RosterEntry{other}, since, now); ok {
			t.Errorf("%s must not take over, got %q", name, got.Me)
		}
	}
	if _, ok := Successor(RosterEntry{Me: "mac/claude.7430", Seen: old, Labels: Labels{"tool": "claude"}}, []RosterEntry{e("mac/claude.aaaa", live, Labels{"tool": "claude"})}, since, now); ok {
		t.Error("two sessions with no repository have nothing in common")
	}
	if got, ok := Successor(gone, []RosterEntry{{Me: "mac/claude.aaaa", Seen: old, Listening: true, Labels: repo}}, since, now); !ok || got.Me != "mac/claude.aaaa" {
		t.Errorf("a session in a wait is live, got %q %v", got.Me, ok)
	}
}

func TestARoleHolderOnTheSameMachineBeatsTheNewestInTheRepo(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	live := now.Add(-time.Minute)
	gone := RosterEntry{Me: "mac/claude.7430", Seen: now.Add(-2 * time.Hour), Labels: Labels{"repo": "api", "tool": "claude", RoleLabel: "build+review"}}
	sameRepo := RosterEntry{Me: "mac/claude.aaaa", Seen: live, Labels: Labels{"repo": "api", "tool": "claude"}}
	holder := RosterEntry{Me: "mac/codex.bbbb", Seen: live, Labels: Labels{"repo": "other", "tool": "codex", RoleLabel: "review"}}
	elsewhere := RosterEntry{Me: "air/claude.cccc", Seen: live, Labels: Labels{"repo": "api", "tool": "claude", RoleLabel: "build"}}
	got, ok := Successor(gone, []RosterEntry{sameRepo, holder, elsewhere}, nil, now)
	if !ok || got.Me != "mac/codex.bbbb" {
		t.Errorf("the holder of a role the gone session claimed, on its machine, comes first: got %q %v", got.Me, ok)
	}
	got, ok = Successor(gone, []RosterEntry{sameRepo, elsewhere}, nil, now)
	if !ok || got.Me != "mac/claude.aaaa" {
		t.Errorf("a role claimed on another machine takes nobody's mail: got %q %v", got.Me, ok)
	}
}

func TestInheritedFromNamesTheSessionTheMessageWasWrittenTo(t *testing.T) {
	if from, ok := InheritedFrom(Message{To: "mac/claude.7430"}, "mac/claude.8494"); !ok || from != "mac/claude.7430" {
		t.Errorf("got %q %v", from, ok)
	}
	// an address that names a tool (any word before the dot) reaches every session of it: nothing was handed on
	for _, m := range []Message{{To: "mac/claude.8494"}, {To: "all"}, {To: "mac"}, {To: "mac/claude"}, {To: "@repo=x"}, {To: "mac/*"}, {To: "mac/amend"}, {To: "*/amend"}} {
		me := "mac/claude.8494"
		if m.To == "mac/amend" || m.To == "*/amend" {
			me = "mac/amend.r1"
		}
		if from, ok := InheritedFrom(m, me); ok {
			t.Errorf("a message to %s is not inherited by %s (%q)", m.To, me, from)
		}
	}
	for _, m := range []Message{{To: "mac/claude.8494"}, {To: "all"}, {To: "mac"}, {To: "mac/claude"}, {To: "@repo=x"}, {To: "mac/*"}} {
		if from, ok := InheritedFrom(m, "mac/claude.8494"); ok {
			t.Errorf("a message to %s is not inherited (%q)", m.To, from)
		}
	}
}

func TestPersistentSessionNames(t *testing.T) {
	for to, want := range map[string]bool{
		"mac/codex.build1": true, "mac/build1": true, "mac/claude.7430": false, "mac/claude.a1b2": false,
		"mac/claude": false, "mac": false, "mac/mybot.nightly": true,
	} {
		if got := IsPersistentSession(to); got != want {
			t.Errorf("IsPersistentSession(%q) = %v, want %v", to, got, want)
		}
	}
}
