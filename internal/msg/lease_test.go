package msg

import (
	"testing"
	"time"
)

func TestLeaseHoldersAndKeys(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	live := []Lease{
		{Key: "hub:server.go", By: "b/codex.2", ID: "b1", At: t0.Add(time.Second), Until: t0.Add(time.Hour)},
		{Key: "hub:server.go", By: "a/claude.1", ID: "a1", At: t0, Until: t0.Add(30 * time.Minute)},
		{Key: "hub:server.go", By: "a/claude.1", ID: "a2", At: t0.Add(20 * time.Minute), Until: t0.Add(50 * time.Minute)}, // a renewal
		{Key: "deploy:prod", By: "c/gemini.3", ID: "c1", At: t0, Until: t0.Add(time.Hour)},
	}
	hs := Holders(live)
	if len(hs) != 2 || hs[0].Key != "deploy:prod" || hs[1].By != "a/claude.1" || !hs[1].Until.Equal(t0.Add(50*time.Minute)) {
		t.Errorf("holders: %+v", hs)
	}
	for k, want := range map[string]bool{"hub:server.go": true, "repo/branch": true, "a b": false, "": false, "x\\ny": false, "x;rm": false} {
		if ValidLeaseKey(k) != want {
			t.Errorf("ValidLeaseKey(%q)", k)
		}
	}
	exp := t0.Add(time.Hour)
	if l, ok := LeaseOf(Message{Kind: KindLease, Body: "hub:x\nediting", From: "a/b", ID: "1", Exp: &exp}); !ok || l.Key != "hub:x" {
		t.Errorf("LeaseOf: %+v %v", l, ok)
	}
	if err := (Message{Kind: KindLease}).ValidateOptions(); err == nil {
		t.Error("a lease without a ttl accepted")
	}
}

// A send moves a session's Seen on every node the message reaches, while those nodes may hold
// older labels: the merge takes the labels declared last, not those of the latest sighting.
func TestMergeRostersJudgesLabelsByWhenTheyWereDeclared(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	fresh := RosterEntry{Me: "lap/claude.a1", Seen: t0.Add(time.Minute), LabelsAt: t0.Add(time.Minute), Labels: Labels{"rooms": "v07"}}
	stale := RosterEntry{Me: "lap/claude.a1", Seen: t0.Add(2 * time.Minute), LabelsAt: t0, Labels: Labels{"tool": "claude"}}
	for _, order := range [][]RosterEntry{{fresh, stale}, {stale, fresh}} {
		m := MergeRosters(order[:1], order[1:])
		if len(m) != 1 || m[0].Labels["rooms"] != "v07" || !m[0].Seen.Equal(t0.Add(2*time.Minute)) {
			t.Errorf("%+v", m)
		}
	}
}

func TestShortTargetShowsTermsAsWritten(t *testing.T) {
	for in, want := range map[string]string{
		"@rooms=v07|v07+*|*+v07|*+v07+*":       "@room=v07",
		"@repo=x,caps=run|run+*|*+run|*+run+*": "@repo=x,can=run",
		"@caps!=gpu|gpu+*|*+gpu|*+gpu+*":       "@can!=gpu",
		"lap/claude.a1":                        "lap/claude.a1",
		"@rooms=odd|pattern":                   "@rooms=odd|pattern",
	} {
		if got := ShortTarget(in); got != want {
			t.Errorf("ShortTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchKeyGlobs(t *testing.T) {
	for _, c := range []struct {
		pattern, key string
		want         bool
	}{
		{"load:prod/*", "load:prod/scan-a", true},
		{"load:prod/*", "load:prod/", true}, // * may be empty
		{"load:prod/*", "load:prod", false},
		{"load:prod/*", "load:staging/scan-a", false},
		{"load:*", "load:prod/scan-a", true}, // * crosses a / as it crosses a :
		{"*", "deploy:prod:x", true},
		{"*:prod:*", "deploy:prod:x", true},
		{"deploy:prod:?", "deploy:prod:x", true},
		{"deploy:prod:?", "deploy:prod:xy", false},
		{"deploy:prod:?", "deploy:prod:", false},
		{"deploy:prod:x", "deploy:prod:x", true}, // no wildcard: the key itself
		{"deploy:prod:x", "deploy:prod:xy", false},
		{"deploy:prod:x", "deploy:prod", false},
		{"a*b*c", "aXbXbXc", true}, // needs the first * to give back what it took
		{"a*b*c", "aXbXbX", false},
		{"*b", "abab", true},
		{"**", "", true},
		{"", "", true},
		{"", "x", false},
		{"x", "", false},
		{"hub:server.go", "hub:serverXgo", false}, // . is a dot, not "any character"
	} {
		if got := MatchKey(c.pattern, c.key); got != c.want {
			t.Errorf("MatchKey(%q, %q) = %v, want %v", c.pattern, c.key, got, c.want)
		}
	}
}
