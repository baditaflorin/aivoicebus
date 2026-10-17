package cli

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// An agent runs commands with stdin at /dev/null, which is a character device like a terminal.
// A question there must end as "no terminal" (so the caller says to pass --yes), never as EOF.
func TestAskOnDevNullIsNoTerminal(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer devnull.Close()
	var errb strings.Builder
	_, err = askYesNo(Streams{In: devnull, Out: &strings.Builder{}, Err: &errb}, "Go ahead? [y/N] ")
	if !errors.Is(err, errNoTerminal) {
		t.Fatalf("got %v, want errNoTerminal", err)
	}
}
