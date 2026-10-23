package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Ids are random, so two senders can draw one. A node used to answer the second with the first
// sender's message and a 200, and the second sender was told its words were sent.

func TestSendRefusesAnIdAnotherMessageHoldsAndStoresNothing(t *testing.T) {
	for name, id := range map[string]string{"ordinary id": "00aa11bb22", "test-lane id": msg.NewTestID()} {
		t.Run(name, func(t *testing.T) {
			var said strings.Builder
			s, _ := Open(t.TempDir(), "n1", WithLog(&said))
			first, err := s.Send(msg.Message{ID: id, From: "mac/claude.a", To: "air/codex.b", Body: "first words", TTL: 300})
			if err != nil {
				t.Fatal(err)
			}
			before := s.VV()["n1"]
			got, err := s.Send(msg.Message{ID: id, From: "desk/claude.c", To: "air/codex.d", Body: "other words", TTL: 300})
			if !errors.Is(err, ErrIDTaken) {
				t.Fatalf("another sender's message under a held id must be refused, got %+v %v", got, err)
			}
			if got.ID != "" || got.Body != "" {
				t.Errorf("a refusal returns nothing of anybody's: %+v", got)
			}
			if s.VV()["n1"] != before || len(s.msgs) != 1 {
				t.Errorf("nothing is stored for a refusal: %v, %d messages", s.VV(), len(s.msgs))
			}
			if held, _ := s.byIDLocked(id); held.Body != first.Body {
				t.Errorf("the message that holds the id is untouched: %+v", held)
			}
			if l := said.String(); !strings.Contains(l, id) || !strings.Contains(l, "desk/claude.c") || !strings.Contains(l, "mac/claude.a") {
				t.Errorf("the node's log says which two senders met: %q", l)
			}
			if st := s.Stats(); st.RefusedBy["id_taken"] != 1 {
				t.Errorf("a refusal is counted for the metrics: %v", st.RefusedBy)
			}
		})
	}
}

func TestTheSameMessageSentAgainUnderItsIdIsStillOneMessage(t *testing.T) {
	// a client retries, or writes one message to several nodes: the node answers with the message it holds
	s, _ := Open(t.TempDir(), "n1")
	m := msg.Message{ID: "00aa11bb22", From: "mac/claude.a", To: "air/codex.b", Re: "", Kind: msg.KindQuestion, Prio: msg.PrioAsk, Body: "which branch?"}
	first, err := s.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Send(m)
	if err != nil || again.ID != first.ID || again.At != first.At {
		t.Fatalf("a retry returns the message held: %+v %v", again, err)
	}
	// and the message as the node returned it (it carries At and Thread, which a client may send back)
	if back, err := s.Send(first); err != nil || back.ID != first.ID {
		t.Fatalf("%+v %v", back, err)
	}
	if s.VV()["n1"] != 1 {
		t.Errorf("stored again: %v", s.VV())
	}
}

func TestEachWordOfWhatASenderChoosesMakesAnotherMessageOfAHeldId(t *testing.T) {
	base := msg.Message{ID: "00aa11bb22", From: "mac/claude.a", To: "air/codex.b", Kind: msg.KindQuestion, Prio: msg.PrioAsk, Body: "which branch?"}
	for name, change := range map[string]func(*msg.Message){
		"from": func(m *msg.Message) { m.From = "mac/claude.z" },
		"to":   func(m *msg.Message) { m.To = "air/codex.z" },
		"kind": func(m *msg.Message) { m.Kind = "" },
		"prio": func(m *msg.Message) { m.Prio = msg.PrioUrgent },
		"body": func(m *msg.Message) { m.Body = "which tag?" },
	} {
		s, _ := Open(t.TempDir(), "n1")
		if _, err := s.Send(base); err != nil {
			t.Fatal(err)
		}
		other := base
		change(&other)
		if _, err := s.Send(other); !errors.Is(err, ErrIDTaken) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestARefusedIdTakenSaysWhatToDo(t *testing.T) {
	// an old client shows the node's words as they are: they must say the next step
	if e := ErrIDTaken.Error(); !strings.Contains(e, "send it again") {
		t.Errorf("%q", e)
	}
}

// Replicated records keep the behaviour they had: each node keeps the message it applied first, and the
// other takes its place in the log numbering without being indexed. A node cannot refuse what a peer wrote.
func TestMergeOfTwoMessagesWithOneIdKeepsTheFirstEachNodeApplied(t *testing.T) {
	var said strings.Builder
	a, _ := Open(t.TempDir(), "n1", WithLog(&said))
	b, _ := Open(t.TempDir(), "n2")
	id := "00aa11bb22"
	if _, err := a.Send(msg.Message{ID: id, From: "mac/claude.a", To: "all", Body: "from the first sender"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Send(msg.Message{ID: id, From: "desk/claude.c", To: "all", Body: "from the second sender"}); err != nil {
		t.Fatal(err)
	}
	if n, err := a.Merge(b.Since(map[string]uint64{}, 10)); err != nil || n != 1 {
		t.Fatalf("the record is applied (it holds its place in the numbering): %d %v", n, err)
	}
	if n, err := b.Merge(a.Since(map[string]uint64{}, 10)); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if got := said.String(); !strings.Contains(got, id) || !strings.Contains(got, "desk/claude.c") {
		t.Errorf("a dropped message is said in the node's log, so the divergence can be found: %q", got)
	}
	for node, want := range map[*Store]string{a: "from the first sender", b: "from the second sender"} {
		if got := node.Thread(id); len(got) != 1 || got[0].Body != want {
			t.Errorf("a node keeps the message it applied first: %+v, want %q", got, want)
		}
		if vv := node.VV(); vv["n1"] != 1 || vv["n2"] != 1 {
			t.Errorf("both records are in the log: %v", vv)
		}
	}
}

// A test message written to two nodes is one message, and once purged on one of them its stub (no
// body) next to the other node's record is not two different messages: nothing is said of it.
func TestAPurgedTestMessageNextToItsOtherRecordIsNotACollision(t *testing.T) {
	a, now, _ := laneStore(t)
	var said strings.Builder
	a.logw = &said
	m := sendTest(t, a, "mac/claude.a", "air/codex.b", "written to two nodes", 60)
	b, _ := Open(t.TempDir(), "n2")
	b.SetClock(func() time.Time { return *now })
	if _, err := b.Send(m); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Minute)
	a.Stats() // a's copy is its stub now
	if n, err := a.Merge(b.Since(map[string]uint64{}, 10)); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if said.Len() != 0 {
		t.Errorf("a stub and the record it was made from are one message: %q", said.String())
	}
}
