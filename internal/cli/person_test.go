package cli

import (
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// A task meant for the person is refused to an agent's session, with what the person runs, and
// taken in the person's own terminal. One sent to an agent session by name is that session's.
func TestATaskForThePersonIsRefusedToAnAgentAndTakenAtATerminal(t *testing.T) {
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "task", "lap", "--person", "restart the router in the hall")
	if code != 0 {
		t.Fatalf("task --person: %d %s", code, e)
	}
	id, _ := sentID(t, out)
	c, _ := client.FromEnv()
	ms, _ := c.Thread(id)
	if len(ms) != 1 || ms[0].To != "lap/human" || !strings.HasPrefix(ms[0].Body, msg.PersonTag+" restart the router") || !msg.PersonOnly(ms[0]) {
		t.Fatalf("task --person sent %+v", ms)
	}
	if code, _, e := run(t, "", "task", "--person", "--tool", "codex", "lap", "x"); code != 2 || !strings.Contains(e, "one machine's person") {
		t.Errorf("--person with --tool: %d %q", code, e)
	}
	// an old-style one, to the whole machine, tagged
	code, out, e = run(t, "", "send", "--to", "lap", "--kind", "task", msg.TokenHandTag+" lap\nhands lap its token")
	if code != 0 {
		t.Fatalf("send: %d %s", code, e)
	}
	hand, _ := sentID(t, out)

	// the agent's session on lap
	t.Setenv("AIVOICEBUS_ME", sa)
	t.Setenv("AIVOICEBUS_AGENT", "claude.a1")
	for _, x := range []struct{ id, step string }{{id, "aivoicebus claim " + id}, {hand, "aivoicebus token install " + hand}} {
		code, out, e := run(t, "", "claim", x.id, "--settle", "0s")
		if code == 0 || !strings.Contains(e, "for the person on the machine, not for an agent") || !strings.Contains(e, x.step) {
			t.Errorf("an agent's claim of %s: %d %q %q", x.id, code, out, e)
		}
	}
	if _, q, _ := run(t, "", "queue"); !strings.Contains(q, "for the person, not an agent") || !strings.Contains(q, "aivoicebus token install "+hand) {
		t.Errorf("the agent's queue does not say the task is the person's:\n%s", q)
	} else if strings.Contains(q, id) {
		t.Errorf("an agent's queue lists the person's own queue:\n%s", q)
	}
	for _, x := range []string{id, hand} {
		ms, _ := c.Thread(x)
		for _, m := range ms {
			if m.Kind == msg.KindClaim {
				t.Errorf("a refused claim went out: %+v", m)
			}
		}
	}

	// the person, in their own terminal on lap
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_ME", "lap/agent")
	if _, q, _ := run(t, "", "queue"); !strings.Contains(q, id) || !strings.Contains(q, "restart the router") || !strings.Contains(q, "in your own terminal: aivoicebus claim "+id) {
		t.Errorf("the person's queue does not list their task:\n%s", q)
	}
	if code, out, e := run(t, "", "claim", id, "--settle", "0s"); code != 0 || !strings.Contains(out, "is yours") {
		t.Errorf("the person's claim: %d %q %q", code, out, e)
	}

	// sent to one agent session by name, it is that session's
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ = run(t, "", "send", "--to", sa, "--kind", "task", msg.TokenHandTag+" lap\nfor you, by name")
	named, _ := sentID(t, out)
	t.Setenv("AIVOICEBUS_ME", sa)
	t.Setenv("AIVOICEBUS_AGENT", "claude.a1")
	if code, out, e := run(t, "", "claim", named, "--settle", "0s"); code != 0 || !strings.Contains(out, "is yours") {
		t.Errorf("a claim of a task sent to this session by name: %d %q %q", code, out, e)
	}
}

// held, unhold and answer are the person's too, also where only the process chain or the
// environment says an agent runs the command.
func TestThePersonsCommandsRefuseWhereAnAgentToolMarksTheEnvironment(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "lap/florin")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("AIVOICEBUS_DETECT", "off") // names nothing; does not make an agent a person
	if code, _, e := run(t, "", "held"); code == 0 || !strings.Contains(e, "not for an agent's session") {
		t.Errorf("held under Claude Code's environment: %d %q", code, e)
	}
	t.Setenv("CLAUDECODE", "")
	if code, _, e := run(t, "", "held"); code != 0 {
		t.Errorf("held in a terminal: %d %q", code, e)
	}
}
