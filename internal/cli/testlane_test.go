package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// laneMesh is a node, the sender imac/claude.st1 (selftestNode's), and air/codex.b registered as opted in.
func laneMesh(t *testing.T) {
	t.Helper()
	s := selftestNode(t, nil)
	s.InboxFor("air/codex.b", false, msg.Labels{msg.TestLabel: msg.TestOn})
}

func TestSendTestReachesOnlyAnOptedInSessionThatReadsTheLane(t *testing.T) {
	laneMesh(t)
	code, out, e := run(t, "", "send", "--test", "--to", "air/codex.b", "calc 17*23")
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	id, _ := sentID(t, out)
	if !msg.IsTestID(id) || !strings.Contains(e, "test message") {
		t.Errorf("a test id, and the sender is told it is a test: %q %q", id, e)
	}

	t.Setenv("AIVOICEBUS_ME", "air/codex.b")
	t.Setenv("AIVOICEBUS_TEST", "")
	if _, out, _ := run(t, "", "inbox"); !strings.Contains(out, "no unread") {
		t.Errorf("a plain inbox shows nothing of it: %q", out)
	}
	if code, _, e := run(t, "", "inbox", "--test"); code != 2 || !strings.Contains(e, "has not opted in") || !strings.Contains(e, "AIVOICEBUS_TEST=1") {
		t.Errorf("a session that did not opt in is told how: %d %q", code, e)
	}
	t.Setenv("AIVOICEBUS_TEST", "1")
	if _, out, _ := run(t, "", "inbox"); !strings.Contains(out, "no unread") {
		t.Errorf("even opted in, a plain inbox shows nothing of it: %q", out)
	}
	if code, out, e := run(t, "", "inbox", "--test"); code != 0 || !strings.Contains(out, "calc 17*23") {
		t.Errorf("inbox --test shows it: %d %q %q", code, out, e)
	}
	if code, out, e := run(t, "", "thread", "--test", id); code != 0 || !strings.Contains(out, "calc 17*23") {
		t.Errorf("thread --test shows it: %d %q %q", code, out, e)
	}
	if code, _, _ := run(t, "", "thread", id); code == 0 {
		t.Error("a plain thread read finds nothing of a test thread")
	}

	// the answer is a test message with no flag, and the sender reads it the same way
	if code, out, e := run(t, "", "reply", "--re", id, "391"); code != 0 {
		t.Fatalf("reply: %d %q %q", code, out, e)
	} else if rid, _ := sentID(t, out); !msg.IsTestID(rid) {
		t.Errorf("a reply to a test message is one: %s", rid)
	}
	t.Setenv("AIVOICEBUS_ME", "imac/claude.st1")
	if _, out, _ := run(t, "", "inbox"); !strings.Contains(out, "no unread") {
		t.Errorf("the sender's plain inbox shows no answer: %q", out)
	}
	if _, out, _ := run(t, "", "inbox", "--test"); !strings.Contains(out, "391") {
		t.Errorf("the opted-in sender reads the answer through the lane: %q", out)
	}
}

func TestSendTestRefusesWhatCannotWork(t *testing.T) {
	laneMesh(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"a long ttl":         {[]string{"--to", "air/codex.b", "--ttl", "11m", "x"}, "at most 10m"},
		"nobody opted in":    {[]string{"--to", "air/codex.z", "x"}, "opted in"},
		"waiting unopted-in": {[]string{"--to", "air/codex.b", "--wait-reply", "5s", "x"}, "has not opted in"},
	} {
		code, out, e := run(t, "", append([]string{"send", "--test"}, tc.args...)...)
		if code == 0 || !strings.Contains(e, tc.want) || strings.Contains(out, "sent ") {
			t.Errorf("%s: %d %q %q (want %q)", name, code, out, e, tc.want)
		}
	}
	// dry run says what it would do and sends nothing
	if code, out, e := run(t, "", "send", "--test", "--dry-run", "--to", "air/codex.b", "calc 1+1"); code != 0 || !strings.Contains(out, "dry run") {
		t.Errorf("dry run: %d %q %q", code, out, e)
	}
}

func TestACancelledTestTaskIsATombstoneInTheLane(t *testing.T) {
	laneMesh(t)
	_, out, _ := run(t, "", "send", "--test", "--to", "air/codex.b", "sleep 120")
	id, _ := sentID(t, out)
	if code, _, e := run(t, "", "cancel", id); code != 0 {
		t.Fatalf("cancel: %d %s", code, e)
	}
	t.Setenv("AIVOICEBUS_TEST", "1")
	_, out, _ = run(t, "", "thread", "--test", id)
	if strings.Contains(out, "sleep 120") || !strings.Contains(out, "withdrawn") && !strings.Contains(out, "retract") {
		t.Errorf("the lane's thread shows the tombstone, not the words: %q", out)
	}
}

// ── pause --only ─────────────────────────────────────────────────────────────

func TestPauseOnlyNamesExactlyOneSession(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "desk/claude.boss")
	for _, bad := range []string{"desk", "all", "desk/*", "*/claude", "@repo=x", "desk/human", "desk/claude"} {
		code, out, e := run(t, "", "pause", "--only", bad, "why")
		if code != 2 || !strings.Contains(e, "exactly one session") || strings.Contains(out, "paused") {
			t.Errorf("--only %s: %d %q %q", bad, code, out, e)
		}
	}
	if code, _, e := run(t, "", "pause", "--only"); code != 2 || !strings.Contains(e, "usage: pause") {
		t.Errorf("--only without a value: %d %q", code, e)
	}
	if code, _, e := run(t, "", "pause", "--only", "lap/claude.one"); code != 2 || !strings.Contains(e, "usage: pause") {
		t.Errorf("--only without a reason: %d %q", code, e)
	}

	// one session is paused, the other one on its machine is not
	present(t, "lap/claude.one")
	present(t, "lap/claude.two")
	t.Setenv("AIVOICEBUS_ME", "desk/claude.boss")
	code, out, e := run(t, "", "pause", "--only", "lap/claude.one", "--for", "30m", "testing the switch")
	if code != 0 || !strings.Contains(out, "paused lap/claude.one") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	pausedFor := func(me string) bool {
		t.Setenv("AIVOICEBUS_ME", me)
		code, out, _ := run(t, "", "inbox", "--json")
		if code != 0 {
			t.Fatalf("inbox as %s: %d", me, code)
		}
		var ms []msg.Message
		_ = json.Unmarshal([]byte(out), &ms)
		_, _, on := msg.ActivePause(ms, time.Now())
		return on
	}
	if !pausedFor("lap/claude.one") {
		t.Error("the named session is paused")
	}
	if pausedFor("lap/claude.two") {
		t.Error("another session of the same machine is paused too: --only must stop one session, not the machine")
	}
	// the worker of a machine is one session too
	t.Setenv("AIVOICEBUS_ME", "desk/claude.boss")
	if code, _, e := run(t, "", "pause", "--only", "lap/worker", "--for", "5m", "worker check"); code != 0 {
		t.Errorf("a machine's worker is named by host/worker: %d %q", code, e)
	}
}

func TestPauseOnlyAndTestLetAMessageThroughOnlyToTheLane(t *testing.T) {
	laneMesh(t)
	if code, _, e := run(t, "", "pause", "--only", "air/codex.b", "--test", "--for", "30m", "too long for a test"); code != 2 || !strings.Contains(e, "at most 10m") {
		t.Errorf("a test pause is at most ten minutes: %d %q", code, e)
	}
	code, out, e := run(t, "", "pause", "--only", "air/codex.b", "--test", "--for", "2m", "a test pause")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, e)
	}
	t.Setenv("AIVOICEBUS_ME", "air/codex.b")
	t.Setenv("AIVOICEBUS_TEST", "1")
	pausedBy := func(args ...string) bool {
		_, out, _ := run(t, "", append([]string{"inbox", "--json"}, args...)...)
		var ms []msg.Message
		_ = json.Unmarshal([]byte(out), &ms)
		_, _, on := msg.ActivePause(ms, time.Now())
		return on
	}
	if pausedBy() {
		t.Error("a plain read never sees a test pause: a session's hook must not be stopped by one")
	}
	if !pausedBy("--test") {
		t.Error("a read that asks for the lane sees it: that is how a worker that opted in is paused")
	}
}

func TestResumeOnlyNamesOneSessionToo(t *testing.T) {
	mesh(t)
	old := readPassphrase
	t.Cleanup(func() { readPassphrase = old })
	readPassphrase = func(string) (string, error) { return "resume", nil }
	t.Setenv("AIVOICEBUS_ME", "desk/claude.boss")
	if code, _, e := run(t, "", "resume", "--only", "desk"); code != 2 || !strings.Contains(e, "exactly one session") {
		t.Errorf("%d %q", code, e)
	}
	if code, out, e := run(t, "", "resume", "--only", "lap/claude.one"); code != 0 || !strings.Contains(out, "resumed") {
		t.Errorf("%d %q %q", code, out, e)
	}
}

// ── export ───────────────────────────────────────────────────────────────────

func TestExportWritesATestMessageAsItsStubSoTheNumberingHoldsAndTheWordsDoNot(t *testing.T) {
	laneMesh(t)
	run(t, "", "send", "--to", "air/codex.b", "an ordinary message")
	_, out, _ := run(t, "", "send", "--test", "--to", "air/codex.b", "words of a test that must not be exported")
	id, _ := sentID(t, out)
	dest := filepath.Join(t.TempDir(), "board.jsonl")
	if code, _, e := run(t, "", "export", "--out", dest); code != 0 {
		t.Fatalf("export: %d %s", code, e)
	}
	b, _ := os.ReadFile(dest)
	if strings.Contains(string(b), "words of a test") || !strings.Contains(string(b), "an ordinary message") {
		t.Errorf("export:\n%s", b)
	}
	var stub msg.Message
	var n int
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r msg.Record
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("a line is not a record: %q", l)
		}
		n++
		if r.Msg != nil && r.Msg.ID == id {
			stub = *r.Msg
		}
	}
	if n != 2 || stub.ID != id || stub.Body != "" || stub.From == "" || stub.To != "air/codex.b" || !msg.TestPurged(stub) {
		t.Errorf("the test record is there, as its stub: %d records, %+v", n, stub)
	}
}

// ── the worker flags ─────────────────────────────────────────────────────────

func TestWorkerTestLaneFlagOptsTheWorkerIn(t *testing.T) {
	workerSetup(t, "desk")
	t.Setenv("AIVOICEBUS_TEST", "")
	code, _, e := runWorker(t, "--test-lane", "--exec", "/bin/cat")
	if code != 0 || !strings.Contains(e, "test lane on") {
		t.Errorf("%d %q", code, e)
	}
	if os.Getenv("AIVOICEBUS_TEST") != "1" {
		t.Error("the flag is the opt-in the labels and the lane read follow")
	}
	t.Setenv("AIVOICEBUS_TEST", "")
	if _, _, e := runWorker(t, "--exec", "/bin/cat"); strings.Contains(e, "test lane on") {
		t.Errorf("without the flag the worker is not opted in: %q", e)
	}
}

func TestWorkerInstallCarriesTheTestLaneFlagIntoTheService(t *testing.T) {
	mesh(t)
	home, _ := os.UserHomeDir()
	os.MkdirAll(home+"/.local/bin", 0o755)
	os.WriteFile(home+"/.local/bin/aivoicebus", []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("AIVOICEBUS_ME", "desk/claude.boss")
	code, out, e := run(t, "", "worker", "install", "--exec", "/bin/cat", "--test-lane", "--dry-run")
	if code != 0 || !strings.Contains(out, "--test-lane") {
		t.Errorf("%d %q %q", code, out, e)
	}
	_, out, _ = run(t, "", "worker", "install", "--exec", "/bin/cat", "--dry-run")
	if strings.Contains(out, "--test-lane") {
		t.Errorf("not unless asked: %q", out)
	}
}
