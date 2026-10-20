package outbox

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestLintPriority(t *testing.T) {
	cases := []struct {
		name, body, prio, kind string
		lower                  bool
		warn                   bool
	}{
		// said to need nothing, no priority chosen: lowered
		{name: "starts with FYI", body: "FYI the build is green", lower: true},
		{name: "starts with DONE", body: "DONE: merged and deployed", lower: true},
		{name: "starts with no action", body: "no action needed from you", lower: true},
		{name: "lowercase", body: "fyi: rotated the log", lower: true},
		{name: "brackets and markdown before the word", body: "**[FYI]** cache cleared", lower: true},
		{name: "no reply needed in the middle", body: "Deployed 0.11.5 to the build host. No reply needed.", lower: true},
		{name: "no action needed in the middle", body: "Disk is fine now (no action needed).", lower: true},
		{name: "a question mark in a URL is not a question", body: "FYI the run is at https://ci.example.com/run?id=7 and passed", lower: true},
		{name: "a question mark in inline code is not a question", body: "FYI `grep -E 'a.*?b'` is slow on that file", lower: true},
		{name: "a question mark in a code block is not a question", body: "DONE\n```\nx := a ? b : c\n```\nbuilt", lower: true},

		// the words ask for something: keep the priority
		{name: "FYI with a question", body: "FYI the host is full, who fixes it?"},
		{name: "no action needed but a question", body: "no action needed. Or should the job move?"},
		{name: "reply to me", body: "FYI the disk filled up, reply to me with who owns it"},
		{name: "who owns without a question mark", body: "DONE with the audit; who owns vm701 tuning"},
		{name: "FYI that still asks for something (please)", body: "FYI from the pools session. Please do not start another install"},
		{name: "FYI that says tell me", body: "FYI I stopped the restarts; if that is wrong, tell me."},
		{name: "FYI that says let me know", body: "DONE, let me know if it breaks"},
		{name: "a curly apostrophe don't", body: "FYI don’t touch the lease"},
		{name: "a plain message", body: "the deploy window moved to 5pm"},
		{name: "doneness inside a word", body: "Donetsk office is closed today"},
		{name: "FYI inside a word", body: "fyiwhatever happened"},
		{name: "marker only later in the text", body: "I am done with the migration"},

		// an explicit priority stands, whatever the words say
		{name: "explicit ask", body: "FYI the build is green", prio: "ask"},
		{name: "explicit urgent", body: "FYI the build is red", prio: "urgent"},
		{name: "explicit fyi, nothing asked", body: "FYI the build is green", prio: "fyi"},

		// fyi that asks: warned
		{name: "fyi with a question mark", body: "can anyone take the vacuum job?", prio: "fyi", warn: true},
		{name: "fyi with reply to me", body: "disk at 91%, reply to me if you own it", prio: "FYI", warn: true},
		{name: "fyi with who owns", body: "who owns the gateway", prio: "fyi", warn: true},
		{name: "fyi question kind", body: "is it safe to restart?", prio: "fyi", kind: "question", warn: true},
		{name: "fyi without a question", body: "lunch at noon", prio: "fyi"},

		// kinds that are not a plain message are never touched
		{name: "a task", body: "FYI run the audit", kind: "task"},
		{name: "a task at fyi that asks", body: "who owns this?", prio: "fyi", kind: "task"},
		{name: "an answer", body: "DONE, merged", kind: "answer"},
		{name: "a progress note", body: "DONE with step 2", kind: "progress"},
		{name: "a question kind is not lowered", body: "FYI the build is green", kind: "question"},
	}
	for _, c := range cases {
		got := LintPriority(c.body, c.prio, c.kind)
		if got.Lower != c.lower {
			t.Errorf("%s: lower = %v, want %v", c.name, got.Lower, c.lower)
		}
		if (got.Warning != "") != c.warn {
			t.Errorf("%s: warning = %q, want one: %v", c.name, got.Warning, c.warn)
		}
		if c.warn && !strings.Contains(got.Warning, "--prio ask") {
			t.Errorf("%s: the warning does not say how to interrupt: %q", c.name, got.Warning)
		}
	}
	// A plain message is told to add the kind; a question already has it.
	if w := LintPriority("who owns it?", "fyi", "").Warning; !strings.Contains(w, "--kind question --prio ask") {
		t.Errorf("%q", w)
	}
	if w := LintPriority("who owns it?", "fyi", "question").Warning; strings.Contains(w, "--kind") {
		t.Errorf("a question needs only --prio ask: %q", w)
	}
}

// t0 is a clock for the staleness tests: no test reads the real time.
var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func seen(me string, ago time.Duration, labels ...string) msg.RosterEntry {
	return msg.RosterEntry{Me: me, Seen: t0.Add(-ago), Labels: msg.ParseLabels(strings.Join(labels, ","))}
}

func TestStaleTargetUsesTheThirtyMinuteWindowOnAFixedClock(t *testing.T) {
	me := "mac14/claude.me01"
	listening := seen("mac14/claude.wait", 5*time.Hour)
	listening.Listening = true
	roster := []msg.RosterEntry{
		seen("mac14/claude.fresh", time.Minute),
		seen("mac14/claude.edge", LiveWithin),               // exactly 30m: still live (the rule ask uses)
		seen("mac14/claude.late", LiveWithin+time.Second),   // one second over
		seen("mac14/claude.old", 9*time.Hour, "repo=alpha"), // the case from the log
		listening,                                            // in a wait: live whatever Seen says
		seen("mac14/join-probe", 4*time.Hour),                // a probe is not a session
		seen("mac14/claude.me01", 4*time.Hour),               // the sender itself
		{Me: "mac14/claude.future", Seen: t0.Add(time.Hour)}, // clock skew: not stale
	}
	cases := []struct {
		to    string
		stale bool
		known bool
	}{
		{"mac14/claude.fresh", false, true},
		{"mac14/claude.edge", false, true},
		{"mac14/claude.late", true, true},
		{"mac14/claude.old", true, true},
		{"MAC14/Claude.OLD", true, true}, // names compare without case
		{"mac14/claude.wait", false, true},
		{"mac14/claude.future", false, true},
		{"mac14/claude.never", true, false}, // not in the roster at all
		{"mac14/join-probe", false, false},
		{"mac14/claude.me01", false, false},
		// not one session: never stale, whatever the roster says
		{"mac14", false, false},
		{"mac14/claude", false, false},
		{"mac14/*", false, false},
		{"*/claude", false, false},
		{"all", false, false},
		{"@repo=alpha", false, false},
		{"mac14/human", false, false},
		{"mac14/worker", false, false},
	}
	for _, c := range cases {
		st, stale := StaleTarget(roster, c.to, me, t0, LiveWithin)
		if stale != c.stale || (stale && st.Known != c.known) {
			t.Errorf("%s: stale=%v known=%v, want stale=%v known=%v", c.to, stale, st.Known, c.stale, c.known)
		}
	}
	// The window is the caller's: the same session is live in a wider one.
	if _, stale := StaleTarget(roster, "mac14/claude.late", me, t0, time.Hour); stale {
		t.Error("a 1h window counts a session seen 30m01s ago as live")
	}
	if _, stale := StaleTarget(roster, "mac14/claude.edge", me, t0, time.Minute); !stale {
		t.Error("a 1m window does not")
	}
}

func TestStaleTargetNamesWhoIsLiveOnThatMachineSameRepoFirst(t *testing.T) {
	me := "air/claude.me01"
	waiting := seen("mac14/codex.wait", 2*time.Hour, "repo=other")
	waiting.Listening = true
	roster := []msg.RosterEntry{
		seen("mac14/claude.dead", 9*time.Hour, "repo=alpha"),
		seen("mac14/codex.1111", 3*time.Minute, "repo=other"),
		seen("mac14/claude.2222", 20*time.Minute, "repo=alpha"), // the stale session's repo: first
		waiting,
		seen("mac14/claude.old2", 3*time.Hour, "repo=alpha"), // not live
		seen("mac14/join-reader", time.Minute),               // a probe
		seen("air/claude.me01", time.Minute),                 // the sender, on another machine
		seen("air/claude.3333", time.Minute, "repo=alpha"),   // another machine
	}
	st, stale := StaleTarget(roster, "mac14/claude.dead", me, t0, LiveWithin)
	if !stale || !st.Known {
		t.Fatalf("%+v", st)
	}
	var got []string
	for _, e := range st.Live {
		got = append(got, e.Me)
	}
	if want := "mac14/claude.2222 mac14/codex.wait mac14/codex.1111"; strings.Join(got, " ") != want {
		t.Errorf("live = %v, want %s (same repo, then listening, then newest)", got, want)
	}
	text := st.Refusal(t0, LiveWithin)
	for _, want := range []string{
		"refused: mac14/claude.dead was last seen", "(9h ago)", "last 30m",
		"live on mac14 now: mac14/claude.2222 (repo alpha, 20m ago), mac14/codex.wait (repo other, listening)",
		`ask mac14 "..."`, `task mac14 "..."`, "--queue (or --force)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "air/") || strings.Contains(text, "join-reader") || strings.Contains(text, "old2") {
		t.Errorf("only live sessions of that machine are named:\n%s", text)
	}

	// Nobody there: no ask suggestion (it would find no one), the other two ways remain.
	st, _ = StaleTarget([]msg.RosterEntry{seen("mac14/claude.dead", time.Hour)}, "mac14/claude.dead", me, t0, LiveWithin)
	text = st.Refusal(t0, LiveWithin)
	if !strings.Contains(text, "nobody else on mac14 was seen in the last 30m") || strings.Contains(text, "one live session there answers") || !strings.Contains(text, "task mac14") {
		t.Errorf("%s", text)
	}
	// Never seen: it says so, there is no time to show.
	st, _ = StaleTarget(nil, "mac14/claude.dead", me, t0, LiveWithin)
	if text = st.Refusal(t0, LiveWithin); !strings.Contains(text, "mac14/claude.dead has not been seen by the nodes") {
		t.Errorf("%s", text)
	}
	// Many live: five are named, the rest counted.
	var crowd []msg.RosterEntry
	for i := 0; i < 8; i++ {
		crowd = append(crowd, seen("mac14/codex.c"+string(rune('a'+i))+"00", time.Duration(i+1)*time.Minute))
	}
	st, _ = StaleTarget(crowd, "mac14/claude.dead", me, t0, LiveWithin)
	if text = st.Refusal(t0, LiveWithin); !strings.Contains(text, ", +3 more") {
		t.Errorf("%s", text)
	}
}

func TestSinceSaysJustNowUnderAMinute(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "just now", 59 * time.Second: "just now", time.Minute: "1m ago", 9 * time.Hour: "9h ago"} {
		if got := since(d); got != want {
			t.Errorf("since(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestAgoReadsInASentence(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second: "under 1m", 31 * time.Minute: "31m", 119 * time.Minute: "119m",
		2 * time.Hour: "2h", 9*time.Hour + 40*time.Minute: "9h", 47 * time.Hour: "47h", 48 * time.Hour: "2d", 100 * time.Hour: "4d",
	} {
		if got := ago(d); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}

// staleMesh is a node that knows a dead session (seen two hours ago) and a live one on the same machine.
func staleMesh(t *testing.T) (*client.Client, *store.Store) {
	t.Helper()
	c, s := meshWith(t, "me/claude.0001", nil)
	s.SetClock(func() time.Time { return time.Now().Add(-2 * time.Hour) })
	s.InboxFor("mac14/claude.dead", false, msg.ParseLabels("repo=alpha"))
	s.SetClock(time.Now)
	s.InboxFor("mac14/claude.live", false, msg.ParseLabels("repo=alpha"))
	return c, s
}

func TestPrepareRefusesAStaleExactSessionOnlyForAuthoredMessages(t *testing.T) {
	c, _ := staleMesh(t)
	var warn strings.Builder
	_, err := Prepare(c, Request{To: "mac14/claude.dead", Body: "please look at the PR", Authored: true}, &warn)
	if err == nil || !strings.Contains(err.Error(), "refused: mac14/claude.dead was last seen") || !strings.Contains(err.Error(), "mac14/claude.live") {
		t.Fatalf("an authored message to a session seen 2h ago: %v", err)
	}
	for name, r := range map[string]Request{
		"--queue":          {To: "mac14/claude.dead", Body: "please look at the PR", Authored: true, Queue: true},
		"--force":          {To: "mac14/claude.dead", Body: "please look at the PR", Authored: true, Force: true},
		"not authored":     {To: "mac14/claude.dead", Body: "please look at the PR"},
		"a live session":   {To: "mac14/claude.live", Body: "please look at the PR", Authored: true},
		"the whole tool":   {To: "mac14/claude", Body: "please look at the PR", Authored: true},
		"the whole host":   {To: "mac14", Body: "please look at the PR", Authored: true},
		"a selector":       {To: "@repo=alpha", Body: "please look at the PR", Authored: true},
		"a session no one": {To: "mac14/claude.nobody", Body: "x", Authored: true, Queue: true},
	} {
		if _, err := Prepare(c, r, &warn); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Prepare(c, Request{To: "mac14/claude.never", Body: "hello", Authored: true}, &warn); err == nil || !strings.Contains(err.Error(), "has not been seen by the nodes") {
		t.Errorf("a session the nodes never heard of: %v", err)
	}
}

func TestPrepareWarnsOnAStaleReplyTargetAndSendsIt(t *testing.T) {
	c, s := staleMesh(t)
	s.SetClock(func() time.Time { return time.Now().Add(-2 * time.Hour) }) // the asker's last word was two hours ago
	orig, err := c.Send(msg.Message{ID: msg.NewID(), From: "mac14/claude.dead", To: "me/claude.0001", Body: "can you check?"})
	s.SetClock(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var warn strings.Builder
	p, err := Prepare(c, Request{Re: orig.ID, Body: "checked, it is fine", Authored: true, TargetSaid: true}, &warn)
	if err != nil {
		t.Fatalf("a reply is not refused for its asker having gone: %v", err)
	}
	if p.Msg.To != "mac14/claude.dead" || !strings.Contains(warn.String(), "warning: the reply goes to mac14/claude.dead") || !strings.Contains(warn.String(), "mac14/claude.live") {
		t.Errorf("to=%s warn=%q", p.Msg.To, warn.String())
	}
	// The same words with --to naming the dead session are refused: the sender chose it.
	if _, err := Prepare(c, Request{To: "mac14/claude.dead", Re: orig.ID, Body: "checked, it is fine", Authored: true}, &warn); err == nil {
		t.Error("an explicit --to is the sender's choice, and is checked")
	}
}

func TestPrepareLowersAMessageThatSaysItNeedsNothing(t *testing.T) {
	c, _ := staleMesh(t)
	var warn strings.Builder
	p, err := Prepare(c, Request{To: "mac14/claude.live", Body: "FYI the build is green", Authored: true}, &warn)
	if err != nil || p.Msg.Prio != msg.PrioFYI || !p.Lowered || p.Context != 0 || p.Notes[len(p.Notes)-1] != "(sent as fyi; --prio ask to interrupt)" {
		t.Fatalf("lowered: %v %+v", err, p)
	}
	var out strings.Builder
	p.Describe(&out)
	if !strings.Contains(out.String(), "priority  fyi") || !strings.Contains(out.String(), "lowered to fyi") {
		t.Errorf("a dry run says so:\n%s", out.String())
	}
	for name, r := range map[string]Request{
		"not authored (a tool's own message)": {To: "mac14/claude.live", Body: "FYI the build is green"},
		"an explicit priority":                {To: "mac14/claude.live", Body: "FYI the build is green", Prio: "ask", Authored: true},
		"a question in it":                    {To: "mac14/claude.live", Body: "FYI the build is green, ship it?", Authored: true},
		"a task":                              {To: "mac14/claude.live", Body: "FYI the build is green", Kind: "task", Authored: true},
		"a file":                              {To: "mac14/claude.live", Body: "FYI the build is green", Authored: true, File: true},
	} {
		p, err := Prepare(c, r, &warn)
		if err != nil || p.Lowered || p.Msg.Prio == msg.PrioFYI {
			t.Errorf("%s: %v lowered=%v prio=%q", name, err, p.Lowered, p.Msg.Prio)
		}
	}
	warn.Reset()
	p, err = Prepare(c, Request{To: "mac14/claude.live", Body: "who owns vm701 tuning?", Prio: "fyi", Authored: true}, &warn)
	if err != nil || p.Msg.Prio != msg.PrioFYI || !strings.Contains(warn.String(), "--kind question --prio ask") {
		t.Errorf("a question sent as fyi is warned about and still sent: %v %q", err, warn.String())
	}
	warn.Reset()
	if _, err := Prepare(c, Request{To: "mac14/claude.live", Body: "who owns vm701 tuning?", Prio: "fyi"}, &warn); err != nil || warn.Len() != 0 {
		t.Errorf("a message the tool wrote itself is not linted: %v %q", err, warn.String())
	}
}
