package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
)

const boss, sa, sb = "desk/claude.z9", "lap/claude.a1", "lap/codex.b2"

func sentID(t *testing.T, out string) (id, thread string) {
	t.Helper()
	f := strings.Fields(out)
	if len(f) < 4 || f[0] != "sent" {
		t.Fatalf("not a send: %q", out)
	}
	return f[1], f[3]
}

func TestATaskToAMachineIsTakenByOneSession(t *testing.T) {
	mesh(t)
	present(t, sa)
	present(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "task", "lap", "send me the screenshot on the Desktop")
	if code != 0 {
		t.Fatalf("task: %d %s", code, e)
	}
	id, _ := sentID(t, out)

	t.Setenv("AIVOICEBUS_ME", sa)
	if _, q, _ := run(t, "", "queue"); !strings.Contains(q, "[task]") || !strings.Contains(q, "aivoicebus claim "+id) {
		t.Fatalf("queue on %s:\n%s", sa, q)
	}
	t.Setenv("AIVOICEBUS_ME", sb)
	if code, out, e := run(t, "", "claim", id, "--settle", "0s"); code != 0 || !strings.Contains(out, "is yours") {
		t.Fatalf("claim by %s: %d %s %s", sb, code, out, e)
	}
	if _, q, _ := run(t, "", "queue"); strings.Contains(q, id) {
		t.Errorf("the owner's queue still lists the task it claimed (it is acknowledged): %s", q)
	}
	t.Setenv("AIVOICEBUS_ME", sa)
	if _, q, _ := run(t, "", "queue"); strings.Contains(q, id) {
		t.Errorf("the task is still queued for a session that did not take it:\n%s", q)
	}
	if code, _, e := run(t, "", "claim", id, "--settle", "0s"); code == 0 || !strings.Contains(e, "already taken by "+sb) {
		t.Errorf("a second claim: %d %q", code, e)
	}
	t.Setenv("AIVOICEBUS_ME", boss)
	if _, in, _ := run(t, "", "inbox"); !strings.Contains(in, "[claim]") || !strings.Contains(in, sb) {
		t.Errorf("the sender is not told who took it:\n%s", in)
	}
	if code, _, e := run(t, "", "claim", id); code == 0 || !strings.Contains(e, "you sent task") {
		t.Errorf("the sender claiming its own task: %d %q", code, e)
	}
}

func TestWaitConversationsAndEnd(t *testing.T) {
	mesh(t)
	present(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	// Nothing comes: --wait-reply gives up with exit 6 and says how to wait later.
	code, out, e := run(t, "", "send", "--to", sb, "--wait-reply", "1s", "did the image arrive?")
	if code != exitTimeout || !strings.Contains(e, "aivoicebus wait --thread") {
		t.Fatalf("wait-reply with no answer: %d %q", code, e)
	}
	q, thread := sentID(t, out)

	t.Setenv("AIVOICEBUS_ME", sb)
	if code, _, e := run(t, "", "reply", "--re", q, "yes, saved to ~/Desktop"); code != 0 {
		t.Fatalf("reply: %s", e)
	}
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, _ = run(t, "", "wait", "--thread", thread, "--for", "5s", "--every", "1s")
	if code != 0 || !strings.Contains(out, "saved to ~/Desktop") || !strings.Contains(out, "UNTRUSTED") || !strings.Contains(out, "--wait-reply") {
		t.Fatalf("wait: %d\n%s", code, out)
	}
	// What a wait showed is read: waiting again finds nothing.
	if code, _, _ := run(t, "", "wait", "--thread", thread, "--for", "1s"); code != exitTimeout {
		t.Errorf("a second wait returned what the first showed: %d", code)
	}
	// The other side ends it, and the wait says so.
	t.Setenv("AIVOICEBUS_ME", sb)
	if code, _, e := run(t, "", "reply", "--re", q, "--end", "done here"); code != 0 {
		t.Fatalf("reply --end: %s", e)
	}
	if code, _, _ := run(t, "", "reply", "--re", q, "--end", "--wait-reply", "1m", "x"); code != exitUsage {
		t.Errorf("--end with --wait-reply: %d", code)
	}
	t.Setenv("AIVOICEBUS_ME", boss)
	if _, out, _ := run(t, "", "wait", "--from", "lap", "--for", "5s", "--every", "1s"); !strings.Contains(out, "[end]") || !strings.Contains(out, "ended the conversation") {
		t.Errorf("wait on an end:\n%s", out)
	}
}

func TestPingSaysWhoIsThere(t *testing.T) {
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	if code, out, _ := run(t, "", "ping", "lap"); code != 0 || !strings.Contains(out, sa) || !strings.Contains(out, "seen") {
		t.Errorf("ping lap: %d %q", code, out)
	}
	if code, out, _ := run(t, "", "ping", "nowhere"); code != exitNotFound || !strings.Contains(out, "aivoicebus task nowhere") {
		t.Errorf("ping nowhere: %d %q", code, out)
	}
}

// Waiting for the answer to a task: the claim is progress, not the answer.
func TestWaitingForATaskAnswerSkipsTheClaim(t *testing.T) {
	mesh(t)
	present(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "task", "lap", "resize the image")
	id, thread := sentID(t, out)
	t.Setenv("AIVOICEBUS_ME", sb)
	run(t, "", "claim", id, "--settle", "0s", "on it")
	run(t, "", "reply", "--re", id, "done: 800x600")
	t.Setenv("AIVOICEBUS_ME", boss)
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	var progress []string
	got, err := waitFor(c, boss, tasks.Filter{Thread: thread, Progress: func(m msg.Message) { progress = append(progress, m.Body) }}, 3*time.Second, time.Second)
	if err != nil || len(got) != 1 || got[0].Body != "done: 800x600" {
		t.Fatalf("wait returned %+v %v", got, err)
	}
	if len(progress) != 1 || progress[0] != "on it" {
		t.Errorf("the claim was not reported as progress: %q", progress)
	}
}

// Flags after the text are flags, not text: `task lap "..." --wait-reply 10m` (the form the usage
// line shows) must wait, and must not send "--wait-reply 10m" as part of the task.
func TestFlagsAfterTheTextAreFlags(t *testing.T) {
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "task", "lap", "resize the image", "--wait-reply", "1s", "--ttl", "1h")
	if code != exitTimeout || !strings.Contains(e, "aivoicebus wait --thread") {
		t.Fatalf("task with trailing --wait-reply: %d %q %q", code, out, e)
	}
	id, _ := sentID(t, out)
	if _, th, _ := run(t, "", "thread", id); !strings.Contains(th, "resize the image\n") || strings.Contains(th, "--wait-reply") || strings.Contains(th, "--ttl") {
		t.Errorf("the flags went into the text:\n%s", th)
	}
	// Words that are not this command's flags stay text, and -- keeps the rest as text.
	_, out, _ = run(t, "", "send", "--to", sa, "set", "the", "offset", "to", "-5", "--", "--wait-reply", "is", "a", "flag")
	id, _ = sentID(t, out)
	if _, th, _ := run(t, "", "thread", id); !strings.Contains(th, "set the offset to -5 --wait-reply is a flag") {
		t.Errorf("text with dashes:\n%s", th)
	}
	if code, _, e := run(t, "", "send", "--to", sa, "before", "--prio", "fyi", "after"); code != exitUsage || !strings.Contains(e, "both sides") {
		t.Errorf("text on both sides: %d %q", code, e)
	}
}
