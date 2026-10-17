package cli

import (
	"os"
	"os/exec"
	"path/filepath"
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

func TestStatusSaysWhoReadItAndWhoTookIt(t *testing.T) {
	mesh(t)
	present(t, sa)
	present(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "task", "lap", "check the disk")
	id, _ := sentID(t, out)
	if _, st, _ := run(t, "", "status", id); !strings.Contains(st, "not read yet") || !strings.Contains(st, "not taken yet") {
		t.Fatalf("status before anyone read it:\n%s", st)
	}
	t.Setenv("AIVOICEBUS_ME", sb)
	run(t, "", "claim", id, "--settle", "0s")
	run(t, "", "reply", "--re", id, "disk is fine")
	t.Setenv("AIVOICEBUS_ME", boss)
	_, st, _ := run(t, "", "status", id)
	if !strings.Contains(st, "read by 1") || !strings.Contains(st, sb) || !strings.Contains(st, "taken by "+sb) || !strings.Contains(st, "repl") {
		t.Fatalf("status after the claim:\n%s", st)
	}
	if code, _, _ := run(t, "", "status", "0000000000"); code != exitNotFound {
		t.Errorf("unknown id: %d", code)
	}
}

// A wait on a node that holds reads open answers within a second of the message arriving.
func TestWaitIsAnsweredAsSoonAsTheMessageArrives(t *testing.T) {
	mesh(t)
	present(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	sender, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(time.Second)
		sender.Send(msg.Message{From: sb, To: boss, Body: "here", Thread: "t-fast-1"})
	}()
	start := time.Now()
	got, err := waitFor(c, boss, tasks.Filter{Thread: "t-fast-1"}, 20*time.Second, 10*time.Second)
	if err != nil || len(got) != 1 {
		t.Fatalf("wait: %v %v", got, err)
	}
	// It would take up to 10s (the polling interval given) without the held read.
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("the wait took %s: the node did not hold the read open", took)
	}
}

func TestLeasesKeepTwoSessionsOffTheSameThing(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", sa)
	if code, out, e := run(t, "", "lease", "take", "hub:server.go", "--ttl", "30m", "--note", "refactoring", "--settle", "0s"); code != 0 || !strings.Contains(out, "you hold hub:server.go") {
		t.Fatalf("take: %d %q %q", code, out, e)
	}
	t.Setenv("AIVOICEBUS_ME", sb)
	if code, _, e := run(t, "", "lease", "take", "hub:server.go", "--settle", "0s"); code == 0 || !strings.Contains(e, "held by "+sa) || !strings.Contains(e, "refactoring") {
		t.Errorf("a second session took a held lease: %d %q", code, e)
	}
	if _, out, _ := run(t, "", "lease", "list"); !strings.Contains(out, "hub:server.go is held by "+sa) {
		t.Errorf("list: %q", out)
	}
	if code, _, _ := run(t, "", "lease", "release", "hub:server.go"); code != exitNotFound {
		t.Errorf("releasing someone else's lease: %d", code)
	}
	t.Setenv("AIVOICEBUS_ME", sa)
	if code, out, _ := run(t, "", "lease", "release", "hub:server.go"); code != 0 || !strings.Contains(out, "released") {
		t.Fatalf("release: %d %q", code, out)
	}
	t.Setenv("AIVOICEBUS_ME", sb)
	if code, out, e := run(t, "", "lease", "take", "hub:server.go", "--settle", "0s"); code != 0 || !strings.Contains(out, "you hold") {
		t.Errorf("take after the release: %d %q %q", code, out, e)
	}
	// A lease never lands in an inbox.
	t.Setenv("AIVOICEBUS_ME", sa)
	if _, in, _ := run(t, "", "inbox"); strings.Contains(in, "[lease]") {
		t.Errorf("a lease reached an inbox:\n%s", in)
	}
}

func TestTaskRoutingByToolAndCapability(t *testing.T) {
	for _, c := range []struct{ machine, tool, can, model, want string }{
		{"lap", "", "", "", "lap"},
		{"lap", "codex", "", "", "lap/codex"},
		{"", "codex", "", "", "*/codex"},
		{"", "", "gpu", "", "@can=gpu"},
		{"lap", "codex", "", "gpt-*", "@machine=lap,tool=codex,model=gpt-*"},
	} {
		if got, err := taskTarget(c.machine, c.tool, c.can, c.model); err != nil || got != c.want {
			t.Errorf("%+v: %q %v", c, got, err)
		}
	}
	if _, err := taskTarget("", "a,b", "", ""); err == nil {
		t.Error("a value with a comma accepted")
	}
	mesh(t)
	present(t, sa)
	present(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, e := run(t, "", "task", "lap", "--tool", "codex", "run the codex-only check")
	id, _ := sentID(t, out+e)
	t.Setenv("AIVOICEBUS_ME", sa) // claude on lap: not for it
	if _, q, _ := run(t, "", "queue"); strings.Contains(q, id) {
		t.Errorf("a task for codex reached claude:\n%s", q)
	}
	t.Setenv("AIVOICEBUS_ME", sb)
	if _, q, _ := run(t, "", "queue"); !strings.Contains(q, id) {
		t.Errorf("the codex session does not see its task:\n%s", q)
	}
}

func TestHandoffCarriesTheRepoAndTheDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	mesh(t)
	present(t, sb)
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("remote", "add", "origin", "https://user:tok@example.com/acme/app.git")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	git("add", "a.txt")
	git("commit", "-q", "-m", "first")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644)
	t.Chdir(dir)
	t.Setenv("AIVOICEBUS_ME", boss)
	t.Setenv("AIVOICEBUS_FILE_VIA", "node")
	code, out, e := run(t, "", "handoff", "lap/codex.b2", "finish the two-line change", "--done", "tests pass", "--diff")
	if code != 0 {
		t.Fatalf("handoff: %d %q %q", code, out, e)
	}
	t.Setenv("AIVOICEBUS_ME", sb)
	_, in, _ := run(t, "", "inbox")
	for _, want := range []string{"[handoff] finish the two-line change", "repo: https://example.com/acme/app.git", "branch: main", "done when: tests pass", "uncommitted: 1 file(s)", "[task]"} {
		if !strings.Contains(in, want) {
			t.Errorf("handoff missing %q:\n%s", want, in)
		}
	}
	if strings.Contains(in, "tok@") || strings.Contains(in, "user:") {
		t.Errorf("the remote's credentials travelled:\n%s", in)
	}
}

func TestWorkerPresets(t *testing.T) {
	mesh(t)
	if code, _, e := run(t, "", "worker", "--agent", "nope", "--dry-run"); code != exitUsage || !strings.Contains(e, "claude, codex, gemini, grok") {
		t.Errorf("unknown preset: %d %q", code, e)
	}
	if code, _, e := run(t, "", "worker", "--agent", "codex", "--exec", "x", "--dry-run"); code != exitUsage {
		t.Errorf("--agent with --exec: %d %q", code, e)
	}
	if _, _, e := run(t, "", "worker", "--agent", "codex", "--dry-run"); !strings.Contains(e, "codex exec --skip-git-repo-check --sandbox read-only") {
		t.Errorf("preset not shown: %q", e)
	}
}

func TestARoomReachesItsMembersInOneThread(t *testing.T) {
	mesh(t)
	for _, me := range []string{sa, sb} {
		t.Setenv("AIVOICEBUS_ME", me)
		if code, out, e := run(t, "", "room", "join", "release-05"); code != 0 || !strings.Contains(out, "joined release-05") {
			t.Fatalf("join as %s: %d %q %q", me, code, out, e)
		}
	}
	present(t, "desk/gemini.g3") // not a member
	t.Setenv("AIVOICEBUS_ME", sa)
	if _, out, _ := run(t, "", "room", "who", "release-05"); !strings.Contains(out, sa) || !strings.Contains(out, sb) {
		t.Errorf("who: %q", out)
	}
	if code, _, e := run(t, "", "room", "say", "release-05", "tag is cut, run your checks"); code != 0 {
		t.Fatalf("say: %q", e)
	}
	t.Setenv("AIVOICEBUS_ME", sb)
	if code, out, _ := run(t, "", "room", "wait", "release-05", "--for", "3s", "--every", "1s"); code != 0 || !strings.Contains(out, "tag is cut") || !strings.Contains(out, "thread=room-release-05") {
		t.Errorf("member's wait: %d %q", code, out)
	}
	t.Setenv("AIVOICEBUS_ME", "desk/gemini.g3")
	if _, in, _ := run(t, "", "inbox"); strings.Contains(in, "tag is cut") {
		t.Errorf("a non-member got the room's message:\n%s", in)
	}
	t.Setenv("AIVOICEBUS_ME", sb)
	run(t, "", "room", "leave", "release-05")
	if _, out, _ := run(t, "", "room", "list"); strings.Contains(out, "release-05") {
		t.Errorf("still in the room after leaving: %q", out)
	}
}

func TestAPrivateMessageIsSealedForItsMachine(t *testing.T) {
	mesh(t)
	present(t, sb)
	t.Setenv("AIVOICEBUS_ME", sb)
	run(t, "", "send", "--to", boss, "hello") // lap's key becomes known to the mesh with any signed message
	t.Setenv("AIVOICEBUS_ME", boss)
	if code, _, e := run(t, "", "send", "--to", "all", "--private", "x"); code != exitUsage || !strings.Contains(e, "one machine") {
		t.Errorf("private to all: %d %q", code, e)
	}
	if code, out, e := run(t, "", "send", "--to", sb, "--private", "the staging url is http://10.0.0.5"); code != 0 {
		t.Fatalf("private send: %d %q %q", code, out, e)
	}
	t.Setenv("AIVOICEBUS_ME", sb)
	_, in, _ := run(t, "", "inbox")
	if !strings.Contains(in, "[private]") || !strings.Contains(in, "10.0.0.5") {
		t.Errorf("the recipient does not read it:\n%s", in)
	}
	if _, out, _ := run(t, "", "keys"); !strings.Contains(out, "this machine signs with SHA256:") || !strings.Contains(out, "lap") {
		t.Errorf("keys: %q", out)
	}
}
