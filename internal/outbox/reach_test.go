package outbox

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Targets that reach nobody: a selector none of whose sessions is live, a session named without its
// machine, and one session that has read nothing for hours. Every clock here is t0.

func listening(e msg.RosterEntry) msg.RosterEntry { e.Listening = true; return e }

// readAt is e having last read (declared its labels) `ago` before t0, whatever its last-seen says.
func readAt(e msg.RosterEntry, ago time.Duration) msg.RosterEntry {
	e.LabelsAt = t0.Add(-ago)
	return e
}

func TestQuietSelectorWhenNoSessionItMatchesIsLive(t *testing.T) {
	roster := []msg.RosterEntry{
		seen("lap/claude.old", 5*time.Hour, "repo=graph"),
		seen("air/codex.newer", 2*time.Hour, "repo=graph"),
		seen("air/claude.other", 3*time.Minute, "repo=gateway"),
		seen("desk/claude.ancient", 30*time.Hour, "repo=graph"),
	}
	q, quiet := QuietSelector(roster, "@repo=graph", "me/claude.0001", t0, LiveWithin)
	if !quiet {
		t.Fatal("two sessions in repo graph, seen 2h and 5h ago, and none live: quiet")
	}
	if len(q.Matched) != 2 || q.Matched[0].Me != "air/codex.newer" || q.Matched[1].Me != "lap/claude.old" {
		t.Errorf("matched (newest first, nothing older than %s): %+v", ReachWindowText, q.Matched)
	}
	if len(q.Live) != 1 || q.Live[0].Me != "air/claude.other" {
		t.Errorf("who is live on the newest one's machine: %+v", q.Live)
	}
	if q.Target != "@repo=graph" {
		t.Errorf("target %q", q.Target)
	}

	for name, r := range map[string][]msg.RosterEntry{
		"one is listening":                {roster[0], listening(roster[1])},
		"one was seen 10m ago":            {roster[0], seen("air/codex.newer", 10*time.Minute, "repo=graph")},
		"one was seen exactly 30m ago":    {roster[0], seen("air/codex.newer", LiveWithin, "repo=graph")},
		"nobody matched in 24h":           {roster[3]},
		"nobody matches at all":           {roster[2]},
		"only the sender matches":         {seen("me/claude.0001", 3*time.Hour, "repo=graph")},
		"only a join probe matches":       {seen("lap/"+msg.ProbeReaderAgent, 3*time.Hour, "repo=graph")},
		"the sender is the live match":    {roster[0], seen("ME/claude.0001", time.Minute, "repo=graph")},
		"a live session matches a branch": {seen("lap/claude.b", time.Minute, "repo=graph,branch=main"), roster[0]},
	} {
		q, quiet := QuietSelector(r, "@repo=graph", "me/claude.0001", t0, LiveWithin)
		switch name {
		case "the sender is the live match": // the sender does not count as live for its own message
			if !quiet || len(q.Matched) != 1 {
				t.Errorf("%s: quiet=%v %+v", name, quiet, q)
			}
		default:
			if quiet {
				t.Errorf("%s: quiet: %+v", name, q)
			}
		}
	}
	if _, quiet := QuietSelector(roster, "@repo=graph", "me/claude.0001", t0, 6*time.Hour); quiet {
		t.Error("within 6h a session seen 2h ago is live")
	}
	if q, quiet := QuietSelector(append(roster[:0:0], roster[0], seen("air/codex.newer", LiveWithin+time.Second, "repo=graph")), "@repo=graph", "", t0, LiveWithin); !quiet || len(q.Matched) != 2 {
		t.Errorf("a second past the window is not live: %v %+v", quiet, q)
	}
	for _, to := range []string{"lap/claude.old", "lap", "lap/claude", "all", "*/claude"} {
		if _, quiet := QuietSelector(roster, to, "me/claude.0001", t0, LiveWithin); quiet {
			t.Errorf("%s is not a selector", to)
		}
	}
	if _, quiet := QuietSelector(roster, "  @REPO=graph ", "me/claude.0001", t0, LiveWithin); !quiet {
		t.Error("a selector is read as the send reads it: trimmed, lower case")
	}
	// Who is live on the newest one's machine: its repo first (a session there on another branch), then the others.
	branch := []msg.RosterEntry{
		seen("lap/claude.main", 2*time.Hour, "repo=graph,branch=main"),
		seen("lap/claude.other", time.Minute, "repo=gateway"),
		seen("lap/claude.dev", 20*time.Minute, "repo=graph,branch=dev"),
	}
	if q, quiet := QuietSelector(branch, "@repo=graph,branch=main", "me/claude.0001", t0, LiveWithin); !quiet || len(q.Live) != 2 || q.Live[0].Me != "lap/claude.dev" {
		t.Errorf("same repo first: %v %+v", quiet, q.Live)
	}
}

func TestQuietRefusalNamesWhoMatchedAndWhoIsLiveThere(t *testing.T) {
	roster := []msg.RosterEntry{
		seen("lap/claude.old", 5*time.Hour, "repo=graph"),
		seen("air/codex.newer", 2*time.Hour, "repo=graph"),
		seen("air/claude.other", 3*time.Minute, "repo=gateway"),
	}
	roster[1].Title = "fleet-graph deploy"
	q, _ := QuietSelector(roster, "@repo=graph", "me/claude.0001", t0, LiveWithin)
	text := q.Refusal(t0, LiveWithin)
	for _, want := range []string{
		"refused: none of the sessions @repo=graph matches is live (seen in the last 30m, or standing by), so this message may sit unread",
		"matched in the last 24h: air/codex.newer", "fleet-graph deploy", "(seen 2h ago)", "lap/claude.old (seen 5h ago)",
		"live on air now: air/claude.other (repo gateway, 3m ago)",
		`ask air "..."`,
		"run it again with --queue (or --force)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	// Nobody live on that machine: the ask line goes, the --queue line stays.
	q, _ = QuietSelector(roster[:2], "@repo=graph", "me/claude.0001", t0, LiveWithin)
	text = q.Refusal(t0, LiveWithin)
	if strings.Contains(text, "ask air") || !strings.Contains(text, "nobody else on air was seen in the last 30m") || !strings.Contains(text, "--queue") {
		t.Errorf("nobody live there:\n%s", text)
	}
	// Many matches are counted, not all named.
	var many []msg.RosterEntry
	for i := range 8 {
		many = append(many, seen(fmt.Sprintf("lap/claude.s%d", i), time.Duration(i+1)*time.Hour, "repo=graph"))
	}
	q, _ = QuietSelector(many, "@repo=graph", "me/claude.0001", t0, LiveWithin)
	if text := q.Refusal(t0, LiveWithin); !strings.Contains(text, ", +3 more") || strings.Contains(text, "lap/claude.s7") {
		t.Errorf("eight matched:\n%s", text)
	}
}

func TestBareSessionIsAToolNameAndADotWithoutAMachine(t *testing.T) {
	for _, to := range []string{"claude.f324", "codex.build1", "CLAUDE.F324", " claude.f324 ", "mcp.ab12", "cursor-agent.x1", "worker.ab12"} {
		if !BareSession(to) {
			t.Errorf("%q names a session without its machine", to)
		}
	}
	for _, to := range []string{"lap/claude.f324", "claude", "claude.", "lap", "mac14.local", "build1.claude", "@repo=claude.x", "all", "*/claude.f324",
		"owner:claude.x", "claude.f324,x", "claude.a=b", "laptop-b", ""} {
		if BareSession(to) {
			t.Errorf("%q is not a bare session name", to)
		}
	}
}

func TestResolveBareFindsTheOneLiveSessionOfThatName(t *testing.T) {
	me := "me/claude.0001"
	live := seen("lap/claude.f324", 5*time.Minute, "repo=alpha")
	gone := seen("air/claude.f324", 3*time.Hour)
	other := seen("lap/claude.zz99", time.Minute)

	to, how, err := ResolveBare([]msg.RosterEntry{other, live}, "claude.f324", me, t0, LiveWithin, false)
	if err != nil || to != "lap/claude.f324" || !strings.Contains(how, "claude.f324 -> lap/claude.f324: the one live session of that name") {
		t.Fatalf("one live: %q %q %v", to, how, err)
	}
	// One live and one gone elsewhere: the live one.
	if to, _, err := ResolveBare([]msg.RosterEntry{gone, live}, "CLAUDE.F324", me, t0, LiveWithin, false); err != nil || to != "lap/claude.f324" {
		t.Errorf("live beats gone: %q %v", to, err)
	}
	// A listening session is live however long ago it was seen.
	if to, _, err := ResolveBare([]msg.RosterEntry{listening(gone)}, "claude.f324", me, t0, LiveWithin, false); err != nil || to != "air/claude.f324" {
		t.Errorf("listening: %q %v", to, err)
	}

	// Two live: a guess, refused, both listed, and how to name one.
	_, _, err = ResolveBare([]msg.RosterEntry{live, seen("air/claude.f324", time.Minute)}, "claude.f324", me, t0, LiveWithin, false)
	if err == nil {
		t.Fatal("two live sessions of that name were guessed between")
	}
	for _, want := range []string{"refused: claude.f324 names a session without its machine", "2 live sessions have that name", "1  air/claude.f324", "2  lap/claude.f324", "name one with its machine: --to air/claude.f324"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("two live: missing %q:\n%v", want, err)
		}
	}
	// None live, one known: refused, with --queue offered; --queue keeps it for that one.
	_, _, err = ResolveBare([]msg.RosterEntry{gone, other}, "claude.f324", me, t0, LiveWithin, false)
	if err == nil || !strings.Contains(err.Error(), "no session of that name is live (seen in the last 30m, or standing by)") ||
		!strings.Contains(err.Error(), "--to air/claude.f324") || !strings.Contains(err.Error(), "add --queue") {
		t.Errorf("none live: %v", err)
	}
	if to, how, err := ResolveBare([]msg.RosterEntry{gone, other}, "claude.f324", me, t0, LiveWithin, true); err != nil || to != "air/claude.f324" || !strings.Contains(how, "not live: it waits for it") {
		t.Errorf("none live, --queue: %q %q %v", to, how, err)
	}
	// None live, two known: --queue does not pick between them.
	if _, _, err := ResolveBare([]msg.RosterEntry{gone, seen("lap/claude.f324", 2*time.Hour)}, "claude.f324", me, t0, LiveWithin, true); err == nil || !strings.Contains(err.Error(), "1  lap/claude.f324") {
		t.Errorf("two gone, --queue: %v", err)
	}
	// Nobody of that name at all.
	if _, _, err := ResolveBare([]msg.RosterEntry{other}, "claude.f324", me, t0, LiveWithin, true); err == nil ||
		!strings.Contains(err.Error(), "no session of that name has been seen by the nodes") || !strings.Contains(err.Error(), "`aivoicebus who`") {
		t.Errorf("nobody: %v", err)
	}
	// The sender is never the session meant.
	if _, _, err := ResolveBare([]msg.RosterEntry{seen("ME/claude.f324", time.Minute)}, "claude.f324", "me/claude.f324", t0, LiveWithin, false); err == nil {
		t.Error("the sender itself was picked")
	}
	// A machine that really has the name, or a target that is not bare, is left alone.
	machine := seen("claude.f324/codex.ab12", time.Hour)
	for _, c := range []struct{ to, want string }{{"claude.f324", "claude.f324"}, {"lap/claude.f324", "lap/claude.f324"}, {"lap", "lap"}, {"@repo=alpha", "@repo=alpha"}} {
		if to, how, err := ResolveBare([]msg.RosterEntry{machine, live}, c.to, me, t0, LiveWithin, false); err != nil || to != c.want || how != "" {
			t.Errorf("%s: %q %q %v", c.to, to, how, err)
		}
	}
}

func TestLastReadIsWhenLabelsWereDeclared(t *testing.T) {
	e := seen("lap/claude.a", time.Minute, "repo=alpha")
	if got := LastRead(e); !got.Equal(e.Seen) {
		t.Errorf("no LabelsAt: last seen stands in: %v", got)
	}
	e = readAt(e, 3*time.Hour)
	if got := LastRead(e); !got.Equal(t0.Add(-3 * time.Hour)) {
		t.Errorf("LabelsAt is the last read: %v", got)
	}
}

func TestUnreadTargetIsAKnownSessionThatHasReadNothingForLong(t *testing.T) {
	me := "me/claude.0001"
	// It sent a minute ago (seen), but last read three hours ago: the case a long run of its own makes.
	busy := readAt(seen("lap/claude.busy", time.Minute, "repo=graph"), 3*time.Hour)
	inRepo := seen("air/codex.peer", 10*time.Minute, "repo=graph")
	onLap := seen("lap/claude.near", 5*time.Minute, "repo=gateway")
	roster := []msg.RosterEntry{busy, inRepo, onLap, seen(me, 0, "repo=graph"), seen("air/codex.gone", 3*time.Hour, "repo=graph")}

	u, unread := UnreadTarget(roster, "lap/claude.busy", me, t0, ReadWithin)
	if !unread {
		t.Fatal("read nothing for 3h, warned at 2h")
	}
	if u.Target != "lap/claude.busy" || u.Repo != "graph" || len(u.Live) != 1 || u.Live[0].Me != "air/codex.peer" {
		t.Errorf("who to try is the live sessions of its repo on any machine, never the sender: %+v", u)
	}
	if _, unread := UnreadTarget(roster, "LAP/claude.busy ", me, t0, ReadWithin); !unread {
		t.Error("a target is read as the send reads it")
	}

	for name, c := range map[string]struct {
		roster     []msg.RosterEntry
		to         string
		readWithin time.Duration
	}{
		"read an hour ago":         {[]msg.RosterEntry{readAt(busy, time.Hour)}, "lap/claude.busy", ReadWithin},
		"read exactly 2h ago":      {[]msg.RosterEntry{readAt(busy, ReadWithin)}, "lap/claude.busy", ReadWithin},
		"listening":                {[]msg.RosterEntry{listening(busy)}, "lap/claude.busy", ReadWithin},
		"turned off":               {roster, "lap/claude.busy", 0},
		"turned off by a negative": {roster, "lap/claude.busy", -time.Hour},
		"unknown to the nodes":     {roster, "lap/claude.nobody", ReadWithin},
		"a selector":               {roster, "@repo=graph", ReadWithin},
		"a machine":                {roster, "lap", ReadWithin},
		"a tool on a machine":      {roster, "lap/claude", ReadWithin},
		"the sender":               {[]msg.RosterEntry{readAt(seen(me, time.Minute), 5*time.Hour)}, me, ReadWithin},
		"within a wider limit":     {roster, "lap/claude.busy", 4 * time.Hour},
		"the person on a machine":  {[]msg.RosterEntry{readAt(seen("lap/human", time.Minute), 5*time.Hour)}, "lap/human", ReadWithin},
		"a join probe":             {[]msg.RosterEntry{readAt(seen("lap/"+msg.ProbeReaderAgent, time.Minute), 5*time.Hour)}, "lap/" + msg.ProbeReaderAgent, ReadWithin},
	} {
		if u, unread := UnreadTarget(c.roster, c.to, me, t0, c.readWithin); unread {
			t.Errorf("%s: warned: %+v", name, u)
		}
	}
	if _, unread := UnreadTarget([]msg.RosterEntry{readAt(busy, ReadWithin+time.Second)}, "lap/claude.busy", me, t0, ReadWithin); !unread {
		t.Error("a second past the limit is warned")
	}
	// No LabelsAt: last seen stands in, so a session seen 3h ago is warned, one seen now is not.
	if _, unread := UnreadTarget([]msg.RosterEntry{seen("lap/claude.x", 3*time.Hour)}, "lap/claude.x", me, t0, ReadWithin); !unread {
		t.Error("seen 3h ago, no labels: warned")
	}
	if _, unread := UnreadTarget([]msg.RosterEntry{seen("lap/claude.x", time.Minute)}, "lap/claude.x", me, t0, ReadWithin); unread {
		t.Error("seen a minute ago, no labels: not warned")
	}
	// No repo: who is live on its machine.
	norepo := readAt(seen("lap/claude.busy", time.Minute), 3*time.Hour)
	u, _ = UnreadTarget([]msg.RosterEntry{norepo, inRepo, onLap}, "lap/claude.busy", me, t0, ReadWithin)
	if u.Repo != "" || len(u.Live) != 1 || u.Live[0].Me != "lap/claude.near" {
		t.Errorf("no repo: %+v", u)
	}
}

func TestUnreadWarningSaysHowLongAndWhoToTryInstead(t *testing.T) {
	me := "me/claude.0001"
	busy := readAt(seen("lap/claude.busy", time.Minute, "repo=baditaflorin/go-fleet-graph"), 6*time.Hour)
	peer := seen("air/codex.peer", 10*time.Minute, "repo=baditaflorin/go-fleet-graph")
	peer.Title = "graph checkpoint"

	u, _ := UnreadTarget([]msg.RosterEntry{busy, peer}, "lap/claude.busy", me, t0, ReadWithin)
	w := u.Warning(t0)
	for _, want := range []string{"warning: lap/claude.busy has read nothing for 6h (last read ", "so it may not see this until its next prompt",
		"live in repo baditaflorin/go-fleet-graph now: air/codex.peer", "graph checkpoint", "(10m ago)"} {
		if !strings.Contains(w, want) {
			t.Errorf("live in repo: missing %q in %q", want, w)
		}
	}

	u, _ = UnreadTarget([]msg.RosterEntry{busy}, "lap/claude.busy", me, t0, ReadWithin)
	owner := u
	owner.Owner = "repo:baditaflorin/go-fleet-graph"
	if w := owner.Warning(t0); !strings.Contains(w, "nobody else in repo baditaflorin/go-fleet-graph is live, and its owner is recorded: `aivoicebus task owner:repo:baditaflorin/go-fleet-graph \"...\"`") {
		t.Errorf("owner: %q", w)
	}
	none := u
	none.NoOwner = true
	if w := none.Warning(t0); !strings.Contains(w, "no owner is recorded for it (`aivoicebus owner set repo:baditaflorin/go-fleet-graph` records one)") {
		t.Errorf("no owner: %q", w)
	}
	if w := u.Warning(t0); !strings.Contains(w, "nobody else in repo baditaflorin/go-fleet-graph was seen in the last 30m") || strings.Contains(w, "owner") {
		t.Errorf("owner unknown: %q", w)
	}

	norepo := readAt(seen("lap/claude.busy", time.Minute), 3*time.Hour)
	u, _ = UnreadTarget([]msg.RosterEntry{norepo, seen("lap/claude.near", 5*time.Minute)}, "lap/claude.busy", me, t0, ReadWithin)
	if w := u.Warning(t0); !strings.Contains(w, "has read nothing for 3h") || !strings.Contains(w, "live on lap now: lap/claude.near (5m ago)") {
		t.Errorf("no repo: %q", w)
	}
	u, _ = UnreadTarget([]msg.RosterEntry{norepo}, "lap/claude.busy", me, t0, ReadWithin)
	if w := u.Warning(t0); !strings.Contains(w, "nobody else on lap was seen in the last 30m") {
		t.Errorf("no repo, nobody live: %q", w)
	}
}

func TestFindOwnerLooksUpTheRepoOnlyWhenNobodyInItIsLive(t *testing.T) {
	var asked []string
	resolve := func(err error) func(string) (msg.OwnerResolution, error) {
		return func(subject string) (msg.OwnerResolution, error) {
			asked = append(asked, subject)
			return msg.OwnerResolution{Subject: subject}, err
		}
	}
	u := Unread{Repo: "Baditaflorin/Go-Fleet-Graph"}
	u.FindOwner(resolve(nil))
	if u.Owner != "repo:baditaflorin/go-fleet-graph" || u.NoOwner || len(asked) != 1 || asked[0] != "repo:baditaflorin/go-fleet-graph" {
		t.Errorf("found: %+v %v", u, asked)
	}
	u = Unread{Repo: "graph"}
	u.FindOwner(resolve(fmt.Errorf("%w for repo:graph: whoever answers for it records itself", msg.ErrNoOwner)))
	if u.Owner != "" || !u.NoOwner {
		t.Errorf("none recorded: %+v", u)
	}
	u = Unread{Repo: "graph"}
	u.FindOwner(resolve(fmt.Errorf("%w for repo:graph: a and b", msg.ErrOwnerConflict)))
	if u.Owner != "" || u.NoOwner {
		t.Errorf("two owners: nothing is said: %+v", u)
	}
	u.FindOwner(resolve(errors.New("nodes unreachable")))
	if u.Owner != "" || u.NoOwner {
		t.Errorf("unreachable: nothing is said: %+v", u)
	}
	asked = nil
	for _, u := range []Unread{{}, {Repo: "graph", Live: []msg.RosterEntry{seen("air/codex.peer", time.Minute)}}, {Repo: "has space"}} {
		u.FindOwner(resolve(nil))
		if u.Owner != "" || u.NoOwner {
			t.Errorf("%+v", u)
		}
	}
	if len(asked) != 0 {
		t.Errorf("looked up %v: no repo, someone live in it, or a repo no subject can name", asked)
	}
}

// A mesh where repo graph has two sessions that last read (and were seen) hours ago, one of them having
// sent since, and repo gateway one live session on lap.
func quietMesh(t *testing.T) func(Request) (Plan, string, error) {
	t.Helper()
	c, s := meshWith(t, "me/claude.0001", nil)
	s.SetClock(func() time.Time { return time.Now().Add(-5 * time.Hour) })
	s.InboxFor("air/codex.old", false, msg.ParseLabels("repo=graph"))
	s.SetClock(func() time.Time { return time.Now().Add(-3 * time.Hour) })
	s.InboxFor("lap/claude.busy", false, msg.ParseLabels("repo=graph"))
	s.SetClock(time.Now)
	s.InboxFor("lap/claude.near", false, msg.ParseLabels("repo=gateway"))
	prep := func(r Request) (Plan, string, error) {
		var warn strings.Builder
		p, err := Prepare(c, r, &warn)
		return p, warn.String(), err
	}
	return prep
}

func TestPrepareRefusesAQuietSelectorOnlyForAuthoredMessages(t *testing.T) {
	prep := quietMesh(t)
	_, _, err := prep(Request{To: "@repo=graph", Body: "hold the deploy: checkpoint running", Authored: true})
	if err == nil || !strings.Contains(err.Error(), "refused: none of the sessions @repo=graph matches is live") ||
		!strings.Contains(err.Error(), "lap/claude.busy (seen 3h ago)") || !strings.Contains(err.Error(), "live on lap now: lap/claude.near") {
		t.Fatalf("authored, nobody live in the repo: %v", err)
	}
	for name, r := range map[string]Request{
		"--queue":            {To: "@repo=graph", Body: "hold", Authored: true, Queue: true},
		"--force":            {To: "@repo=graph", Body: "hold", Authored: true, Force: true},
		"not authored":       {To: "@repo=graph", Body: "hold"},
		"someone live there": {To: "@repo=gateway", Body: "hold", Authored: true},
		"a machine":          {To: "air", Body: "hold", Authored: true},
	} {
		if _, _, err := prep(r); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Nobody in 24h is the older refusal, which only --queue passes.
	if _, _, err := prep(Request{To: "@repo=nothing", Body: "hold", Authored: true}); err == nil || !strings.Contains(err.Error(), "no session seen in the last 24h matches @repo=nothing") {
		t.Errorf("nobody at all: %v", err)
	}
}

func TestPrepareResolvesASessionNamedWithoutItsMachine(t *testing.T) {
	prep := quietMesh(t)
	p, _, err := prep(Request{To: "claude.near", Body: "is the gateway up", Authored: true})
	if err != nil || p.Msg.To != "lap/claude.near" || !strings.Contains(p.Alias, "claude.near -> lap/claude.near") || !strings.Contains(strings.Join(p.Notes, ";"), "the one live session of that name") {
		t.Fatalf("resolved: %v to=%q alias=%q notes=%v", err, p.Msg.To, p.Alias, p.Notes)
	}
	// Not only for authored messages: whatever names a session without its machine would lose it.
	if p, _, err := prep(Request{To: "claude.near", Body: "x"}); err != nil || p.Msg.To != "lap/claude.near" {
		t.Errorf("not authored: %v %q", err, p.Msg.To)
	}
	// The resolved session is one exact session again: --or-successor applies to it.
	if p, _, err := prep(Request{To: "claude.near", Body: "x", Authored: true, Successor: true, SuccessorAsked: true}); err != nil || !p.Successor {
		t.Errorf("successor: %v %+v", err, p.Successor)
	}
	if _, _, err := prep(Request{To: "claude.busy", Body: "x", Authored: true}); err == nil || !strings.Contains(err.Error(), "no session of that name is live") {
		t.Errorf("not live: %v", err)
	}
	// --queue keeps it for the one session of that name; the stale-session check then lets it through.
	if p, _, err := prep(Request{To: "claude.busy", Body: "x", Authored: true, Queue: true}); err != nil || p.Msg.To != "lap/claude.busy" {
		t.Errorf("--queue: %v %q", err, p.Msg.To)
	}
	if _, _, err := prep(Request{To: "codex.nobody1", Body: "x", Authored: true}); err == nil || !strings.Contains(err.Error(), "no session of that name has been seen") {
		t.Errorf("nobody: %v", err)
	}
	// An amendment keeps the audience of the message it corrects.
	if p, _, err := prep(Request{To: "claude.near", Body: "x", Queue: true, Amending: true}); err != nil || p.Msg.To != "claude.near" {
		t.Errorf("amending: %v %q", err, p.Msg.To)
	}
}

func TestAReplyToAMessageSentWithoutAMachineGoesToThatSession(t *testing.T) {
	c, s := meshWith(t, "me/claude.0001", nil)
	s.SetClock(func() time.Time { return time.Now().Add(-3 * time.Hour) })
	s.InboxFor("lap/claude.busy", false, msg.ParseLabels("repo=graph"))
	s.SetClock(time.Now)
	s.InboxFor("lap/claude.near", false, nil)
	for target, want := range map[string]string{"claude.near": "lap/claude.near", "claude.busy": "lap/claude.busy"} {
		// What an older client sent: to a "machine" of that name.
		lost, err := c.Send(msg.Message{ID: msg.NewID(), From: "me/claude.0001", To: target, Body: "are you there?"})
		if err != nil {
			t.Fatal(err)
		}
		var warn strings.Builder
		p, err := Prepare(c, Request{Re: lost.ID, Body: "following up", Authored: true, TargetSaid: true}, &warn)
		if err != nil || p.Msg.To != want {
			t.Errorf("%s: %v to=%q %s", target, err, p.Msg.To, warn.String())
		}
	}
}

func TestPrepareSaysSoWhenTheNodesCannotFindABareName(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_ME", "me/claude.0001")
	c := &client.Client{URLs: []string{srv.URL}, Token: "tok", NoSign: true}
	_, err := Prepare(c, Request{To: "claude.f324", Body: "x", Authored: true}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "the nodes could not say who is around to find it") || !strings.Contains(err.Error(), "HOST/claude.f324") {
		t.Errorf("%v", err)
	}
}

func TestAMachineThatHasTheNameIsThatMachine(t *testing.T) {
	c, s := meshWith(t, "me/claude.0001", nil)
	s.InboxFor("claude.f324/codex.ab12", false, nil)
	s.InboxFor("lap/claude.f324", false, nil)
	p, err := Prepare(c, Request{To: "claude.f324", Body: "x", Authored: true}, io.Discard)
	if err != nil || p.Msg.To != "claude.f324" || p.Alias != "" || slices.Contains(p.Notes, "") {
		t.Errorf("%v to=%q alias=%q notes=%q", err, p.Msg.To, p.Alias, p.Notes)
	}
}

func TestASuccessorNoteIsNotRepeatedAsAReadWarning(t *testing.T) {
	c, _ := staleMesh(t) // mac14/claude.dead read 2h ago, in repo alpha
	var warn strings.Builder
	p, err := Prepare(c, Request{To: "mac14/claude.dead", Body: "x", Authored: true, Successor: true, SuccessorAsked: true, ReadWithin: time.Hour}, &warn)
	if err != nil || !strings.Contains(strings.Join(p.Notes, ";"), "goes to the session that replaces it") || strings.Contains(warn.String(), "read nothing") {
		t.Errorf("%v notes=%v warn=%q", err, p.Notes, warn.String())
	}
}

func TestPrepareWarnsWhenTheSessionNamedHasReadNothingForHours(t *testing.T) {
	c, s := meshWith(t, "me/claude.0001", nil)
	s.SetClock(func() time.Time { return time.Now().Add(-3 * time.Hour) })
	s.InboxFor("lap/claude.busy", false, msg.ParseLabels("repo=graph"))
	s.SetClock(time.Now)
	s.InboxFor("air/codex.peer", false, msg.ParseLabels("repo=graph"))
	// It has sent since, so the nodes have seen it a moment ago: the 30m stale check passes it.
	if _, err := c.Send(msg.Message{ID: msg.NewID(), From: "lap/claude.busy", To: "air/codex.peer", Body: "progress: step 4 of 9"}); err != nil {
		t.Fatal(err)
	}
	var warn strings.Builder
	p, err := Prepare(c, Request{To: "lap/claude.busy", Body: "please pause before the deploy", Authored: true, ReadWithin: ReadWithin}, &warn)
	if err != nil || p.Msg.To != "lap/claude.busy" {
		t.Fatalf("it still sends: %v", err)
	}
	if w := warn.String(); !strings.Contains(w, "warning: lap/claude.busy has read nothing for 3h") || !strings.Contains(w, "live in repo graph now: air/codex.peer") {
		t.Errorf("warning: %q", w)
	}
	warn.Reset()
	if _, err := Prepare(c, Request{To: "lap/claude.busy", Body: "x", Authored: true}, &warn); err != nil || strings.Contains(warn.String(), "read nothing") {
		t.Errorf("ReadWithin 0 is off: %v %q", err, warn.String())
	}
	warn.Reset()
	if _, err := Prepare(c, Request{To: "lap/claude.busy", Body: "x", Authored: true, ReadWithin: 4 * time.Hour}, &warn); err != nil || strings.Contains(warn.String(), "read nothing") {
		t.Errorf("a wider limit: %v %q", err, warn.String())
	}
	// A test message has its own check, and is not warned.
	warn.Reset()
	_, _ = Prepare(c, Request{To: "lap/claude.busy", Body: "x", Authored: true, ReadWithin: ReadWithin, Test: true}, &warn)
	if strings.Contains(warn.String(), "read nothing") {
		t.Errorf("test lane: %q", warn.String())
	}
}

func TestAReplyToAGoneAskerIsWarnedOnceNotTwice(t *testing.T) {
	c, s := staleMesh(t)
	s.SetClock(func() time.Time { return time.Now().Add(-3 * time.Hour) })
	orig, err := c.Send(msg.Message{ID: msg.NewID(), From: "mac14/claude.dead", To: "me/claude.0001", Body: "can you check?"})
	s.SetClock(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var warn strings.Builder
	if _, err := Prepare(c, Request{Re: orig.ID, Body: "checked", Authored: true, TargetSaid: true, ReadWithin: ReadWithin}, &warn); err != nil {
		t.Fatal(err)
	}
	if w := warn.String(); !strings.Contains(w, "warning: the reply goes to mac14/claude.dead") || strings.Contains(w, "read nothing") {
		t.Errorf("the stale-reply warning says it; the read warning is not added: %q", w)
	}
	// --queue to the same session named: no refusal, and the read warning says how long.
	warn.Reset()
	if _, err := Prepare(c, Request{To: "mac14/claude.dead", Body: "checked", Authored: true, Queue: true, ReadWithin: ReadWithin}, &warn); err != nil || !strings.Contains(warn.String(), "mac14/claude.dead has read nothing for 2h") {
		t.Errorf("--queue: %v %q", err, warn.String())
	}
}
