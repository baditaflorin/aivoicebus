package cli

import (
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// --done-when goes in the message as a trailer line, from a fixed list of conditions, and an unknown
// one is refused at the sender with nothing sent.
func TestDoneWhenOnSendAndTask(t *testing.T) {
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	c, _ := client.FromEnv()

	code, out, e := run(t, "", "send", "--to", "lap", "--done-when", "release>=0.15.0", "update lap")
	if code != 0 {
		t.Fatalf("send: %d %s", code, e)
	}
	id, _ := sentID(t, out)
	ms, _ := c.Thread(id)
	if len(ms) != 1 || ms[0].Body != "update lap\n[done-when: release>=0.15.0]" {
		t.Errorf("the trailer: %+v", ms)
	}

	code, out, e = run(t, "", "task", "lap", "install the collector", "--done-when", "file:~/.local/bin/agent-tail")
	if code != 0 {
		t.Fatalf("task: %d %s", code, e)
	}
	tid, _ := sentID(t, out)
	ms, _ = c.Thread(tid)
	if len(ms) != 1 || ms[0].Kind != msg.KindTask || !strings.HasSuffix(ms[0].Body, "\n[done-when: file:~/.local/bin/agent-tail]") {
		t.Errorf("task: %+v", ms)
	}

	for _, bad := range []string{"exec:id", "file:/etc/passwd", "release>=1.2", "service:a;b", "$(id)"} {
		code, out, e := run(t, "", "send", "--to", "lap", "--done-when", bad, "x")
		if code != exitUsage || !strings.Contains(e, "the conditions are release>=X.Y.Z") || strings.Contains(out, "sent") {
			t.Errorf("%q: %d %q %q", bad, code, out, e)
		}
	}
	if code, _, e := run(t, "", "send", "--to", "lap", "--private", "--done-when", "release>=0.15.0", "x"); code != exitUsage || !strings.Contains(e, "--private") {
		t.Errorf("--private: %d %q", code, e)
	}
}

// --key and --supersedes: a later message replaces an earlier one for every session that has not read it.
func TestKeyAndSupersedesReplaceTheEarlierMessage(t *testing.T) {
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "send", "--to", "lap", "--key", "update-lap", "update lap to 0.15.0")
	first, _ := sentID(t, out)
	_, out, _ = run(t, "", "send", "--to", "lap", "plain note about the router")
	plain, _ := sentID(t, out)
	if got := inboxIDs(t, "lap/claude.new1"); !strings.Contains(got, first) || !strings.Contains(got, plain) {
		t.Fatalf("shown before: %q", got)
	}
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ = run(t, "", "send", "--to", "lap", "--key", "update-lap", "update lap to 0.15.1")
	second, _ := sentID(t, out)
	code, out, e := run(t, "", "send", "--to", "lap", "--supersedes", plain, "the router is fixed, ignore the note")
	if code != 0 {
		t.Fatalf("supersedes: %d %s", code, e)
	}
	third, _ := sentID(t, out)
	got := inboxIDs(t, "lap/codex.new2")
	if strings.Contains(got, first) || strings.Contains(got, plain) || !strings.Contains(got, second) || !strings.Contains(got, third) {
		t.Errorf("only the replacements are shown: %q", got)
	}
	c, _ := client.FromEnv()
	if ms, _ := c.Thread(first); len(ms) != 1 {
		t.Error("the replaced message stays readable")
	}
	t.Setenv("AIVOICEBUS_ME", boss)
	for _, bad := range [][]string{{"--key", "has space"}, {"--supersedes", "short"}, {"--supersedes", "0123456789", "--private"}} {
		args := append([]string{"send", "--to", "lap"}, bad...)
		if code, _, _ := run(t, "", append(args, "x")...); code != exitUsage {
			t.Errorf("%v: %d", bad, code)
		}
	}
	t.Setenv("AIVOICEBUS_ME", "imac/claude.other") // another machine
	if code, _, e := run(t, "", "send", "--to", "lap", "--supersedes", first, "mine now"); code != exitUsage || !strings.Contains(e, "only a message this machine sent") {
		t.Errorf("another machine's message: %d %q", code, e)
	}
}
