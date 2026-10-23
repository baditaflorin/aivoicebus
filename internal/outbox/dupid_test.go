package outbox

import (
	"io"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// A plan is built with an id; when a node holds another message under it, the message goes out under a
// fresh one, and the plan (which `send` reads to tell a repeat from a new message) holds the id it went out under.
func TestAPlanHoldsTheIdItWentOutUnderWhenTheFirstWasTaken(t *testing.T) {
	c, s := meshWith(t, "me/claude.0001", map[string]string{"air/codex.b": ""})
	p, err := Prepare(c, Request{To: "air/codex.b", Body: "my words"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	taken := p.Msg.ID
	if _, err := s.Send(msg.Message{ID: taken, From: "desk/claude.c", To: "air/codex.d", Body: "somebody else's words"}); err != nil {
		t.Fatal(err)
	}
	out, err := p.Send(c)
	if err != nil {
		t.Fatal(err)
	}
	if out.ID == taken || out.Body != "my words" || p.Msg.ID != out.ID {
		t.Fatalf("the message went out under a new id and the plan says which: plan %s, sent %+v", p.Msg.ID, out)
	}
	if got := s.Thread(taken); len(got) != 1 || got[0].Body != "somebody else's words" {
		t.Errorf("%+v", got)
	}
	// `retract` without an id takes back what this machine sent last: it is the message that went out
	if e, exact, ok := lastSent("me/claude.0001", time.Now(), false); !ok || !exact || e.ID != out.ID {
		t.Errorf("the last message sent is %+v, want %s", e, out.ID)
	}
}

func TestAPlanThatIsNotATestStaysOutOfTheLaneAndATestOneStaysIn(t *testing.T) {
	c, s := laneMesh(t, nil, "test=on")
	p, err := Prepare(c, Request{To: "air/codex.b", Body: "calc 17*23", Test: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	taken := p.Msg.ID
	if _, err := s.Send(msg.Message{ID: taken, From: "desk/claude.c", To: "air/codex.d", Body: "somebody else's test", TTL: 60}); err != nil {
		t.Fatal(err)
	}
	out, err := p.Send(c)
	if err != nil || out.ID == taken || !msg.IsTestID(out.ID) || !out.IsTest() || p.Msg.ID != out.ID {
		t.Fatalf("a test message keeps a test id: %+v %v", out, err)
	}
}
