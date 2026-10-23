package viewer

import (
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func receiptsOf(m map[string]msg.Receipts) func(string) (msg.Receipts, bool) {
	return func(id string) (msg.Receipts, bool) { r, ok := m[id]; return r, ok }
}

func TestTheOrphanedLaneListsMailForGoneSessionsAndWhatBecameOfIt(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	repo := msg.Labels{"repo": "api", "tool": "claude"}
	roster := []msg.RosterEntry{
		{Me: "mac/claude.gone", Seen: now.Add(-3 * time.Hour), Labels: repo, Title: "Old work"},
		{Me: "mac/claude.live", Seen: now.Add(-time.Minute), Labels: repo},
		{Me: "mac/claude.wait", Seen: now.Add(-9 * time.Hour), Listening: true, Labels: repo},
	}
	at := now.Add(-time.Hour)
	note := func(id, to, body string) msg.Message {
		return msg.Message{ID: id, Thread: id, From: "air/claude.asker", FromTitle: "Asker", To: to, Body: body, At: at}
	}
	marked := note(msg.NewSuccessorID(), "mac/claude.gone", "marked and unread\nsecond line")
	kept := note("0000000002", "mac/claude.gone", "not marked, unread")
	handed := note(msg.NewSuccessorID(), "mac/claude.gone", "marked and read by the successor")
	own := note("0000000004", "mac/claude.gone", "read by the one it names")
	live := note("0000000005", "mac/claude.live", "to a live session")
	waiting := note("0000000006", "mac/claude.wait", "to one in a wait")
	forgotten := note("0000000007", "mac/claude.forgotten", "to one the roster forgot")
	machine := note("0000000008", "mac", "to a machine")
	selector := note("0000000011", "@repo=my.repo", "to a selector whose value has a dot, which is no session")
	tool := note("0000000012", "mac/claude", "to a tool")
	claim := msg.Message{ID: "0000000009", Thread: "0000000009", From: "air/claude.asker", To: "mac/claude.gone", Kind: msg.KindClaim, Body: "x", At: at}
	old := note(msg.NewSuccessorID(), "mac/claude.gone", "read by the successor days ago")
	retracted := note("0000000010", "mac/claude.gone", "withdrawn")
	retracted.Retracted = &msg.Retraction{By: retracted.From}
	reads := map[string]msg.Receipts{}
	for _, m := range []msg.Message{marked, kept, handed, own, live, waiting, forgotten, machine, selector, tool, claim, old, retracted} {
		reads[m.ID] = msg.Receipts{ID: m.ID}
	}
	reads[handed.ID] = msg.Receipts{Reads: []msg.Receipt{{By: "mac/claude.live", At: now.Add(-10 * time.Minute)}}}
	reads[own.ID] = msg.Receipts{Reads: []msg.Receipt{{By: "mac/claude.gone", At: now.Add(-30 * time.Minute)}}}
	reads[old.ID] = msg.Receipts{Reads: []msg.Receipt{{By: "mac/claude.live", At: now.Add(-3 * 24 * time.Hour)}}}

	got := Orphans(roster, []msg.Message{marked, kept, handed, own, live, waiting, forgotten, machine, selector, tool, claim, old, retracted}, receiptsOf(reads), now)
	by := map[string]Orphan{}
	for _, o := range got {
		by[o.ID] = o
	}
	if len(got) != 4 {
		t.Fatalf("want marked, kept, handed and the one the roster forgot; got %d: %+v", len(got), got)
	}
	if o := by[marked.ID]; !o.Handover || o.Successor != "mac/claude.live" || o.ReadBy != "" || o.Line != "marked and unread" || o.ToTitle != "Old work" || o.LastSeen == nil {
		t.Errorf("marked: %+v", o)
	}
	if o := by[kept.ID]; o.Handover || o.Successor != "" || o.ReadBy != "" {
		t.Errorf("kept: %+v", o)
	}
	if o := by[handed.ID]; o.ReadBy != "mac/claude.live" || !o.Handover {
		t.Errorf("handed: %+v", o)
	}
	if o := by[forgotten.ID]; o.LastSeen != nil || o.To != "mac/claude.forgotten" {
		t.Errorf("forgotten: %+v", o)
	}
	for _, id := range []string{own.ID, live.ID, waiting.ID, machine.ID, selector.ID, tool.ID, claim.ID, old.ID, retracted.ID} {
		if _, in := by[id]; in {
			t.Errorf("%s must not be in the lane", id)
		}
	}
}

func TestTheOrphanedLaneNeverNamesTheSenderAsTheSuccessorAndShowsNobodyWhenNobodyQualifies(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	repo := msg.Labels{"repo": "api", "tool": "claude"}
	roster := []msg.RosterEntry{
		{Me: "mac/claude.gone", Seen: now.Add(-3 * time.Hour), Labels: repo},
		{Me: "mac/claude.asker", Seen: now, Labels: repo},
	}
	m := msg.Message{ID: msg.NewSuccessorID(), Thread: "t", From: "mac/claude.asker", To: "mac/claude.gone", Body: "x", At: now.Add(-time.Hour)}
	got := Orphans(roster, []msg.Message{m}, receiptsOf(map[string]msg.Receipts{m.ID: {ID: m.ID}}), now)
	if len(got) != 1 || got[0].Successor != "" || !got[0].Handover {
		t.Errorf("only the sender is live in the repo: %+v", got)
	}
}

func TestTheOrphanedLaneIgnoresMailToAToolOrANameNoRosterEntryWears(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	m := msg.Message{ID: "0000000001", Thread: "t", From: "air/claude.asker", To: "air/amend", Body: "to every session of the tool amend", At: now.Add(-time.Hour)}
	got := Orphans(nil, []msg.Message{m}, receiptsOf(map[string]msg.Receipts{m.ID: {ID: m.ID}}), now)
	if len(got) != 0 {
		t.Errorf("host/word is not a session that is gone: %+v", got)
	}
}
