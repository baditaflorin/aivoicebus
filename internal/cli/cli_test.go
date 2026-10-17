package cli

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// run executes one invocation with captured streams.
func run(t *testing.T, stdin string, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e strings.Builder
	code = Run(args, Streams{In: strings.NewReader(stdin), Out: &o, Err: &e})
	return code, o.String(), e.String()
}

// mesh starts one node and points the CLI at it.
func mesh(t *testing.T) {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, "tok"))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir()) // no real ~/.config/aivoicebus/env
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_AGENT", "")
}

func TestUsageAndUnknownCommand(t *testing.T) {
	if code, _, e := run(t, ""); code != 2 || !strings.Contains(e, "usage: aivoicebus") {
		t.Errorf("no args: code=%d err=%q", code, e)
	}
	if code, _, e := run(t, "", "frobnicate"); code != 1 || !strings.Contains(e, `error: unknown command "frobnicate"`) {
		t.Errorf("unknown: code=%d err=%q", code, e)
	}
}

func TestMissingURLExitsTwo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", "")
	code, _, e := run(t, "", "who")
	if code != 2 || !strings.Contains(e, "AIVOICEBUS_URL not set") {
		t.Errorf("code=%d err=%q", code, e)
	}
}

func TestFlagErrorsAndHelp(t *testing.T) {
	if code, _, _ := run(t, "", "send", "--bogus"); code != 2 {
		t.Errorf("bad flag: code=%d, want 2", code)
	}
	if code, _, _ := run(t, "", "send", "-h"); code != 0 {
		t.Errorf("-h: code=%d, want 0", code)
	}
}

func TestSendInboxAckWhoEndToEnd(t *testing.T) {
	mesh(t)

	t.Setenv("AIVOICEBUS_ME", "imac/claude")
	if code, out, e := run(t, "", "whoami"); code != 0 || strings.TrimSpace(out) != "imac/claude" {
		t.Fatalf("whoami: %d %q %q", code, out, e)
	}
	code, out, e := run(t, "", "send", "--to", "air/codex", "hello", "there")
	if code != 0 || !strings.HasPrefix(out, "sent ") {
		t.Fatalf("send: %d %q %q", code, out, e)
	}
	if code, out, _ := run(t, "from stdin\n", "send", "--to", "air/codex", "-"); code != 0 || !strings.HasPrefix(out, "sent ") {
		t.Fatalf("send via stdin: %d %q", code, out)
	}
	if code, _, e := run(t, "", "send", "--to", "air/codex"); code != exitUsage || !strings.Contains(e, "need --to and a body") {
		t.Fatalf("empty body: %d %q", code, e)
	}

	t.Setenv("AIVOICEBUS_ME", "air/codex")
	code, out, _ = run(t, "", "inbox", "--ack")
	for _, want := range []string{"UNTRUSTED DATA", "hello there", "from stdin", "imac/claude -> air/codex"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("inbox missing %q (code %d):\n%s", want, code, out)
		}
	}
	if _, out, _ = run(t, "", "inbox"); !strings.Contains(out, "(no unread messages)") {
		t.Fatalf("after --ack the inbox should be empty:\n%s", out)
	}
	if _, out, _ = run(t, "", "inbox", "--all"); !strings.Contains(out, "hello there") {
		t.Fatalf("--all should still list acked messages:\n%s", out)
	}
	if _, out, _ = run(t, "", "who"); !strings.Contains(out, "imac/claude") || !strings.Contains(out, "air/codex") {
		t.Fatalf("who:\n%s", out)
	}
}

func TestThreadAndReply(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "imac/claude")
	_, out, _ := run(t, "", "send", "--to", "air/codex", "question?")
	id := strings.Fields(out)[1]
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	if code, _, e := run(t, "", "send", "--to", "imac/claude", "--re", id, "answer"); code != 0 {
		t.Fatalf("reply: %d %q", code, e)
	}
	_, out, _ = run(t, "", "thread", id)
	if !strings.Contains(out, "question?") || !strings.Contains(out, "answer") {
		t.Fatalf("thread should hold both messages:\n%s", out)
	}
	if !strings.HasPrefix(out, view.UntrustedBanner+"\n[aivoicebus] Thread subject (peer text): \"question?\"\n--- ") {
		t.Fatalf("thread opens with the banner and the subject:\n%s", out)
	}
	if code, _, e := run(t, "", "thread"); code != exitUsage || !strings.Contains(e, "usage: thread <id>") {
		t.Fatalf("thread without id: %d %q", code, e)
	}
}

func TestHookCommandRunsThroughTheCLI(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "imac/claude")
	run(t, "", "send", "--to", "air/codex", "ping")
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	code, out, _ := run(t, "", "hook", "--tool", "codex")
	if code != 0 || !strings.Contains(out, "1 unread message(s)") || !strings.Contains(out, "ping") {
		t.Fatalf("hook: %d\n%s", code, out)
	}
}

func TestVersionAndUsage(t *testing.T) {
	if code, out, _ := run(t, "", "version"); code != 0 || !strings.HasPrefix(out, "aivoicebus dev (commit ") {
		t.Errorf("version: %d %q", code, out)
	}
	code, out, _ := run(t, "", "usage")
	for _, want := range []string{"aivoicebus send --to", "untrusted data", "aivoicebus doctor"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("usage missing %q:\n%s", want, out)
		}
	}
}

func TestSetupAndDoctorThroughTheCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_URL", "")
	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_ME", "imac/claude")
	envFile := filepath.Join(home, ".config/aivoicebus/env")

	code, out, _ := run(t, "", "setup", "--dry-run", "--url", "http://10.0.0.1:7777")
	if code != 0 || !strings.Contains(out, "would change") {
		t.Fatalf("dry run: %d\n%s", code, out)
	}
	if _, err := os.Stat(envFile); err == nil {
		t.Fatal("dry run wrote the env file")
	}

	if code, out, e := run(t, "", "setup", "--yes", "--url", "http://10.0.0.1:7777"); code != 0 || !strings.Contains(out, "changed") {
		t.Fatalf("setup: %d\n%s\n%s", code, out, e)
	}
	if b, _ := os.ReadFile(envFile); !strings.Contains(string(b), "AIVOICEBUS_URL='http://10.0.0.1:7777'") {
		t.Fatalf("env file: %q", b)
	}

	// setup installed the binary the hooks run; there is no token yet: doctor must fail on that alone
	if fi, err := os.Stat(filepath.Join(home, ".local/bin/aivoicebus")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("setup did not install the binary: %v", err)
	}
	code, out, _ = run(t, "", "doctor", "--offline")
	if code != 1 || strings.Contains(out, "FAIL  install") || !strings.Contains(out, "FAIL  token") {
		t.Fatalf("doctor on an unfinished install: %d\n%s", code, out)
	}
	// provide them; doctor now passes (the test binary is not the installed one, which only warns)
	os.MkdirAll(filepath.Join(home, ".local/bin"), 0o755)
	os.WriteFile(filepath.Join(home, ".local/bin/aivoicebus"), []byte("x"), 0o755)
	os.WriteFile(filepath.Join(home, ".config/aivoicebus/token"), []byte(strings.Repeat("t", 40)), 0o600)
	code, out, _ = run(t, "", "doctor", "--offline")
	if code != 0 || !strings.Contains(out, "imac/claude") || strings.Contains(out, "FAIL") {
		t.Fatalf("doctor after install: %d\n%s", code, out)
	}
	// --fix repairs drift
	os.WriteFile(filepath.Join(home, ".claude/settings.json"), []byte(`{"hooks":{}}`), 0o644)
	if code, _, _ = run(t, "", "doctor", "--offline"); code != 1 {
		t.Fatal("doctor should fail when the hook is gone")
	}
	if code, out, _ = run(t, "", "doctor", "--offline", "--fix"); code != 0 || strings.Contains(out, "FAIL") {
		t.Fatalf("doctor --fix: %d\n%s", code, out)
	}
}

func TestDoctorChecksTheMesh(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "imac/claude")
	_, out, _ := run(t, "", "doctor")
	if !strings.Contains(out, "mesh") || !strings.Contains(out, "reachable") {
		t.Errorf("online doctor should probe the mesh:\n%s", out)
	}
}

func TestNameRegistersAChosenAgentName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_HOST", "laptop")
	if code, out, _ := run(t, "", "name"); code != 0 || !strings.Contains(out, "no long-lived names") {
		t.Errorf("%d %q", code, out)
	}
	if code, out, e := run(t, "", "name", "add", "Codex.Build1"); code != 0 || !strings.Contains(out, "laptop/codex.build1") || !strings.Contains(out, "AIVOICEBUS_NAME=codex.build1") {
		t.Errorf("%d %q %q", code, out, e)
	}
	if code, out, _ := run(t, "", "name", "list"); code != 0 || strings.TrimSpace(out) != "laptop/codex.build1" {
		t.Errorf("%d %q", code, out)
	}
	if code, _, e := run(t, "", "name", "add", "codex.a1b2"); code != exitUsage || !strings.Contains(e, "at least") {
		t.Errorf("an automatic-shaped name is refused: %d %q", code, e)
	}
	if code, out, _ := run(t, "", "name", "rename", "codex.build1", "codex.build2"); code != 0 || !strings.Contains(out, "stay addressed to it") {
		t.Errorf("%d %q", code, out)
	}
	if code, out, _ := run(t, "", "name", "list"); strings.TrimSpace(out) != "laptop/codex.build2" || code != 0 {
		t.Errorf("%q", out)
	}
	if code, _, e := run(t, "", "name", "rm", "codex.build1"); code != exitNotFound || !strings.Contains(e, "not registered") {
		t.Errorf("%d %q", code, e)
	}
	if code, _, _ := run(t, "", "name", "rm", "codex.build2"); code != 0 {
		t.Error("rm")
	}
	if code, _, e := run(t, "", "name", "bogus"); code != exitUsage || !strings.Contains(e, "usage: name") {
		t.Errorf("%d %q", code, e)
	}
}

func TestRolePinWritesASafeFileAndRefusesAnUnsafePin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, pin := range []string{"*", "*/claude.*", "claude.*"} {
		if code, _, e := run(t, "", "role", "pin", "build", pin); code != exitUsage || e == "" {
			t.Errorf("%q: %d %q", pin, code, e)
		}
	}
	if code, _, e := run(t, "", "role", "pin", "repo", "build-*/claude.*"); code != exitUsage || !strings.Contains(e, "not a role name") {
		t.Errorf("%d %q", code, e)
	}
	run(t, "", "role", "pin", "build", "build-*/claude.*")
	run(t, "", "role", "pin", "build", "build-*/claude.*") // twice is one line
	run(t, "", "role", "pin", "build", "laptop/codex.build1")
	path := os.Getenv("HOME") + "/.config/aivoicebus/roles"
	b, _ := os.ReadFile(path)
	if string(b) != "build=build-*/claude.*\nbuild=laptop/codex.build1\n" {
		t.Errorf("%q", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("%v", fi.Mode())
	}
	if code, _, e := run(t, "", "role", "unpin", "lint"); code != exitNotFound || !strings.Contains(e, "not pinned") {
		t.Errorf("%d %q", code, e)
	}
	if code, _, _ := run(t, "", "role", "bogus"); code != exitUsage {
		t.Error("usage")
	}
}
