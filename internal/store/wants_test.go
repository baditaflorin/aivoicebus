package store

import (
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func sendAt(t *testing.T, s *Store, from, to, body string) msg.Message {
	t.Helper()
	m, err := s.Send(msg.Message{From: from, To: to, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A message that names an earlier one of its sender's machine hides it from every session that has
// not read it, the ones that start later too; --all and the thread still show it.
func TestSupersedesHidesTheEarlierMessageFromEverySession(t *testing.T) {
	s := open(t)
	old := sendAt(t, s, "desk/claude.s1", "air", "update to 0.15.0")
	if !inboxHas(s, "air/claude.a1", old.ID) {
		t.Fatal("shown before")
	}
	neu := sendAt(t, s, "desk/claude.s2", "air", msg.AppendWants("update to 0.15.1", msg.Wants{Supersedes: old.ID}))
	for _, me := range []string{"air/claude.a1", "air/codex.new"} {
		if inboxHas(s, me, old.ID) || !inboxHas(s, me, neu.ID) {
			t.Errorf("%s: only the replacement is shown", me)
		}
	}
	if len(s.Inbox("air/claude.c3", true)) != 2 || len(s.Thread(old.ID)) != 1 {
		t.Error("--all and the thread still read the earlier one")
	}
}

func TestOnlyTheSendersMachineCanSupersede(t *testing.T) {
	s := open(t)
	old := sendAt(t, s, "desk/claude.s1", "air", "do the thing")
	sendAt(t, s, "other/claude.s9", "air", msg.AppendWants("never mind", msg.Wants{Supersedes: old.ID}))
	if !inboxHas(s, "air/claude.a1", old.ID) {
		t.Error("another machine cannot hide a message it did not send")
	}
	appr, _ := s.Send(msg.Message{From: "desk/claude.s1", To: "air/human", Kind: msg.KindTask, Body: msg.ApprovalTag + " run it?"})
	sendAt(t, s, "desk/claude.s1", "air/human", msg.AppendWants("no", msg.Wants{Supersedes: appr.ID}))
	if !inboxHas(s, "air/human", appr.ID) {
		t.Error("an approval request is never replaced")
	}
}

// The same key from the same machine to the same target: only the latest is delivered, however many
// were sent; another target or another key is another purpose.
func TestAKeyKeepsOnlyTheLatestForTheSameTarget(t *testing.T) {
	s := open(t)
	k := msg.Wants{Key: "update-air"}
	a := sendAt(t, s, "desk/claude.s1", "air", msg.AppendWants("update to 0.15.0", k))
	other := sendAt(t, s, "desk/claude.s1", "air", msg.AppendWants("clean the disk", msg.Wants{Key: "disk-air"}))
	elsewhere := sendAt(t, s, "desk/claude.s1", "mac", msg.AppendWants("update to 0.15.0", k))
	b := sendAt(t, s, "desk/claude.s1", "air", msg.AppendWants("update to 0.15.1", k))
	c := sendAt(t, s, "desk/claude.s7", "air", msg.AppendWants("update to 0.15.2", k)) // a new session of the same machine takes over
	if inboxHas(s, "air/claude.x", a.ID) || inboxHas(s, "air/claude.x", b.ID) {
		t.Error("the earlier ones are replaced")
	}
	if !inboxHas(s, "air/claude.x", c.ID) || !inboxHas(s, "air/claude.x", other.ID) || !inboxHas(s, "mac/claude.x", elsewhere.ID) {
		t.Error("the latest, another key and another target are all delivered")
	}
}

// A retracted replacement replaces nothing: the earlier message is delivered again.
func TestARetractedReplacementBringsTheEarlierOneBack(t *testing.T) {
	s := open(t)
	old := sendAt(t, s, "desk/claude.s1", "air", "update")
	neu := sendAt(t, s, "desk/claude.s1", "air", msg.AppendWants("update again", msg.Wants{Supersedes: old.ID}))
	if inboxHas(s, "air/claude.x", old.ID) {
		t.Fatal("replaced")
	}
	if _, err := s.Retract(neu.ID, "desk/claude.s1", false); err != nil {
		t.Fatal(err)
	}
	if !inboxHas(s, "air/claude.y", old.ID) {
		t.Error("the replacement was taken back: the earlier one is delivered again")
	}
}

// Two messages cannot replace each other: only a later one replaces an earlier one.
func TestSupersedesNeverMakesACycle(t *testing.T) {
	s := open(t)
	now := time.Now()
	s.SetClock(func() time.Time { return now })
	a := sendAt(t, s, "desk/claude.s1", "air", "first")
	now = now.Add(time.Second)
	b := sendAt(t, s, "desk/claude.s1", "air", msg.AppendWants("second", msg.Wants{Supersedes: a.ID}))
	now = now.Add(time.Second)
	c := sendAt(t, s, "desk/claude.s1", "air", msg.AppendWants("third", msg.Wants{Supersedes: b.ID}))
	if inboxHas(s, "air/claude.x", a.ID) || inboxHas(s, "air/claude.x", b.ID) || !inboxHas(s, "air/claude.x", c.ID) {
		t.Error("a chain: only the last is shown")
	}
}
