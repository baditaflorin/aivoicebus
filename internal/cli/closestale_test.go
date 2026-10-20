package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// thread ID --close-stale ends a conversation nobody has written in for a while, with a summary,
// but only when its sender or the person asks, and only when it is stale and still open.
func TestCloseStaleEndsAQuietConversationWithASummary(t *testing.T) {
	mesh(t)
	present(t, sa)
	present(t, sb)
	t.Cleanup(func() { staleNow = time.Now })
	t.Setenv("AIVOICEBUS_ME", boss)
	t.Setenv("AIVOICEBUS_AGENT", "claude.z9") // an agent's session
	_, out, _ := run(t, "", "task", "lap", "check the disk on lap")
	id, _ := sentID(t, out)
	t.Setenv("AIVOICEBUS_ME", sb)
	t.Setenv("AIVOICEBUS_AGENT", "codex.b2")
	run(t, "", "claim", id, "--settle", "0s")
	run(t, "", "state", id, "doing", "looking at the disk now")
	run(t, "", "reply", "--re", id, "disk is 91% full, looking at what fills it")

	if code, _, e := run(t, "", "thread", id, "--close-stale"); code == 0 || !strings.Contains(e, "only the conversation's sender") {
		t.Fatalf("another agent closed it: %d %s", code, e)
	}
	t.Setenv("AIVOICEBUS_ME", boss)
	t.Setenv("AIVOICEBUS_AGENT", "claude.z9")
	if code, _, e := run(t, "", "thread", id, "--close-stale"); code == 0 || !strings.Contains(e, "is not stale") {
		t.Fatalf("closed before it was stale: %d %s", code, e)
	}
	staleNow = func() time.Time { return time.Now().Add(31 * time.Minute) }
	code, out, e := run(t, "", "thread", id, "--close-stale")
	if code != 0 || !strings.Contains(out, "closed thread "+id+" as stale") {
		t.Fatalf("the sender closing a stale thread: %d %s %s", code, out, e)
	}
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	ms, err := c.Thread(id)
	if err != nil {
		t.Fatal(err)
	}
	end := ms[len(ms)-1]
	for _, want := range []string{"[closed as stale] no message in this conversation for 31m", "closed by the conversation's sender", "size: 4 message(s)",
		"state: doing", "last from " + sb, "disk is 91% full", "aivoicebus send"} {
		if end.Kind != msg.KindEnd || end.From != boss || end.To != sb || !strings.Contains(end.Body, want) {
			t.Errorf("the [end] lacks %q: %+v", want, end)
		}
	}
	if code, _, e := run(t, "", "thread", id, "--close-stale"); code == 0 || !strings.Contains(e, "already ended") {
		t.Errorf("closed twice: %d %s", code, e)
	}

	// the person, at their own terminal, may close anyone's; a conversation that is done is closed already
	staleNow = time.Now
	t.Setenv("AIVOICEBUS_ME", sa)
	_, out, _ = run(t, "", "send", "--to", sb, "can you look at the logs too?")
	other, _ := sentID(t, out)
	t.Setenv("AIVOICEBUS_ME", boss)
	t.Setenv("AIVOICEBUS_AGENT", "")
	staleNow = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if code, out, e := run(t, "", "thread", other, "--close-stale", "--stale-after", "1h"); code != 0 || !strings.Contains(out, "as stale") {
		t.Fatalf("the person closing someone's stale thread: %d %s %s", code, out, e)
	}
	if ms, _ := c.Thread(other); !strings.Contains(ms[len(ms)-1].Body, "closed by the user") {
		t.Errorf("who closed it: %q", ms[len(ms)-1].Body)
	}
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ = run(t, "", "task", "lap", "a quick one")
	done, _ := sentID(t, out)
	t.Setenv("AIVOICEBUS_ME", sb)
	run(t, "", "claim", done, "--settle", "0s")
	run(t, "", "state", done, "done", "finished")
	t.Setenv("AIVOICEBUS_ME", boss)
	if code, _, e := run(t, "", "thread", done, "--close-stale"); code == 0 || !strings.Contains(e, "closed already: done") {
		t.Errorf("a done conversation: %d %s", code, e)
	}
	if code, _, e := run(t, "", "thread", done, "--close-stale", "--digest"); code != exitUsage {
		t.Errorf("--close-stale with --digest: %d %s", code, e)
	}
}
