package tasks

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func ownerRes(machine string, needs ...string) msg.OwnerResolution {
	return msg.OwnerResolution{Subject: "0mcp:vm701", Record: msg.OwnerRecord{Subject: "0mcp:vm701", Machine: machine, Needs: needs, By: machine + "/claude.aaaa"}}
}

func TestOwnerDeliveryPicksTheSessionThatHasTheNeeds(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	pve := msg.Labels{msg.RoleLabel: "pve", "tool": "claude"}
	other := msg.Labels{"tool": "claude"}
	roster := []msg.RosterEntry{
		{Me: "imac/claude.pve1", Seen: now.Add(-10 * time.Minute), Labels: pve},
		{Me: "imac/claude.pve2", Seen: now.Add(-2 * time.Minute), Labels: pve, Listening: true},
		{Me: "imac/claude.b5c9", Seen: now.Add(-time.Second), Labels: other}, // the most recent, but not the pve workstream
		{Me: "air/claude.pve3", Seen: now, Labels: pve},                      // has the role, on another machine
	}
	r := OwnerDelivery(roster, ownerRes("imac", "role:pve"), "me/claude.self", now)
	if r.Person || r.To != "imac/claude.pve2" || !strings.Contains(r.Why, "listening now") {
		t.Fatalf("the listening session with the role on the owner's machine: %+v", r)
	}
	roster[1].Listening = false
	if r = OwnerDelivery(roster, ownerRes("imac", "role:pve"), "me/claude.self", now); r.To != "imac/claude.pve2" {
		t.Errorf("then the most recently seen of those with the role: %+v", r)
	}
}

func TestOwnerDeliveryFallsBackToThePersonWhenNobodyThereIsLive(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	pve := msg.Labels{msg.RoleLabel: "pve"}
	for name, tc := range map[string]struct {
		roster []msg.RosterEntry
		res    msg.OwnerResolution
	}{
		"the role holder was seen 2h ago":   {[]msg.RosterEntry{{Me: "imac/claude.pve1", Seen: now.Add(-2 * time.Hour), Labels: pve}}, ownerRes("imac", "role:pve")},
		"live, but without the role":        {[]msg.RosterEntry{{Me: "imac/claude.b5c9", Seen: now, Labels: msg.Labels{"tool": "claude"}}}, ownerRes("imac", "role:pve")},
		"the role is held on another host":  {[]msg.RosterEntry{{Me: "air/claude.pve3", Seen: now, Labels: pve}}, ownerRes("imac", "role:pve")},
		"no needs, everyone is stale":       {[]msg.RosterEntry{{Me: "imac/claude.a", Seen: now.Add(-3 * time.Hour)}}, ownerRes("imac")},
		"no needs, only other machines":     {[]msg.RosterEntry{{Me: "air/claude.a", Seen: now}}, ownerRes("imac")},
		"no needs, only the asker is there": {[]msg.RosterEntry{{Me: "imac/claude.self", Seen: now, Listening: true}}, ownerRes("imac")},
	} {
		r := OwnerDelivery(tc.roster, tc.res, "imac/claude.self", now)
		if !r.Person || r.To != "imac/human" || !strings.Contains(r.Why, "task for the person") {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

func TestOwnerDeliveryWithoutNeedsGoesToTheMachine(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	roster := []msg.RosterEntry{
		{Me: "imac/claude.a", Seen: now.Add(-5 * time.Minute)},
		{Me: "IMAC/codex.b", Seen: now.Add(-29 * time.Minute)},
		{Me: "imac/claude.old", Seen: now.Add(-3 * time.Hour)},
		{Me: "imac2/claude.c", Seen: now},
	}
	r := OwnerDelivery(roster, ownerRes("imac"), "me/claude.self", now)
	if r.Person || r.To != "imac" || !strings.Contains(r.Why, "2 live session(s)") {
		t.Errorf("every live session there is shown it: %+v", r)
	}
}
