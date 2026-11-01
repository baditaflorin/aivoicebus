package cli

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/term"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

var escapes = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestTakeColourReadsTheGlobalFlags(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		rest []string
		mode term.Mode
	}{
		{[]string{"queue"}, []string{"queue"}, term.Auto},
		{[]string{"--color=always", "queue", "--json"}, []string{"queue", "--json"}, term.Always},
		{[]string{"--color", "never", "inbox"}, []string{"inbox"}, term.Never},
		{[]string{"--no-color", "who"}, []string{"who"}, term.Never},
		{[]string{"--color=never", "--color=always", "who"}, []string{"who"}, term.Always},
		{[]string{"queue", "--color=always"}, []string{"queue", "--color=always"}, term.Auto}, // after the command it is the command's own flag
	} {
		rest, mode, err := takeColour(tc.in)
		if err != nil || mode != tc.mode || strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
			t.Errorf("%v: rest %v mode %v err %v", tc.in, rest, mode, err)
		}
	}
	for _, bad := range [][]string{{"--color=rainbow", "queue"}, {"--color"}, {"--color", "pink", "queue"}} {
		if _, _, err := takeColour(bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if code, _, e := run(t, "", "--color=rainbow", "queue"); code != exitUsage || !strings.Contains(e, "auto, always or never") {
		t.Errorf("a bad mode: %d %q", code, e)
	}
}

// board sends an urgent task to lap and a note to it, and returns the id of the task.
func colourBoard(t *testing.T) string {
	t.Helper()
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "task", "--prio", "urgent", "lap", "run the redirect test")
	id, _ := sentID(t, out)
	run(t, "", "send", "--to", sa, "got a minute?")
	t.Setenv("AIVOICEBUS_ME", sa)
	return id
}

// Into a buffer (a pipe, a file, a test) the output is the plain text, whatever the environment
// would otherwise allow; with colour forced it is that same text with escapes added, and nothing else.
func TestColourOnlyAddsEscapesToWhatAPipeGets(t *testing.T) {
	id := colourBoard(t)
	for _, args := range [][]string{{"queue"}, {"inbox", "--all"}, {"thread", id}, {"thread", id, "--digest"}, {"who", "--all"}, {"status"}, {"status", id}} {
		t.Setenv("AIVOICEBUS_COLOR", "")
		if args[0] == "status" && len(args) == 1 {
			continue // the home screen has timings and local state: compared below on its shape
		}
		_, plain, _ := run(t, "", args...)
		if strings.Contains(plain, "\x1b") {
			t.Errorf("%v: a buffer got an escape: %q", args, plain)
		}
		t.Setenv("AIVOICEBUS_COLOR", "never")
		if _, got, _ := run(t, "", args...); got != plain {
			t.Errorf("%v: --color never changed the text", args)
		}
		t.Setenv("AIVOICEBUS_COLOR", "always")
		_, col, _ := run(t, "", args...)
		if args[0] != "who" || !strings.Contains(plain, "seen") { // who prints times that move on
			if stripped := escapes.ReplaceAllString(col, ""); stripped != plain {
				t.Errorf("%v: colour changed the text:\n--- without escapes\n%s\n--- plain\n%s", args, stripped, plain)
			}
		}
		if !strings.Contains(col, "\x1b[") {
			t.Errorf("%v: --color always drew nothing:\n%s", args, col)
		}
	}
}

func TestQueueInColourMarksWhatToTypeAndWhatToDistrust(t *testing.T) {
	id := colourBoard(t)
	_, out, _ := run(t, "", "--color=always", "queue")
	for _, want := range []string{
		"\x1b[1;33m" + view.UntrustedBanner, // the banner, as a warning
		"\x1b[1;35m[task]",                  // the kind
		"\x1b[1;31m[urgent]",                // the priority
		"`\x1b[1;36maivoicebus claim " + id, // the command to type
		"\x1b[1m" + id,                      // the id
	} {
		if !strings.Contains(out, want) {
			t.Errorf("queue lacks %q:\n%s", want, strings.ReplaceAll(out, "\x1b", "<ESC>"))
		}
	}
}

// The hooks and the MCP server are read by a model: colour is never added to them, forced or not.
func TestHooksAndMCPNeverGetEscapes(t *testing.T) {
	colourBoard(t)
	t.Setenv("AIVOICEBUS_COLOR", "always")
	for _, args := range [][]string{{"hook", "--tool", "codex"}, {"--color=always", "hook", "--tool", "codex"}, {"hook", "--tool", "claude"}} {
		if code, out, e := run(t, "", args...); strings.Contains(out, "\x1b") || strings.Contains(e, "\x1b") {
			t.Errorf("%v (exit %d) wrote an escape: %q %q", args, code, out, e)
		}
	}
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	for _, tool := range []string{"aivoicebus_inbox", "aivoicebus_queue", "aivoicebus_who", "aivoicebus_status"} {
		call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": map[string]any{}}})
		code, out, e := run(t, in+string(call)+"\n", "--color=always", "mcp")
		if code != 0 || !strings.Contains(out, tool) && !strings.Contains(out, "result") {
			t.Fatalf("%s: %d %q %q", tool, code, out, e)
		}
		if strings.Contains(out, "\x1b") || strings.Contains(out, `\u001b`) || strings.Contains(e, "\x1b") {
			t.Errorf("%s: the MCP server wrote an escape: %q", tool, out)
		}
	}
	// a command run in an agent's session is read by it too
	t.Setenv("AIVOICEBUS_AGENT", "claude")
	if _, out, _ := run(t, "", "--color=always", "queue"); strings.Contains(out, "\x1b") {
		t.Errorf("queue in an agent's session was coloured: %q", out)
	}
}

// --json is for programs: no escape, whatever the mode.
func TestJSONIsNeverColoured(t *testing.T) {
	colourBoard(t)
	for _, args := range [][]string{{"--color=always", "queue", "--json"}, {"--color=always", "inbox", "--json"}, {"--color=always", "who", "--json"}} {
		if _, out, _ := run(t, "", args...); strings.Contains(out, "\x1b") || strings.Contains(out, `\u001b`) {
			t.Errorf("%v: %q", args, out)
		}
	}
}
