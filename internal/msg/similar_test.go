package msg

import (
	"strings"
	"testing"
)

func TestSimilarity(t *testing.T) {
	for _, c := range []struct {
		a, b   string
		repeat bool
	}{
		{"Thanks!", "thanks", true},
		{"!!!", "?", true}, // nothing but punctuation is the same nothing
		{"turn 6", "turn 7", false},
		{"Thanks! Let me know if you need anything else.", "Thanks! Do let me know if you need anything else.", true},
		{"Ran the tests on branch foo: 3 failures in store_test.go", "Fixed 2 of them, ran the tests on branch foo again: 1 failure in store_test.go", false},
		{"please confirm that build 3 of the store package is deployed", "please confirm that build 7 of the store package is deployed", true},
	} {
		if got := Similarity(c.a, c.b) >= RepeatSimilarity; got != c.repeat {
			t.Errorf("%q vs %q: %.2f, repeat %v, want %v", c.a, c.b, Similarity(c.a, c.b), got, c.repeat)
		}
		if Similarity(c.a, c.b) != Similarity(c.b, c.a) {
			t.Errorf("%q vs %q: not symmetric", c.a, c.b)
		}
	}
	long := strings.Repeat("word ", 10000)
	if Similarity(long+"one", long+"two") != 1 {
		t.Error("only the first few kilobytes of a body are read")
	}
}

func TestSizeOf(t *testing.T) {
	ms := []Message{
		{ID: "a000000001", From: "lap/claude.a1", Body: "can you run it?"},
		{ID: "a000000002", From: "box/worker", Body: "[worker on box] exit 0 after 12s (14212 tokens in (9984 cached), 5 out, $0.0123).\n42"},
		{ID: "a000000002", From: "box/worker", Body: "[worker on box] exit 0 after 12s (14212 tokens in (9984 cached), 5 out, $0.0123).\n42"},
		{ID: "a000000003", From: "box/codex.b2", Body: "[worker on box] exit 0 after 1s (999 tokens in, 1 out).\nnot a worker"},
		{ID: "a000000004", From: "box/worker", Body: "[worker on box] exit 1 after 3s.\nno counts"},
		{ID: "a000000005", From: "box/worker", Body: "[worker on box] exit 0 after 2s (100 tokens in, 20 out).\nmore"},
	}
	ms[0].Thread, ms[0].BudgetChars = ms[0].ID, 5000
	z := SizeOf(ms)
	want := 15 + 84 + 68 + 42 + 61
	if z.Messages != 5 || z.Chars != want || z.EstTokens != want/CharsPerToken || z.Runs != 2 || z.TokensIn != 14312 || z.TokensOut != 25 || z.CostUSD != 0.0123 || z.BudgetChars != 5000 {
		t.Errorf("%+v (want %d chars)", z, want)
	}
}
