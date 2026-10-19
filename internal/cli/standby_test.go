package cli

import (
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Standing by, an agent's wait never wakes on a task meant for the person as a task to take: it
// comes back once with a line that says it waits for the person and what they run, and never
// again for it. It also looks at the person's own queue, without making the person a session.
func TestStandbySkipsATaskForThePersonAndSaysSoOnce(t *testing.T) {
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "task", "lap", "--person", "swap the disk in the hall")
	personID, _ := sentID(t, out)
	_, out, _ = run(t, "", "send", "--to", "lap", "--kind", "task", msg.TokenHandTag+" lap\nhands lap its token")
	handID, _ := sentID(t, out)

	t.Setenv("AIVOICEBUS_ME", sa)
	t.Setenv("AIVOICEBUS_AGENT", "claude.a1")
	code, out, e := run(t, "", "wait", "--for", "10s")
	if code != 0 || strings.Count(out, "For the person, not for you") != 2 ||
		!strings.Contains(out, "aivoicebus token install "+handID) || !strings.Contains(out, "aivoicebus claim "+personID) {
		t.Fatalf("the first wait: %d\n%s%s", code, out, e)
	}
	if strings.Contains(out, "take it with `aivoicebus claim") || strings.Contains(out, "swap the disk") {
		t.Errorf("the wait offered the person's task to the agent, or showed its words:\n%s", out)
	}
	if code, out, _ := run(t, "", "wait", "--for", "2s"); code != exitTimeout || strings.Contains(out, "For the person") {
		t.Errorf("a second wait told it again: %d\n%s", code, out)
	}
	c, _ := client.FromEnv()
	if ms, _ := c.Peek("lap/human"); !containsMsg(ms, personID) {
		t.Errorf("the person's task is no longer waiting for them: %v", ms)
	}
	if _, who, _ := run(t, "", "who"); strings.Contains(who, "lap/human") {
		t.Errorf("the standby's look made the person a session:\n%s", who)
	}

	// --only: tasks wakes on a task, not on a note; replies on an answer, not on a task
	t.Setenv("AIVOICEBUS_ME", boss)
	run(t, "", "send", "--to", sa, "the build is green")
	_, out, _ = run(t, "", "task", "lap", "run the linters")
	lintID, _ := sentID(t, out)
	t.Setenv("AIVOICEBUS_ME", sa)
	if code, out, _ := run(t, "", "wait", "--only", "tasks", "--for", "10s"); code != 0 || !strings.Contains(out, lintID) || strings.Contains(out, "build is green") {
		t.Errorf("wait --only tasks: %d\n%s", code, out)
	}
	if code, out, _ := run(t, "", "wait", "--only", "replies", "--for", "2s"); code != exitTimeout || strings.Contains(out, "build is green") {
		t.Errorf("wait --only replies woke on a note: %d\n%s", code, out)
	}
	_, out, _ = run(t, "", "send", "--to", boss, "which branch?")
	askID, _ := sentID(t, out)
	t.Setenv("AIVOICEBUS_ME", boss)
	run(t, "", "send", "--re", askID, "main")
	t.Setenv("AIVOICEBUS_ME", sa)
	if code, out, _ := run(t, "", "wait", "--only", "replies", "--for", "10s"); code != 0 || !strings.Contains(out, "main") {
		t.Errorf("wait --only replies: %d\n%s", code, out)
	}
	if code, out, _ := run(t, "", "wait", "--for", "10s"); code != 0 || !strings.Contains(out, "build is green") {
		t.Errorf("the note --only left unread: %d\n%s", code, out)
	}
	if code, _, e := run(t, "", "wait", "--only", "everything"); code != 2 || !strings.Contains(e, "--only all|tasks|replies") {
		t.Errorf("a bad --only: %d %q", code, e)
	}
}

func containsMsg(ms []msg.Message, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}
