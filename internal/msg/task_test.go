package msg

import (
	"testing"
	"time"
)

func TestTaskOwnerIsTheEarliestClaimNotByTheSender(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	takes := []TakeRef{
		{ID: "c3", From: "lap/claude.b", At: t0.Add(2 * time.Second)},
		{ID: "c1", From: "boss/claude.a", At: t0}, // the task's own sender: never the owner
		{ID: "c2", From: "lap/codex.x", At: t0.Add(time.Second)},
	}
	if o, ok := TaskOwner("boss/claude.a", takes); !ok || o.ID != "c2" {
		t.Fatalf("owner %+v %v, want c2", o, ok)
	}
	// Same instant: the id decides, the same way on every node whatever order they arrived in.
	tie := []TakeRef{{ID: "b9", From: "x/a.1", At: t0}, {ID: "a1", From: "x/b.2", At: t0}}
	for _, order := range [][]TakeRef{tie, {tie[1], tie[0]}} {
		if o, _ := TaskOwner("s/c.0", order); o.ID != "a1" {
			t.Errorf("tie broken by arrival order: %+v", o)
		}
	}
	if _, ok := TaskOwner("s/c.0", nil); ok {
		t.Error("an untaken task has an owner")
	}
	if !Takes(Message{Kind: KindClaim, Re: "t"}) || !Takes(Message{Kind: KindAnswer, Re: "t"}) || Takes(Message{Kind: KindClaim}) || Takes(Message{Re: "t"}) {
		t.Error("Takes")
	}
}

func TestAClaimMustNameItsTask(t *testing.T) {
	if err := (Message{Kind: KindClaim}).ValidateOptions(); err == nil {
		t.Error("a claim without re accepted")
	}
	for _, k := range []string{KindTask, KindEnd} {
		if err := (Message{Kind: k}).ValidateOptions(); err != nil {
			t.Errorf("%s: %v", k, err)
		}
		m := Message{ID: "3f2a9c01de", From: "a/b", To: "c", Thread: "3f2a9c01de", Body: "x", At: time.Now(), Kind: k}
		if err := m.validateReplicated(); err != nil {
			t.Errorf("replicated %s refused: %v", k, err)
		}
	}
}
