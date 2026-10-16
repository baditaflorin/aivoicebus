package outbox

import (
	"fmt"
	"strings"
	"testing"
)

func TestCapLinesAreEmptyForAnOrdinarySendAndSanitiseAndCapTheNames(t *testing.T) {
	if got := (Plan{}).CapLines(); got != nil {
		t.Errorf("an ordinary send says nothing about capabilities: %v", got)
	}
	var names []string
	for i := 0; i < 11; i++ {
		names = append(names, fmt.Sprintf("h/claude.%d\x1b[2J", i))
	}
	lines := Plan{Recipients: names}.CapLines()
	if len(lines) != 1 || strings.Contains(lines[0], "\x1b") || !strings.Contains(lines[0], ", +3 more") || !strings.Contains(lines[0], "self-declared") {
		t.Errorf("%q", lines)
	}
	lines = Plan{Recipients: names[:1], CapNote: "careful"}.CapLines()
	if len(lines) != 2 || lines[1] != "careful" {
		t.Errorf("%q", lines)
	}
}
