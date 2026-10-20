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
