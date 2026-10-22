package mcp

import (
	"io"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// recordOwner writes an owner record as session as, the way `aivoicebus owner set` does.
func recordOwner(t *testing.T, as, subject, note string) {
	t.Helper()
	t.Setenv("AIVOICEBUS_ME", as)
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := outbox.Prepare(c, outbox.Request{To: as, Body: msg.LeaseBody(msg.OwnerKey(subject), note), Kind: msg.KindLease, TTL: "30d"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Send(c); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIVOICEBUS_ME", "me/mcp.test")
}

// An agent can send, task and ask the owner of a subject. A record nobody pinned only previews until the
// agent confirms, since anyone with the token can write one; an urgent message never goes through it.
func TestSendTaskAndAskToAnOwnerThroughMCP(t *testing.T) {
	s, st := start(t)
	pve := msg.Labels{"role": "pve", "tool": "claude"}
	st.InboxFor("imac/claude.pve1", false, pve)
	st.InboxFor("imac/claude.b5c9", false, msg.Labels{"tool": "claude"})
	recordOwner(t, "imac/claude.fce3", "0mcp:vm701", "machine=imac needs=role:pve person=florin")
	recordOwner(t, "imac/claude.fce3", "service:gateway", "machine=imac needs=role:gone")

	text, isErr := s.call("aivoicebus_send", map[string]any{"to": "owner:0mcp:vm701", "body": "who tunes vm701?"})
	if isErr || !strings.Contains(text, "owner:0mcp:vm701 -> imac needs role:pve (florin)") || !strings.Contains(text, "-> imac/claude.pve1") || !strings.Contains(text, "this owner record is not pinned") {
		t.Fatalf("unconfirmed send: %v %q", isErr, text)
	}
	if got := st.InboxFor("imac/claude.pve1", false, pve); len(got) != 0 {
		t.Fatalf("a preview sent something: %+v", got)
	}
	text, isErr = s.call("aivoicebus_send", map[string]any{"to": "owner:0mcp:vm701", "body": "who tunes vm701?", "confirm_caps": true})
	if isErr || !strings.Contains(text, "sent ") || !strings.Contains(text, "owner:0mcp:vm701 -> imac") {
		t.Fatalf("confirmed send: %v %q", isErr, text)
	}
	if got := st.InboxFor("imac/claude.pve1", false, pve); len(got) != 1 || got[0].To != "imac/claude.pve1" || got[0].Body != "who tunes vm701?" {
		t.Errorf("the role holder's inbox: %+v", got)
	}
	if got := st.InboxFor("imac/claude.b5c9", false, msg.Labels{"tool": "claude"}); len(got) != 0 {
		t.Errorf("the other session gets nothing: %+v", got)
	}
	text, isErr = s.call("aivoicebus_send", map[string]any{"to": "owner:0mcp:vm701", "body": "stop it", "prio": "urgent", "confirm_caps": true})
	if !isErr || !strings.Contains(text, "never used for an urgent message") {
		t.Errorf("urgent through an unpinned record: %v %q", isErr, text)
	}

	// nobody there has the role: a plain send goes to the person as a task marked for the person
	if text, isErr = s.call("aivoicebus_send", map[string]any{"to": "owner:service:gateway", "body": "fyi: failover tonight", "confirm_caps": true}); isErr || !strings.Contains(text, "task for the person") || !strings.Contains(text, "sent ") {
		t.Errorf("send to the person: %v %q", isErr, text)
	}
	if got := st.InboxFor("imac/human", false, nil); len(got) != 1 || got[0].Kind != msg.KindTask || got[0].Body != msg.PersonTag+" fyi: failover tonight" {
		t.Errorf("the person's inbox after a send: %+v", got)
	}
	if text, isErr = s.call("aivoicebus_send", map[string]any{"to": "owner:service:gateway", "body": "which pool?", "kind": "question", "confirm_caps": true}); !isErr || !strings.Contains(text, "without kind") {
		t.Errorf("a question for the person: %v %q", isErr, text)
	}

	// a task: previewed, then confirmed; with needs of its own it is refused
	if text, isErr = s.call("aivoicebus_task", map[string]any{"to": "owner:0mcp:vm701", "body": "check the pool"}); isErr || !strings.Contains(text, "not pinned") || strings.Contains(text, "task ") && strings.Contains(text, "sent") {
		t.Errorf("unconfirmed task: %v %q", isErr, text)
	}
	if text, isErr = s.call("aivoicebus_task", map[string]any{"to": "owner:0mcp:vm701", "body": "check the pool", "needs": "gpu", "confirm_caps": true}); !isErr || !strings.Contains(text, "picks its own session") {
		t.Errorf("task with needs: %v %q", isErr, text)
	}
	if text, isErr = s.call("aivoicebus_task", map[string]any{"to": "owner:0mcp:vm701", "body": "check the pool", "confirm_caps": true}); isErr || !strings.Contains(text, "task ") || !strings.Contains(text, "-> imac/claude.pve1") {
		t.Errorf("confirmed task: %v %q", isErr, text)
	}
	// nobody there has the role: a task goes to the person; an ask sends nothing
	if text, isErr = s.call("aivoicebus_task", map[string]any{"to": "owner:service:gateway", "body": "the gateway is flapping", "confirm_caps": true}); isErr || !strings.Contains(text, "task for the person") {
		t.Errorf("task for the person: %v %q", isErr, text)
	}
	if got := st.InboxFor("imac/human", false, nil); len(got) != 2 || got[1].Kind != msg.KindTask || !strings.HasPrefix(got[1].Body, msg.PersonTag+" the gateway") {
		t.Errorf("the person's inbox after the task (the send before it, then the task): %+v", got)
	}
	if text, isErr = s.call("aivoicebus_ask", map[string]any{"target": "owner:service:gateway", "body": "why?"}); isErr || !strings.Contains(text, "not pinned") {
		t.Errorf("unconfirmed ask: %v %q", isErr, text)
	}
	text, isErr = s.call("aivoicebus_ask", map[string]any{"target": "owner:service:gateway", "body": "why?", "confirm_caps": true})
	if isErr || !strings.HasPrefix(text, "owner:service:gateway") || !strings.Contains(text, "NOBODY MATCHED") || !strings.Contains(text, "aivoicebus_task") {
		t.Errorf("ask with nobody: %v %q", isErr, text)
	}
	if text, isErr = s.call("aivoicebus_ask", map[string]any{"target": "owner:0mcp:vm701", "body": "x", "any": true, "confirm_caps": true}); !isErr || !strings.Contains(text, "picks its own session") {
		t.Errorf("ask with any: %v %q", isErr, text)
	}
	if text, isErr = s.call("aivoicebus_send", map[string]any{"to": "owner:1docker:vm1", "body": "x"}); !isErr || !strings.Contains(text, "no owner is recorded") {
		t.Errorf("no owner: %v %q", isErr, text)
	}
}
