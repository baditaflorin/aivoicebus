package tasks

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func seen(me string, ago time.Duration, listening bool) msg.RosterEntry {
	return msg.RosterEntry{Me: me, Seen: now.Add(-ago), Listening: listening}
}

func pickOf(t *testing.T, roster []msg.RosterEntry, target string, o PickOpts) (Pick, error) {
	t.Helper()
	o.Now, o.Me = now, "desk/claude.me"
	return PickSession(roster, target, o)
}

// A session listening now is asked before one seen more recently that is not listening.
func TestAskPicksTheListeningSessionFirst(t *testing.T) {
	roster := []msg.RosterEntry{seen("lap/claude.a1", time.Second, false), seen("lap/codex.b2", 5*time.Minute, true), seen("box/codex.c3", 0, true)}
	p, err := pickOf(t, roster, "lap", PickOpts{})
	if err != nil || p.To != "lap/codex.b2" || !p.Listening || !strings.Contains(p.Why, "listening now") {
		t.Fatalf("%+v %v", p, err)
	}
}

// With nobody listening, the session seen most recently is asked, and the reason says so.
func TestAskPicksTheMostRecentWhenNobodyListens(t *testing.T) {
	roster := []msg.RosterEntry{seen("lap/claude.a1", 10*time.Minute, false), seen("lap/codex.b2", time.Minute, false), seen("lap/codex.old", 2*time.Hour, false)}
	p, err := pickOf(t, roster, "lap", PickOpts{})
	if err != nil || p.To != "lap/codex.b2" || p.Listening || !strings.Contains(p.Why, "seen most recently (1m0s ago)") {
		t.Fatalf("%+v %v", p, err)
	}
	// The tool narrows it; a session seen too long ago, or this session, is nobody.
	if p, err := pickOf(t, roster, "lap/claude", PickOpts{}); err != nil || p.To != "lap/claude.a1" {
		t.Errorf("tool: %+v %v", p, err)
	}
	if _, err := pickOf(t, roster, "lap/claude", PickOpts{Within: 5 * time.Minute}); !errors.Is(err, ErrNobody) {
		t.Errorf("stale: %v", err)
	}
	if _, err := pickOf(t, []msg.RosterEntry{seen("desk/claude.me", 0, true)}, "desk", PickOpts{}); !errors.Is(err, ErrNobody) {
		t.Errorf("itself: %v", err)
	}
}

// Two sessions nothing tells apart (both listening, or seen within AskAlike of each other) are
// refused and listed in a fixed order, unless --pick, a session id or --any says which.
func TestAskRefusesToGuessBetweenLookAlikes(t *testing.T) {
	both := []msg.RosterEntry{seen("m1/claude.zz", time.Second, true), seen("m1/claude.aa", time.Minute, true), seen("m1/codex.cc", 0, false)}
	p, err := pickOf(t, both, "m1", PickOpts{})
	if !errors.Is(err, ErrAlike) || len(p.Alike) != 2 || p.Alike[0].Me != "m1/claude.aa" || p.Alike[1].Me != "m1/claude.zz" {
		t.Fatalf("listening alike: %+v %v", p, err)
	}
	if p, err := pickOf(t, both, "m1", PickOpts{Pick: 2}); err != nil || p.To != "m1/claude.zz" || !strings.Contains(p.Why, "--pick 2 of 2") {
		t.Errorf("--pick: %+v %v", p, err)
	}
	if _, err := pickOf(t, both, "m1", PickOpts{Pick: 3}); err == nil || !strings.Contains(err.Error(), "2 to pick from") {
		t.Errorf("--pick out of range: %v", err)
	}
	if p, err := pickOf(t, both, "m1", PickOpts{Any: true, Rand: func(int) int { return 1 }}); err != nil || p.To != "m1/claude.zz" || !strings.Contains(p.Why, "picked from 2 listening") {
		t.Errorf("--any: %+v %v", p, err)
	}
	if p, err := pickOf(t, both, "m1/claude.zz", PickOpts{}); err != nil || p.To != "m1/claude.zz" || !strings.Contains(p.Why, "named") {
		t.Errorf("a session id: %+v %v", p, err)
	}
	// Seen 30s apart, none listening: alike. Seen 10m apart: the recent one.
	recent := []msg.RosterEntry{seen("m1/claude.aa", 10*time.Second, false), seen("m1/claude.bb", 40*time.Second, false)}
	if _, err := pickOf(t, recent, "m1", PickOpts{}); !errors.Is(err, ErrAlike) {
		t.Errorf("seen together: %v", err)
	}
	if p, err := pickOf(t, recent, "m1", PickOpts{Any: true}); err != nil || p.To != "m1/claude.aa" {
		t.Errorf("--any takes the latest: %+v %v", p, err)
	}
	recent[1].Seen = now.Add(-10 * time.Minute)
	if p, err := pickOf(t, recent, "m1", PickOpts{}); err != nil || p.To != "m1/claude.aa" {
		t.Errorf("seen apart: %+v %v", p, err)
	}
}

// A session id is asked as named while it is live; a stale one is nobody, and --pick has nothing to choose.
func TestAskANamedSession(t *testing.T) {
	roster := []msg.RosterEntry{seen("m1/codex.aa", time.Hour, false), seen("m1/codex.bb", 3*time.Minute, false)}
	if _, err := pickOf(t, roster, "m1/codex.aa", PickOpts{}); !errors.Is(err, ErrNobody) {
		t.Errorf("stale: %v", err)
	}
	if p, err := pickOf(t, roster, "m1/codex.bb", PickOpts{}); err != nil || !strings.Contains(p.Why, "seen 3m0s ago") {
		t.Errorf("live: %+v %v", p, err)
	}
	if _, err := pickOf(t, roster, "m1/codex.bb", PickOpts{Pick: 1}); err == nil {
		t.Error("--pick with a session id")
	}
	for to, want := range map[string]bool{"m1/codex.bb": true, "m1/codex": false, "m1": false, "*/codex.bb": false, "@repo=x": false, "m1/*": false} {
		if IsSession(to) != want {
			t.Errorf("IsSession(%q) = %v", to, !want)
		}
	}
}
