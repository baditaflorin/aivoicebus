package store

import (
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func rosterTitle(s *Store, me string) string {
	for _, e := range s.Roster() {
		if e.Me == me {
			return e.Title
		}
	}
	return "(absent)"
}

func TestNoteTitleNamesASessionAndAReadThatDeclaresNoneKeepsIt(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	s.InboxFor("h/claude.a1b2", false, msg.Labels{"repo": "x"})
	s.NoteTitle("h/claude.a1b2", "Fix the login")
	if got := rosterTitle(s, "h/claude.a1b2"); got != "Fix the login" {
		t.Fatalf("%q", got)
	}
	s.InboxFor("h/claude.a1b2", false, nil) // a later read declares no title
	if got := rosterTitle(s, "h/claude.a1b2"); got != "Fix the login" {
		t.Errorf("a read without a title forgot it: %q", got)
	}
	s.NoteTitle("h/claude.a1b2", "")
	if got := rosterTitle(s, "h/claude.a1b2"); got != "Fix the login" {
		t.Errorf("an empty title is no title: %q", got)
	}
	s.NoteTitle("h/codex.q1w2", "Only sends") // known only from what it sent
	if got := rosterTitle(s, "h/codex.q1w2"); got != "Only sends" {
		t.Errorf("%q", got)
	}
}

func TestTheTitleSurvivesARestartAndTravelsByGossip(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	s.InboxFor("h/claude.a1b2", false, nil)
	s.NoteTitle("h/claude.a1b2", "Fix the login")
	s.FlushPresence()
	s2, _ := Open(dir, "n1")
	if got := rosterTitle(s2, "h/claude.a1b2"); got != "Fix the login" {
		t.Errorf("after a restart: %q", got)
	}
	peer, _ := Open(t.TempDir(), "n2")
	if n := peer.MergePresence(s2.Roster()); n != 1 {
		t.Fatalf("merged %d", n)
	}
	if got := rosterTitle(peer, "h/claude.a1b2"); got != "Fix the login" {
		t.Errorf("by gossip: %q", got)
	}
	// a peer's word is cleaned again
	peer.MergePresence([]msg.RosterEntry{{Me: "h/x", Seen: time.Now(), Title: "evil\u202e\nname"}})
	if got := rosterTitle(peer, "h/x"); got != "evil name" {
		t.Errorf("%q", got)
	}
	// an older sighting still gives a title the node lacks
	peer.MergePresence([]msg.RosterEntry{{Me: "h/y", Seen: time.Now()}})
	peer.MergePresence([]msg.RosterEntry{{Me: "h/y", Seen: time.Now().Add(-time.Hour), Title: "late name"}})
	if got := rosterTitle(peer, "h/y"); got != "late name" {
		t.Errorf("%q", got)
	}
}

func TestAMessageKeepsItsTitleAndAWithdrawnOneLosesIt(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	m, err := s.Send(msg.Message{From: "h/claude.a1b2", FromTitle: "Fix the login", FromModel: "claude-opus-5-5", To: "r/codex.q1w2", Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	in := s.Inbox("r/codex.q1w2", false)
	if len(in) != 1 || in[0].FromTitle != "Fix the login" {
		t.Fatalf("%+v", in)
	}
	if _, err := s.Retract(m.ID, "h/claude.a1b2", false); err != nil {
		t.Fatal(err)
	}
	for _, x := range s.Thread(m.ID) {
		if x.ID == m.ID && (x.FromTitle != "" || x.FromModel != "") {
			t.Errorf("a tombstone keeps the sender's title or model: %q %q", x.FromTitle, x.FromModel)
		}
	}
}
