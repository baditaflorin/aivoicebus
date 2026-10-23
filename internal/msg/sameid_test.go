package msg

import (
	"testing"
	"time"
)

func TestNewIDLikeKeepsTheShapeOfTheIdItReplaces(t *testing.T) {
	for i := 0; i < 200; i++ {
		plain := NewID()
		if got := NewIDLike(plain); !ValidID(got) || IsTestID(got) || IsSuccessorID(got) || got == plain {
			t.Fatalf("an ordinary id gets another ordinary one, never a test id: %q -> %q", plain, got)
		}
		test := NewTestID()
		if got := NewIDLike(test); !ValidID(got) || !IsTestID(got) || IsSuccessorID(got) || got == test {
			t.Fatalf("a test id gets another test id, or the message would stop being a test: %q -> %q", test, got)
		}
		mark := NewSuccessorID()
		if got := NewIDLike(mark); !ValidID(got) || !IsSuccessorID(got) || IsTestID(got) || got == mark {
			t.Fatalf("a successor mark gets another one, or the nodes would stop handing the message on: %q -> %q", mark, got)
		}
	}
	for _, odd := range []string{"", "not an id", "ABCDEF0123"} { // whatever a hand-chosen id was, what comes back is an ordinary id
		if got := NewIDLike(odd); !ValidID(got) || IsTestID(got) || IsSuccessorID(got) {
			t.Errorf("%q -> %q", odd, got)
		}
	}
}

func TestSameSendIsWhatASenderChoosesAndNotWhatANodeStamps(t *testing.T) {
	base := Message{ID: "0123456789", From: "a/claude.x", To: "b/codex.y", Re: "aabbccddee", Kind: KindQuestion, Prio: PrioAsk, Body: "which?"}
	exp := time.Now().Add(time.Hour)
	stamped := base // what a node makes of it: it sets these, so two nodes holding one message differ in them
	stamped.At, stamped.Exp, stamped.Thread, stamped.MaxTurns, stamped.FromTitle, stamped.Sig = time.Now(), &exp, "aabbccddee", 4, "build", "sig"
	if !base.SameSend(stamped) || !stamped.SameSend(base) {
		t.Error("the node's own fields are not part of what was sent")
	}
	for name, change := range map[string]func(*Message){
		"from": func(m *Message) { m.From = "a/claude.z" },
		"to":   func(m *Message) { m.To = "b/codex.z" },
		"re":   func(m *Message) { m.Re = "" },
		"kind": func(m *Message) { m.Kind = "" },
		"prio": func(m *Message) { m.Prio = PrioUrgent },
		"body": func(m *Message) { m.Body = "which one?" },
	} {
		other := base
		change(&other)
		if base.SameSend(other) || other.SameSend(base) {
			t.Errorf("a message whose %s differs only shares an id with this one", name)
		}
	}
}
