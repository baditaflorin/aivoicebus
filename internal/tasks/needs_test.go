package tasks

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestPickNeedsPrefersListeningThenTheMostRecent(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	common := msg.Labels{"repo": "go-common", "origin": "baditaflorin/go-common", "tool": "claude"}
	web := msg.Labels{"repo": "web", "origin": "baditaflorin/web", "tool": "claude"}
	roster := []msg.RosterEntry{
		{Me: "a/claude.web1", Seen: now.Add(-1 * time.Second), Labels: web, Listening: true},   // listening, wrong repo
		{Me: "b/claude.old1", Seen: now.Add(-20 * time.Minute), Labels: common},                // has it, seen earlier
		{Me: "c/claude.new1", Seen: now.Add(-3 * time.Minute), Labels: common},                 // has it, seen more recently
		{Me: "d/claude.gone", Seen: now.Add(-5 * time.Hour), Labels: common, Listening: false}, // has it, not live
		{Me: "me/claude.self", Seen: now, Labels: common, Listening: true},                     // the sender
	}
	o := PickOpts{Me: "me/claude.self", Now: now}
	needs := []string{"repo:baditaflorin/go-common"}

	p, err := PickNeeds(roster, "all", needs, o)
	if err != nil || p.To != "c/claude.new1" || p.Listening {
		t.Fatalf("none of those listening: the most recently seen one, got %+v %v", p, err)
	}
	if !strings.Contains(p.Why, "has repo:baditaflorin/go-common") || !strings.Contains(p.Why, "seen most recently (3m0s ago) (of 2 that have it)") {
		t.Errorf("why: %s", p.Why)
	}

	roster[1].Listening = true // the older one is waiting now: listening beats recency
	p, err = PickNeeds(roster, "all", needs, o)
	if err != nil || p.To != "b/claude.old1" || !p.Listening || !strings.Contains(p.Why, "listening now") {
		t.Fatalf("listening first, got %+v %v", p, err)
	}

	// A target narrows where to look; the session on repo X is picked whatever else listens.
	p, err = PickNeeds(roster, "c", needs, o)
	if err != nil || p.To != "c/claude.new1" {
		t.Errorf("at machine c: %+v %v", p, err)
	}
	if p, err = PickNeeds(roster, "all", []string{"repo:web"}, o); err != nil || p.To != "a/claude.web1" {
		t.Errorf("repo:web by name: %+v %v", p, err)
	}
}

func TestPickNeedsRefusesWithTheClosestCandidates(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	roster := []msg.RosterEntry{
		{Me: "a/codex.x", Seen: now.Add(-1 * time.Minute), Labels: msg.Labels{"repo": "web", "tool": "codex"}}, // lacks all 3
		{Me: "b/codex.y", Seen: now.Add(-2 * time.Minute), Labels: msg.Labels{"repo": "go-common", "origin": "o/go-common", "tool": "codex"}},
		{Me: "c/claude.z", Seen: now.Add(-6 * time.Hour), Labels: msg.Labels{"repo": "go-common", "origin": "o/go-common", "tool": "claude", msg.CapsLabel: "gpu"}},
		{Me: "d/claude.w", Seen: now.Add(-1 * time.Second), Labels: msg.Labels{"tool": "claude"}, Listening: true},
		{Me: "e/gemini.v", Seen: now.Add(-1 * time.Second), Labels: msg.Labels{"tool": "gemini"}},
	}
	needs := []string{"repo:o/go-common", "tool:claude", "gpu"}
	p, err := PickNeeds(roster, "all", needs, PickOpts{Now: now})
	if !errors.Is(err, ErrNoMatch) || p.To != "" {
		t.Fatalf("nobody live has it all: %+v %v", p, err)
	}
	if !strings.Contains(err.Error(), "none of the 5 session(s) at all seen in the last 30m0s has repo:o/go-common,tool:claude,gpu") {
		t.Errorf("err: %v", err)
	}
	var got []string
	for _, c := range p.Closest {
		got = append(got, CandidateLine(c, now))
	}
	want := []string{
		"c/claude.z (not live, seen 6h0m0s ago): has everything, but is not live", // lacks nothing, but stale
		"d/claude.w (listening now): lacks repo:o/go-common,gpu",                  // lacks 2, listening
		"b/codex.y (seen 2m0s ago): lacks tool:claude,gpu",                        // lacks 2, seen
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("closest:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := PickNeeds(nil, "all", needs, PickOpts{Now: now}); !errors.Is(err, ErrNoMatch) {
		t.Errorf("an empty roster: %v", err)
	}
}
