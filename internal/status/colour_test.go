package status

import (
	"bytes"
	"flag"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/term"
)

var update = flag.Bool("update", false, "rewrite the golden file in testdata")

// screen is a home screen with things to act on and one part that did not answer.
func screen() Report {
	return Report{
		Me:      Me{ID: "lap/claude.a1", Why: "AIVOICEBUS_ME"},
		Token:   &Token{Kind: "machine", Host: "lap"},
		Release: &Release{Here: "0.15.0", Latest: "0.15.1", Update: true, Nodes: map[string]int{"0.15.0": 2}},
		Nodes:   &Nodes{Configured: 2, Up: 2, Peers: 1, PeersOK: 1},
		Inbox:   &Inbox{Unread: 3, Urgent: 1, Ask: 1, FYI: 1},
		Tasks:   &Tasks{Waiting: 1, IDs: []string{"01hx7a3f9k"}},
		Person:  &Person{Approvals: []string{"01hx7b"}},
		Held:    &Held{},
		Pause:   &Pause{},
		Worker:  &Worker{Service: "not installed"},
		Standby: &Standby{},
		Quiet:   &Quiet{Window: "off"},
		Unknown: []string{"pause"},
		Problems: []Problem{
			{"release", "release 0.15.1 is available (this is 0.15.0)", "the person, in a terminal: aivoicebus update (or once, to have it done by itself: aivoicebus update --auto on)"},
			{"inbox", "3 unread: 1 urgent, 1 ask, 1 fyi", "aivoicebus inbox"},
			{"tasks", "1 task(s) wait for this session or machine: 01hx7a3f9k", "aivoicebus queue"},
			{"person", "1 approval(s) wait for the person", "the person, in a terminal: aivoicebus approve 01hx7b"},
		},
		TookMs: 212, BudgetMs: 1500,
	}
}

func TestStatusInColour(t *testing.T) {
	var plain, col bytes.Buffer
	Render(&plain, screen())
	Render(term.Wrap(&col), screen())
	if got := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(col.String(), ""); got != plain.String() {
		t.Errorf("colour changed the text:\n%s\n--- vs\n%s", got, plain.String())
	}
	shown := strings.ReplaceAll(col.String(), "\x1b", "<ESC>")
	if *update {
		if err := os.WriteFile("testdata/status.golden", []byte(shown), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/status.golden")
	if err != nil {
		t.Fatal(err)
	}
	if shown != string(want) {
		t.Errorf("status differs from testdata/status.golden (go test ./internal/status -run TestStatusInColour -update):\n--- got\n%s\n--- want\n%s", shown, want)
	}
}
