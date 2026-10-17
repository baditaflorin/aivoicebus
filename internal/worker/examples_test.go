package worker

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The service examples in deploy/worker give --exec as text a person copies: it must split into
// the command the docs describe, with the system prompt as one argument.
func TestTheServiceExamplesExecSplitAsDocumented(t *testing.T) {
	unit, err := os.ReadFile("../../deploy/worker/aivoicebus-worker.service")
	if err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile("../../deploy/worker/org.aivoicebus.worker.plist")
	if err != nil {
		t.Fatal(err)
	}
	fromUnit := regexp.MustCompile(`(?m)^ExecStart=\S+ worker --exec "([^"]+)"$`).FindSubmatch(unit)
	fromPlist := regexp.MustCompile(`<string>--exec</string>\s*<string>([^<]+)</string>`).FindSubmatch(plist)
	if fromUnit == nil || fromPlist == nil {
		t.Fatalf("no --exec found: unit %v plist %v", fromUnit != nil, fromPlist != nil)
	}
	if string(fromUnit[1]) != string(fromPlist[1]) {
		t.Errorf("the unit and the plist run different commands:\n%s\n%s", fromUnit[1], fromPlist[1])
	}
	argv, err := SplitCommand(string(fromUnit[1]))
	if err != nil || len(argv) != 6 || argv[0] != "claude" || argv[1] != "-p" || argv[3] != "Read,Grep,Glob" || argv[4] != "--append-system-prompt" || !strings.Contains(argv[5], "untrusted input") {
		t.Errorf("split: %q %v", argv, err)
	}
}
