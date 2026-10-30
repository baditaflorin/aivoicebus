package hook

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// The token budget for what waits for the person: a session start injects at most one line of about 160
// characters for each of the first three, then one line that counts the rest, however many wait; and
// nothing at all once they are settled. Measured, so a change that makes it grow fails here.
func TestWhatWaitsForThePersonCostsAtMostFourShortLinesAndSettledCostsNothing(t *testing.T) {
	s, _ := personMesh(t, "0.15.0")
	long := strings.Repeat("rebuild the whole agent history collector and verify it twice ", 6)
	var ids []string
	for i := 0; i < 20; i++ {
		from := fmt.Sprintf("very-long-machine-name-%d/claude.session%d", i, i)
		m, err := s.Send(msg.Message{From: from, To: "air/human", Kind: msg.KindTask, Body: fmt.Sprintf("%s item %d: %s", msg.PersonTag, i, long)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	out := runHook(t, "")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	chars := utf8.RuneCountInString(out)
	t.Logf("20 pending items: %d lines, %d characters injected", len(lines), chars)
	if len(lines) != view.PersonLinesShown+1 {
		t.Fatalf("three items and a count, got %d lines:\n%s", len(lines), out)
	}
	for _, l := range lines {
		if n := utf8.RuneCountInString(l); n > view.PersonLineRunes {
			t.Errorf("a line is %d characters: %s", n, l)
		}
	}
	if !strings.Contains(lines[3], "+17 more") {
		t.Errorf("the rest is counted: %s", lines[3])
	}
	if limit := view.PersonLinesShown*view.PersonLineRunes + 70; chars > limit {
		t.Errorf("%d characters injected, the budget is %d", chars, limit)
	}

	for _, id := range ids {
		if err := s.Settle(id, "air/human"); err != nil {
			t.Fatal(err)
		}
	}
	out = runHook(t, "")
	t.Logf("all settled: %d characters injected", utf8.RuneCountInString(out))
	if out != "" {
		t.Errorf("all settled must inject nothing, got %q", out)
	}
}
