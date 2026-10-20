package hook

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestTierAndBroadcast(t *testing.T) {
	const me = "host/claude.a1b2"
	cases := []struct {
		to        string
		tier      int
		broadcast bool
	}{
		{"host/claude.a1b2", tierSession, false},
		{"*/claude.a1b2", tierSession, false},
		{"host/claude", tierHostTool, false},
		{"@repo=x,tool=claude", tierSlice, true},
		{"@machine=host", tierSlice, true},
		{"host", tierSlice, true},
		{"host/*", tierSlice, true},
		{"*/claude", tierTool, true},
		{"*/*", tierAll, true},
		{"all", tierAll, true},
	}
	for _, c := range cases {
		if got := tier(c.to, me); got != c.tier {
			t.Errorf("tier(%q) = %d, want %d", c.to, got, c.tier)
		}
		if got := broadcast(c.to, me); got != c.broadcast {
			t.Errorf("broadcast(%q) = %v, want %v", c.to, got, c.broadcast)
		}
		if got := msg.IsBroadcast(c.to); got != c.broadcast { // what view.Headlines shortens by
			t.Errorf("msg.IsBroadcast(%q) = %v, want %v", c.to, got, c.broadcast)
		}
	}
	// An identity without a session part is still exactly itself.
	if got := tier("air/codex", "air/codex"); got != tierSession {
		t.Errorf("a target equal to my identity is one session, got tier %d", got)
	}
	if broadcast("air/codex", "air/codex.zz9") || tier("air/codex", "air/codex.zz9") != tierHostTool {
		t.Error("host/tool is one tool on one machine, directed")
	}
}

func TestRankNarrowestAudienceFirstAndBroadcastsNewestFirst(t *testing.T) {
	const me = "host/claude.a1b2"
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	mk := func(to string, minute int) msg.Message {
		return msg.Message{ID: fmt.Sprintf("%s@%d", to, minute), To: to, At: t0.Add(time.Duration(minute) * time.Minute)}
	}
	in := []msg.Message{
		mk("all", 1),
		mk("*/claude", 2),
		mk("host/claude", 3),
		mk("all", 4),
		mk("host/claude.a1b2", 5),
		mk("@repo=x", 6),
		mk("host/claude", 7),
		mk("*/claude", 8),
	}
	got := rank(in, me)
	var ids []string
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	// This session; one tool here (arrival order: the oldest ask has waited longest); a
	// slice of the fleet; every session of a tool, then everybody, each newest first.
	want := []string{
		"host/claude.a1b2@5",
		"host/claude@3", "host/claude@7",
		"@repo=x@6",
		"*/claude@8", "*/claude@2",
		"all@4", "all@1",
	}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Errorf("order:\n got %v\nwant %v", ids, want)
	}
	if in[0].ID != "all@1" || in[7].ID != "*/claude@8" {
		t.Error("rank must not reorder its input")
	}
}

func TestChooseCapsBroadcastsAtTwoAndNeverLetsThemCrowdOutDirectedMessages(t *testing.T) {
	const me = "air/codex"
	d := func(n int) []msg.Message { // directed to me
		var ms []msg.Message
		for i := 0; i < n; i++ {
			ms = append(ms, msg.Message{ID: fmt.Sprintf("d%d", i), To: me})
		}
		return ms
	}
	b := func(n int) []msg.Message { // to everybody
		var ms []msg.Message
		for i := 0; i < n; i++ {
			ms = append(ms, msg.Message{ID: fmt.Sprintf("b%d", i), To: "all"})
		}
		return ms
	}
	cases := []struct {
		name                       string
		in                         []msg.Message
		shown, more, waiting, bcsh int // bcsh: broadcasts among shown
	}{
		{"directed only, under the cap", d(3), 3, 0, 0, 0},
		{"directed only, over the cap", d(7), 5, 2, 0, 0},
		{"broadcasts only", b(5), 2, 0, 3, 2},
		{"directed first, then two broadcast slots", append(d(3), b(4)...), 5, 0, 2, 2},
		{"directed fill every slot, broadcasts wait", append(d(5), b(3)...), 5, 0, 3, 0},
		{"the total cap still holds, with one broadcast slot left", append(d(4), b(3)...), 5, 0, 2, 1},
		{"over the cap on both counts", append(d(7), b(3)...), 5, 2, 3, 0},
	}
	for _, c := range cases {
		shown, more, waiting := choose(c.in, me)
		bcsh := 0
		for _, m := range shown {
			if broadcast(m.To, me) {
				bcsh++
			}
		}
		if len(shown) != c.shown || more != c.more || waiting != c.waiting || bcsh != c.bcsh {
			t.Errorf("%s: shown %d (%d broadcasts), more %d, waiting %d; want %d (%d), %d, %d",
				c.name, len(shown), bcsh, more, waiting, c.shown, c.bcsh, c.more, c.waiting)
		}
	}
	// An urgent broadcast ranks ahead of a directed ask, and a third one does not stop the ask.
	ranked := []msg.Message{{ID: "u1", To: "all"}, {ID: "u2", To: "all"}, {ID: "u3", To: "all"}, {ID: "ask", To: me}}
	shown, more, waiting := choose(ranked, me)
	if len(shown) != 3 || shown[2].ID != "ask" || more != 0 || waiting != 1 {
		t.Errorf("shown %v more %d waiting %d", shown, more, waiting)
	}
}

// sendAt stores m as if the node received it at time at.
func sendAt(t *testing.T, s *store.Store, at time.Time, m msg.Message) {
	t.Helper()
	s.SetClock(func() time.Time { return at })
	defer s.SetClock(time.Now)
	if _, err := s.Send(m); err != nil {
		t.Fatal(err)
	}
}

// inOrder fails unless every part appears in got, each after the one before it.
func inOrder(t *testing.T, got string, parts ...string) {
	t.Helper()
	last := -1
	for _, p := range parts {
		i := strings.Index(got, p)
		if i < 0 {
			t.Errorf("missing %q:\n%s", p, got)
			return
		}
		if i < last {
			t.Errorf("%q is out of order:\n%s", p, got)
			return
		}
		last = i
	}
}

// unreadBodies is the bodies air/codex still has unread, in the order the node lists them.
func unreadBodies(s *store.Store) []string {
	var out []string
	for _, m := range s.Inbox("air/codex", false) {
		out = append(out, m.Body)
	}
	return out
}

func TestADirectAskIsShownBehindFiveOldBroadcasts(t *testing.T) {
	s, _ := mesh(t)
	now := time.Now()
	for i := 1; i <= 5; i++ { // five greetings and release notes, the oldest first
		sendAt(t, s, now.Add(-time.Duration(6-i)*time.Hour), msg.Message{From: "other/agent", To: "all", Body: fmt.Sprintf("greeting-%d", i)})
	}
	sendAt(t, s, now, msg.Message{From: "other/agent", To: "air/codex", Body: "please review the PR"})

	got := runHook(t, "")
	if !strings.Contains(got, "please review the PR") {
		t.Fatalf("the direct ask was buried behind the broadcasts:\n%s", got)
	}
	if n := strings.Count(got, "--- "); n != 1+maxBroadcasts {
		t.Errorf("printed %d messages, want the ask and %d broadcasts:\n%s", n, maxBroadcasts, got)
	}
	// The ask first, then the two newest broadcasts, newest first.
	inOrder(t, got, "please review the PR", "greeting-5", "greeting-4")
	for _, old := range []string{"greeting-3", "greeting-2", "greeting-1"} {
		if strings.Contains(got, old) {
			t.Errorf("%s should wait in the inbox:\n%s", old, got)
		}
	}
	for _, want := range []string{"6 unread message(s)", "+3 broadcast(s) waiting: `aivoicebus inbox`"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "more unread") {
		t.Errorf("only broadcasts were left out, so there is no \"more unread\" line:\n%s", got)
	}
	// Only what was printed is acknowledged: the three hidden broadcasts stay unread.
	if left := strings.Join(unreadBodies(s), ","); left != "greeting-1,greeting-2,greeting-3" {
		t.Errorf("unread afterwards: %s", left)
	}

	// Hidden broadcasts are not lost: the next start shows the next two, then the last.
	next := runHook(t, "")
	inOrder(t, next, "greeting-3", "greeting-2")
	if !strings.Contains(next, "+1 broadcast(s) waiting") || strings.Contains(next, "please review the PR") {
		t.Errorf("second start:\n%s", next)
	}
	last := runHook(t, "")
	if !strings.Contains(last, "greeting-1") || strings.Contains(last, "waiting") {
		t.Errorf("third start:\n%s", last)
	}
	if again := runHook(t, ""); again != "" {
		t.Errorf("everything was delivered, got %q", again)
	}
}

func TestSpecificityBeatsRecencyAcrossAudiences(t *testing.T) {
	s, _ := mesh(t)
	t.Setenv("AIVOICEBUS_LABELS", "repo=pinned")
	now := time.Now()
	sendAt(t, s, now.Add(-4*time.Hour), msg.Message{From: "other/agent", To: "@repo=pinned", Body: "for this repo"})
	sendAt(t, s, now.Add(-3*time.Hour), msg.Message{From: "other/agent", To: "all", Body: "all-oldest"})
	sendAt(t, s, now.Add(-2*time.Hour), msg.Message{From: "other/agent", To: "all", Body: "all-middle"})
	sendAt(t, s, now.Add(-1*time.Hour), msg.Message{From: "other/agent", To: "all", Body: "all-newest"})

	got := runHook(t, "")
	inOrder(t, got, "for this repo", "all-newest")
	for _, hidden := range []string{"all-oldest", "all-middle"} {
		if strings.Contains(got, hidden) {
			t.Errorf("%s should wait:\n%s", hidden, got)
		}
	}
	if !strings.Contains(got, "+2 broadcast(s) waiting") {
		t.Errorf("missing the waiting line:\n%s", got)
	}
}

func TestUrgentStillFirstAndTheBroadcastCapStillHoldsInside(t *testing.T) {
	s, _ := mesh(t)
	now := time.Now()
	sendAt(t, s, now.Add(-4*time.Hour), msg.Message{From: "other/agent", To: "air/codex", Body: "ask-direct"})
	sendAt(t, s, now.Add(-3*time.Hour), msg.Message{From: "other/agent", To: "all", Body: "ask-all"})
	sendAt(t, s, now.Add(-2*time.Hour), msg.Message{From: "other/agent", To: "all", Body: "urgent-all", Prio: msg.PrioUrgent})
	sendAt(t, s, now.Add(-1*time.Hour), msg.Message{From: "other/agent", To: "air/codex", Body: "urgent-direct", Prio: msg.PrioUrgent})

	got := runHook(t, "")
	// Urgent before ask; inside each class the message to this session before the broadcast.
	inOrder(t, got, "urgent-direct", "urgent-all", "ask-direct", "ask-all")
	if !strings.Contains(got, "2 marked urgent: tell the user now") {
		t.Errorf("missing the urgent advice:\n%s", got)
	}
	if strings.Contains(got, "waiting") { // the two broadcasts are exactly the cap
		t.Errorf("two broadcasts fit:\n%s", got)
	}
}

func TestDirectedMessagesNeverShareTheBroadcastCap(t *testing.T) {
	s, _ := mesh(t)
	now := time.Now()
	for i := 1; i <= 3; i++ {
		sendAt(t, s, now.Add(-time.Duration(10-i)*time.Hour), msg.Message{From: "other/agent", To: "air/codex", Body: fmt.Sprintf("direct-%d", i)})
	}
	for i := 1; i <= 4; i++ {
		sendAt(t, s, now.Add(-time.Duration(5-i)*time.Hour), msg.Message{From: "other/agent", To: "all", Body: fmt.Sprintf("bulletin-%d", i)})
	}
	got := runHook(t, "")
	// Three directed, then the two newest broadcasts: five in all, two broadcasts at most.
	inOrder(t, got, "direct-1", "direct-2", "direct-3", "bulletin-4", "bulletin-3")
	if n := strings.Count(got, "--- "); n != maxShown {
		t.Errorf("printed %d messages, want %d:\n%s", n, maxShown, got)
	}
	if !strings.Contains(got, "+2 broadcast(s) waiting") || strings.Contains(got, "more unread") {
		t.Errorf("wrong overflow lines:\n%s", got)
	}
}

func TestHiddenUrgentBroadcastsAreNotCountedAndSurfaceAtTheNextPrompt(t *testing.T) {
	s, _ := mesh(t)
	everyPrompt(t)
	now := time.Now()
	for i := 1; i <= 4; i++ {
		sendAt(t, s, now.Add(-time.Duration(5-i)*time.Hour), msg.Message{From: "other/agent", To: "all", Body: fmt.Sprintf("alarm-%d", i), Prio: msg.PrioUrgent})
	}
	got := runHook(t, "")
	inOrder(t, got, "alarm-4", "alarm-3")
	if !strings.Contains(got, "2 marked urgent: tell the user now") || !strings.Contains(got, "+2 broadcast(s) waiting") {
		t.Errorf("the header counts what is shown, the rest is left waiting:\n%s", got)
	}
	// The per-prompt hook is not subject to the broadcast cap: urgent news that did not fit
	// at session start reaches the session with its next prompt.
	prompt := promptHook(t, "")
	for _, want := range []string{"2 URGENT message(s) arrived", "alarm-2", "alarm-1"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt hook missing %q:\n%s", want, prompt)
		}
	}
	if len(unreadBodies(s)) != 0 {
		t.Errorf("everything was shown by now: %v", unreadBodies(s))
	}
}
