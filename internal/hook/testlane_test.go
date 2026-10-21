package hook

import (
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// A test message never gets into the context of a session, not even of one that opted in to the lane: the
// session-start hook does not ask for the lane, so the node does not serve it, and a node that does not know the
// lane and serves it anyway is not believed (client.keepLane).
func TestAHookNeverPutsATestMessageIntoAnOptedInSessionsContext(t *testing.T) {
	s, _ := mesh(t)
	t.Setenv("AIVOICEBUS_TEST", "1") // this session opted in
	if _, err := s.Send(msg.Message{ID: msg.NewTestID(), From: "other/agent", To: "all", Body: "a test note that must stay out of the context", TTL: 300}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(msg.Message{ID: msg.NewTestID(), From: "other/agent", To: "air/codex", Prio: msg.PrioUrgent, Body: "an urgent test too", TTL: 300}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(msg.Message{From: "other/agent", To: "all", Body: "deploy finished"}); err != nil {
		t.Fatal(err)
	}
	got := runHook(t, "")
	if !strings.Contains(got, "deploy finished") {
		t.Errorf("the ordinary message is shown: %q", got)
	}
	for _, leak := range []string{"a test note", "an urgent test"} {
		if strings.Contains(got, leak) {
			t.Errorf("a test message is in the hook's context (%q):\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "1 unread") {
		t.Errorf("the count is of what the session may read: %q", got)
	}
	// and the hook did not acknowledge them: they are still there for a read that asks for the lane
	lane := s.InboxLane("air/codex", false, msg.Labels{msg.TestLabel: msg.TestOn}, true)
	if len(lane) != 2 {
		t.Errorf("the lane's messages stay unread after a hook: %v", len(lane))
	}
}
