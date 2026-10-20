package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
)

func completions(t *testing.T, words ...string) []string {
	t.Helper()
	code, out, e := run(t, "", append([]string{"__complete"}, words...)...)
	if code != 0 {
		t.Fatalf("__complete %q: %d %s", words, code, e)
	}
	return strings.Fields(out)
}

func TestCompletionOffersCommandsSubcommandsAndFlags(t *testing.T) {
	freshHome(t)
	if got := completions(t, "fl"); len(got) != 1 || got[0] != "fleet" {
		t.Errorf("commands: %q", got)
	}
	if got := completions(t, ""); contains(got, "__complete") || !contains(got, "send") || !contains(got, "completion") {
		t.Errorf("every listed command but the internal one: %q", got)
	}
	if got := completions(t, "token", ""); strings.Join(got, " ") != "hand install report" {
		t.Errorf("token subcommands: %q", got)
	}
	if got := completions(t, "watch", "notify", ""); strings.Join(got, " ") != "clear set test" {
		t.Errorf("watch notify: %q", got)
	}
	if got := completions(t, "ask", "lap", "--wait"); !contains(got, "--wait-reply") {
		t.Errorf("ask's flags, from its own -h: %q", got)
	}
	if got := completions(t, "fleet", "update", "--"); !contains(got, "--apply") || !contains(got, "--only-behind") {
		t.Errorf("a subcommand's flags: %q", got)
	}
	if got := completions(t, "help", "sta"); !contains(got, "start") || !contains(got, "status") {
		t.Errorf("help completes commands: %q", got)
	}
}

func TestCompletionScriptsForEachShell(t *testing.T) {
	for shell, want := range map[string]string{"zsh": "#compdef aivoicebus", "bash": "complete -o default -F _aivoicebus", "fish": "complete -c aivoicebus"} {
		code, out, _ := run(t, "", "completion", shell)
		if code != 0 || !strings.Contains(out, want) || !strings.Contains(out, "aivoicebus __complete") {
			t.Errorf("%s: %d\n%s", shell, code, out)
		}
	}
	if code, _, _ := run(t, "", "completion", "powershell"); code != exitUsage {
		t.Errorf("an unknown shell is a usage error: %d", code)
	}
}

// Completion reads each command's flags from its -h, in process: for every listed command that must
// stop at the flags, quickly, writing nothing to the home directory and reaching no node.
func TestDashHIsHarmlessForEveryListedCommand(t *testing.T) {
	for _, tp := range helpTopics {
		if tp.name == "__complete" || tp.name == "help" || tp.name == "hook" || tp.name == "mcp" {
			continue
		}
		t.Run(tp.name, func(t *testing.T) {
			p := freshHome(t)
			t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:1")
			done := make(chan struct{})
			go func() { defer close(done); flagsOf([]string{tp.name}) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s -h did not return within 5s", tp.name)
			}
			if entries, _ := os.ReadDir(p.Home); len(entries) != 0 {
				names := []string{}
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("%s -h wrote to the home directory: %v", tp.name, names)
			}
		})
	}
}

// pause parsed every word but --for as its reason, so `pause -h` (or a mistyped flag) paused the
// whole mesh with that word as the reason: found by the -h test above.
func TestPauseNeverTakesAFlagForItsReason(t *testing.T) {
	mesh(t)
	if code, out, _ := run(t, "", "pause", "-h"); code != 0 || !strings.Contains(out, "usage: pause") {
		t.Errorf("pause -h: %d %q", code, out)
	}
	if code, _, e := run(t, "", "pause", "--dry-run"); code != exitUsage || !strings.Contains(e, "unknown flag --dry-run") {
		t.Errorf("pause --dry-run: %d %q", code, e)
	}
	c, _ := client.FromEnv()
	if ms, _ := c.Inbox("x/agent", false); len(ms) != 0 {
		t.Errorf("nothing was sent: %+v", ms)
	}
}

// Every example in `help COMMAND` names a listed command and passes that command's own flag parser
// (run with -h appended, so nothing is sent): an example cannot rot when flags change.
func TestHelpExamplesUseRealFlags(t *testing.T) {
	freshHome(t)
	for name, examples := range helpExamples {
		if !listed(name) {
			t.Errorf("examples for %q, which help does not list", name)
		}
		known := map[string]bool{}
		for _, f := range flagsOf([]string{name}) {
			known[f] = true
		}
		for _, sub := range completionSubs[name] {
			for _, f := range flagsOf([]string{name, sub}) {
				known[f] = true
			}
		}
		for _, ex := range examples {
			for _, w := range strings.Fields(strings.SplitN(ex, "#", 2)[0]) {
				if strings.HasPrefix(w, "--") && !known[strings.SplitN(w, "=", 2)[0]] {
					t.Errorf("%s example uses %s, which %s does not take: %q", name, w, name, ex)
				}
			}
		}
		code, out, _ := run(t, "", "help", name)
		if code != 0 || !strings.Contains(out, "examples:") || !strings.Contains(out, examples[0][:20]) {
			t.Errorf("help %s: %d\n%s", name, code, out)
		}
	}
}
