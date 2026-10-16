package msg

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMatches(t *testing.T) {
	cases := []struct {
		to, me string
		want   bool
	}{
		{"all", "imac/claude", true},
		{"imac", "imac/codex", true},
		{"imac", "air/codex", false},
		{"imac/claude", "imac/claude", true},
		{"imac/claude", "imac/codex", false},
		{"*/codex", "air/codex", true},
		{"*/codex", "air/claude", false},
		{"imac/*", "imac/x", true},
		// several sessions of one tool on one machine
		{"imac/claude", "imac/claude.a1b2", true},
		{"imac/claude.a1b2", "imac/claude.a1b2", true},
		{"imac/claude.a1b2", "imac/claude.c3d4", false},
		{"imac/claude.a1b2", "imac/claude", false},
		{"*/claude", "air/claude.zz99", true},
		{"*/codex", "air/claude.zz99", false},
		{"imac/claude", "imac/claudex.a1b2", false},
		{"imac", "imac/codex.q1w2", true},
		{"imac/claude", "air/claude.a1b2", false},
	}
	for _, c := range cases {
		if got := Matches(c.to, c.me); got != c.want {
			t.Errorf("Matches(%q,%q)=%v", c.to, c.me, got)
		}
	}
}

func TestIDs(t *testing.T) {
	id := NewID()
	if !ValidID(id) {
		t.Fatalf("NewID() %q is not valid", id)
	}
	for _, bad := range []string{"", "abc", "zzzzzzzzzz", id + "0"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) should be false", bad)
		}
	}
}

func TestMatchesLabeledSelectorsAndPlainTargetsTogether(t *testing.T) {
	l := Labels{"repo": "go-url-categorizer-api", "branch": "feature/x", "tool": "claude", "machine": "laptop-a"}
	cases := []struct {
		to   string
		want bool
	}{
		{"@repo=go-url-categorizer-api", true},
		{"@repo=other", false},
		{"@repo=go-url-categorizer-api,tool=claude", true},
		{"@repo=go-url-categorizer-api,tool=codex", false}, // every term must hold
		{"@repo=go-*", true},
		{"@branch=feature/*", true}, // * crosses '/'
		{"@branch=hotfix/*", false},
		{"@role=reviewer", false}, // a label the session does not declare
		{"@REPO=GO-URL-CATEGORIZER-API", true},
		{"@repo=", false}, // malformed selectors match nothing
		{"@", false},
		{"@repo", false},
		{"@bad key=x", false},
		{"all", true},
		{"laptop-a/claude.a1b2", true}, // plain targets still work with labels
		{"imac/claude", false},
	}
	for _, c := range cases {
		if got := MatchesLabeled(c.to, "laptop-a/claude.a1b2", l); got != c.want {
			t.Errorf("MatchesLabeled(%q) = %v, want %v", c.to, got, c.want)
		}
	}
	if Matches("@repo=x", "a/b") {
		t.Error("a session that declares no labels must never match a selector")
	}
}

func TestParseSelectorRejectsWithAReason(t *testing.T) {
	for in, want := range map[string]string{
		"repo=x":                           "starts with @",
		"@":                                "at least one",
		"@repo":                            "key=value",
		"@repo=":                           "key=value",
		"@Re po=x":                         "not valid",
		"@1repo=x":                         "not valid",
		"@repo=a b":                        "may only use",
		"@repo=a;rm":                       "may only use",
		"@repo=a||b":                       "may only use",
		"@repo=a|":                         "may only use",
		"@repo=1|2|3|4|5|6|7|8|9":          "more than 8 alternatives",
		"@repo=!x":                         "may only use",
		"@repo!=":                          "key=value",
		"@a=b=c":                           "may only use",
		"@repo=" + strings.Repeat("a", 65): "at most 64",
		"@a=1,b=2,c=3,d=4,e=5,f=6,g=7,h=8,i=9,j=10,k=11,l=12,m=13": "at most 12",
	} {
		if _, err := ParseSelector(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseSelector(%q): want an error containing %q, got %v", in, want, err)
		}
	}
	sel, err := ParseSelector("@Repo=Foo-*, tool=claude")
	if err != nil || sel.String() != "@repo=foo-*,tool=claude" {
		t.Errorf("normalisation: %v %v", sel, err)
	}
}

func TestGlob(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"abc", "abc", true}, {"abc", "abd", false}, {"abc", "ab", false}, {"abc", "abcd", false},
		{"*", "", true}, {"*", "anything/at/all", true}, {"a*", "a", true}, {"a*", "abc", true}, {"a*", "b", false},
		{"*c", "abc", true}, {"*c", "abd", false}, {"a*c", "abbbc", true}, {"a*c", "ac", true}, {"a*c", "abbbd", false},
		{"*a*b*", "xxaxxbxx", true}, {"*a*b*", "xxbxxaxx", false}, {"**", "x", true}, {"a**b", "ab", true},
		{"feature/*", "feature/x/y", true}, {"", "", true}, {"", "a", false},
	} {
		if got := glob(c.pattern, c.s); got != c.want {
			t.Errorf("glob(%q,%q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestGlobIsLinearOnHostileInput(t *testing.T) {
	// the classic exponential-backtracking shape must stay fast
	start := time.Now()
	glob(strings.Repeat("*a", 30)+"b", strings.Repeat("a", 5000))
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("glob took %v on hostile input", d)
	}
}

func TestLabelsParseCleanAndFormat(t *testing.T) {
	l := ParseLabels(" Repo = My_Repo ,branch=Feature/Foo Bar,bad key=x,empty=,noequals,tool=claude,REPO=dup")
	want := Labels{"repo": "dup", "branch": "feature/foo-bar", "tool": "claude"} // later pair wins; junk dropped
	if l.String() != want.String() {
		t.Errorf("ParseLabels = %q, want %q", l.String(), want.String())
	}
	if (Labels{}).String() != "" || Labels(nil).String() != "" {
		t.Error("no labels format as the empty string")
	}
	if got := ParseLabels(l.String()); got.String() != l.String() {
		t.Errorf("round trip: %q -> %q", l.String(), got.String())
	}
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, fmt.Sprintf("k%02d=v", i))
	}
	if got := ParseLabels(strings.Join(many, ",")); len(got) != MaxLabels {
		t.Errorf("kept %d labels, want the cap of %d", len(got), MaxLabels)
	}
	if got := CleanValue(strings.Repeat("a", 200)); len(got) != MaxLabelValue {
		t.Errorf("CleanValue length %d", len(got))
	}
	for in, want := range map[string]string{"Feature/Ok": "feature/ok", "a b#c": "a-b-c", "--x--": "x", "": "", "ünï": "n"} {
		if got := CleanValue(in); got != want {
			t.Errorf("CleanValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOverlayLetsTheOverridesWin(t *testing.T) {
	got := Labels{"repo": "a", "tool": "claude"}.Overlay(Labels{"repo": "b", "role": "x"})
	if got.String() != "repo=b,role=x,tool=claude" {
		t.Errorf("%q", got.String())
	}
}

func TestPriorityDefaultsToAsk(t *testing.T) {
	for in, want := range map[string]string{"": "ask", "ask": "ask", "fyi": "fyi", "urgent": "urgent"} {
		if got := (Message{Prio: in}).Priority(); got != want {
			t.Errorf("Priority(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Second), now.Add(time.Second)
	if (Message{}).Expired(now) {
		t.Error("a message without an expiry never expires")
	}
	if !(Message{Exp: &past}).Expired(now) || (Message{Exp: &future}).Expired(now) || (Message{Exp: &now}).Expired(now) {
		t.Error("expired exactly when now is after Exp")
	}
}

func TestValidateOptions(t *testing.T) {
	good := []Message{{}, {Prio: "fyi"}, {Prio: "ask"}, {Prio: "urgent"}, {TTL: 60}, {TTL: int64(MaxTTL / time.Second)}, {Prio: "urgent", TTL: 3600}}
	for _, m := range good {
		if err := m.ValidateOptions(); err != nil {
			t.Errorf("%+v rejected: %v", m, err)
		}
	}
	bad := map[string]Message{
		"unknown priority":  {Prio: "critical"},
		"upper-case":        {Prio: "URGENT"},
		"ttl below minimum": {TTL: 59},
		"negative ttl":      {TTL: -5},
		"ttl above maximum": {TTL: int64(MaxTTL/time.Second) + 1},
	}
	for name, m := range bad {
		if m.ValidateOptions() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseTTL(t *testing.T) {
	for in, want := range map[string]time.Duration{"90m": 90 * time.Minute, "2h": 2 * time.Hour, "1m": time.Minute, "3d": 72 * time.Hour, " 1d ": 24 * time.Hour, "30d": 30 * 24 * time.Hour, "1h30m": 90 * time.Minute} {
		if got, err := ParseTTL(in); err != nil || got != want {
			t.Errorf("ParseTTL(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "soon", "0d", "-1d", "1.5d", "30s", "31d", "9999h", "d", "xd"} {
		if _, err := ParseTTL(in); err == nil {
			t.Errorf("ParseTTL(%q) accepted", in)
		}
	}
}

func TestParseSpan(t *testing.T) {
	for in, want := range map[string]time.Duration{"90m": 90 * time.Minute, "24h": 24 * time.Hour, "3d": 72 * time.Hour, " 7d ": 7 * 24 * time.Hour, "1s": time.Second, "3000d": 3000 * 24 * time.Hour} {
		if got, err := ParseSpan(in); err != nil || got != want {
			t.Errorf("ParseSpan(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "soon", "0", "0d", "-1h", "-2d", "1.5d", "d", "xd", "5"} {
		if _, err := ParseSpan(in); err == nil {
			t.Errorf("ParseSpan(%q) accepted", in)
		}
	}
	// the TTL wrapper keeps its bounds and its wording
	if _, err := ParseTTL("soon"); err == nil || !strings.Contains(err.Error(), `ttl "soon" is not a duration`) {
		t.Errorf("ParseTTL wording: %v", err)
	}
}

func TestIsBulletin(t *testing.T) {
	for to, want := range map[string]bool{
		"all":                   true,
		"@repo=x":               true,
		"@repo=x,branch=feat/*": true,
		"imac":                  true, // a whole host
		"imac/claude":           true, // every session of that tool there
		"*/claude":              true,
		"imac/*":                true,
		"imac/claude.a1b2":      false, // one session, one reader
		"laptop-a/codex.q":      false,
	} {
		if got := IsBulletin(to); got != want {
			t.Errorf("IsBulletin(%q) = %v, want %v", to, got, want)
		}
	}
}

func TestBulletinsExpireByDefaultAndOneSessionMessagesDoNot(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	exp := at.Add(10 * 24 * time.Hour)
	cases := []struct {
		name      string
		m         Message
		now       time.Time
		wantExpd  bool
		wantKnown bool // ExpiresAt reports an expiry at all
	}{
		{"fyi bulletin, after a day", Message{To: "all", Prio: PrioFYI, At: at}, at.Add(25 * time.Hour), true, true},
		{"fyi bulletin, within a day", Message{To: "all", Prio: PrioFYI, At: at}, at.Add(23 * time.Hour), false, true},
		{"ask bulletin lives three days", Message{To: "@repo=x", At: at}, at.Add(71 * time.Hour), false, true},
		{"ask bulletin, after three days", Message{To: "@repo=x", At: at}, at.Add(73 * time.Hour), true, true},
		{"urgent counts as three days", Message{To: "imac", Prio: PrioUrgent, At: at}, at.Add(73 * time.Hour), true, true},
		{"one session never by default", Message{To: "imac/claude.a1b2", At: at}, at.Add(400 * time.Hour), false, false},
		{"the sender's expiry wins, later", Message{To: "all", At: at, Exp: &exp}, at.Add(9 * 24 * time.Hour), false, true},
		{"the sender's expiry wins, earlier", Message{To: "imac/claude.a1b2", At: at, Exp: &exp}, at.Add(11 * 24 * time.Hour), true, true},
		{"no timestamp, no default expiry", Message{To: "all"}, at.Add(400 * time.Hour), false, false},
	}
	for _, c := range cases {
		if got := c.m.Expired(c.now); got != c.wantExpd {
			t.Errorf("%s: Expired = %v, want %v", c.name, got, c.wantExpd)
		}
		if _, known := c.m.ExpiresAt(); known != c.wantKnown {
			t.Errorf("%s: ExpiresAt known = %v, want %v", c.name, known, c.wantKnown)
		}
	}
}

func TestMergeRostersTakesTheLatestSightingAndTheLatestLabels(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	a := []RosterEntry{
		{Me: "mac/claude.a", Seen: t0, Labels: Labels{"repo": "x"}},
		{Me: "air/codex", Seen: t0.Add(time.Hour)}, // known to this node only from a message it sent
	}
	b := []RosterEntry{
		{Me: "mac/claude.a", Seen: t0.Add(time.Minute), Labels: Labels{"repo": "y"}},
		{Me: "air/codex", Seen: t0, Labels: Labels{"repo": "z"}},
		{Me: "imac/claude.b", Seen: t0},
	}
	got := MergeRosters(a, nil, b)
	if len(got) != 3 || got[0].Me != "air/codex" || got[1].Me != "imac/claude.b" || got[2].Me != "mac/claude.a" {
		t.Fatalf("one entry per session, sorted: %+v", got)
	}
	if !got[2].Seen.Equal(t0.Add(time.Minute)) || got[2].Labels["repo"] != "y" {
		t.Errorf("latest report wins: %+v", got[2])
	}
	if !got[0].Seen.Equal(t0.Add(time.Hour)) || got[0].Labels["repo"] != "z" {
		t.Errorf("seen from the node that saw it last, labels from the one that had some: %+v", got[0])
	}
	if MergeRosters() == nil || len(MergeRosters()) != 0 {
		t.Error("an empty merge is an empty, non-nil list")
	}
}

func TestValidVia(t *testing.T) {
	for _, via := range []string{"", ViaHook, ViaHookCount, ViaInbox, ViaCLI} {
		if !ValidVia(via) {
			t.Errorf("%q should be valid", via)
		}
	}
	for _, via := range []string{"HOOK", "web", "hook ", "x\ny"} {
		if ValidVia(via) {
			t.Errorf("%q should be refused", via)
		}
	}
}

func TestSelectorsCanOfferAlternativesAndExcludeThings(t *testing.T) {
	l := Labels{"repo": "api", "tool": "claude", "branch": "feature/x"}
	for _, c := range []struct {
		to   string
		want bool
	}{
		{"@repo=api|web", true},
		{"@repo=web|api", true},
		{"@repo=web|docs", false},
		{"@repo=a*|web", true},
		{"@repo!=web", true},                    // not that repo
		{"@repo!=api", false},                   // that very repo
		{"@repo!=api|web", false},               // any of the excluded
		{"@role!=reviewer", true},               // a label nobody declared is not the excluded value
		{"@tool=claude,repo!=web", true},        // positive and negative together
		{"@tool=codex|gemini,repo!=web", false}, // the positive part fails
		{"@branch!=feature/*", false},           // globs work in negations
		{"@branch=hotfix/*|feature/*", true},    // and across alternatives
		{"@repo=api,repo!=api", false},          // a contradiction matches nothing
		{"@REPO!=Web", true},                    // case-insensitive like everything else
	} {
		if got := MatchesLabeled(c.to, "m/claude.1", l); got != c.want {
			t.Errorf("MatchesLabeled(%q) = %v, want %v", c.to, got, c.want)
		}
	}
	if MatchesLabeled("@repo!=web", "m/claude.1", nil) != true {
		t.Error("a session without labels is 'not in repo web' too: negations alone reach it, so senders should pair them with a positive term")
	}
}

func TestSelectorStringRoundTripsAndFlagsTheNewForms(t *testing.T) {
	for in, adv := range map[string]bool{
		"@repo=api":              false,
		"@repo=api,tool=claude":  false,
		"@repo=api|web":          true,
		"@repo!=web":             true,
		"@tool=claude,repo!=a|b": true,
		"@branch=feature/*":      false,
	} {
		sel, err := ParseSelector(in)
		if err != nil || sel.String() != in || sel.Advanced() != adv {
			t.Errorf("%q: %v %v advanced=%v", in, sel, err, sel.Advanced())
		}
	}
}

func TestHereMeansTheSendersMachineAndNothingElse(t *testing.T) {
	own := Labels{"machine": "laptop-a", "tool": "claude"}
	for in, want := range map[string]string{
		"@here":             "@machine=laptop-a",
		"@here,tool=claude": "@machine=laptop-a,tool=claude",
		"@here,repo!=x":     "@machine=laptop-a,repo!=x",
		"@hereford=1":       "@hereford=1", // only the whole word
		"@repo=here":        "@repo=here",
		"all":               "all",
		"mac/claude":        "mac/claude",
	} {
		got, err := ExpandHere(in, own)
		if err != nil || got != want {
			t.Errorf("ExpandHere(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ExpandHere("@here", Labels{}); err == nil {
		t.Error("without a machine label there is no 'here'")
	}
}

func TestKindAndReachAreValidatedAndAnAnswerNamesItsQuestion(t *testing.T) {
	two := 2
	huge := MaxReach + 1
	for _, c := range []struct {
		m    Message
		want string // "" = valid
	}{
		{Message{Kind: KindQuestion}, ""},
		{Message{Kind: KindAnswer, Re: "0123456789"}, ""},
		{Message{Kind: KindAnswer}, "must say which message"},
		{Message{Kind: "shout"}, "not one of question, answer"},
		{Message{Reach: &two}, ""},
		{Message{Reach: &huge}, "out of range"},
	} {
		err := c.m.ValidateOptions()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v: %v, want %q", c.m, err, c.want)
		}
	}
}

func TestGroupsExpandAtSendTimeAndBadOnesAreSkipped(t *testing.T) {
	groups := ParseGroups("# my teams\ninfra = @repo=go-*|ops  # trailing note\nreviewers=@role=reviewer,tool=claude\nbroken=@repo\nBad Name=@repo=x\nnot a line\nopen=repo=x\n")
	if len(groups) != 2 || groups["infra"] != "@repo=go-*|ops" || groups["reviewers"] != "@role=reviewer,tool=claude" {
		t.Fatalf("%v", groups)
	}
	for in, want := range map[string]string{
		"@+infra":             "@repo=go-*|ops",
		"@+infra,tool=claude": "@repo=go-*|ops,tool=claude",
		"@+INFRA":             "@repo=go-*|ops",
		"@repo=x":             "@repo=x",
		"all":                 "all",
	} {
		if got, err := ExpandGroup(in, groups); err != nil || got != want {
			t.Errorf("ExpandGroup(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ExpandGroup("@+nobody", groups); err == nil || !strings.Contains(err.Error(), `no group named "nobody"`) {
		t.Errorf("%v", err)
	}
	if _, err := ExpandGroup("@+x", map[string]string{"x": "not a selector"}); err == nil {
		t.Error("a saved group that is not a selector is reported, not sent")
	}
}

func TestParseSpanRefusesADayCountThatWrapsAround(t *testing.T) {
	for _, in := range []string{"106752d", "9999999999999d", "9223372036854775807d", "0d", "-1d"} {
		if got, err := ParseSpan(in); err == nil {
			t.Errorf("ParseSpan(%q) = %v, want an error", in, got)
		}
	}
	if got, err := ParseSpan("106751d"); err != nil || got <= 0 {
		t.Errorf("the largest whole day count still works: %v %v", got, err)
	}
}
