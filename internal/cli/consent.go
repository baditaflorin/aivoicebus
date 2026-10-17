package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

var errNoTerminal = errors.New("there is no terminal to answer on")

// terminalFile returns r when it is an interactive terminal.
func terminalFile(r io.Reader) (*os.File, bool) {
	f, ok := r.(*os.File)
	if !ok {
		return nil, false
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return nil, false
	}
	return f, true
}

// askLine writes prompt to stderr and reads one line from the terminal on stdin. A variable so tests
// can answer; with no terminal it fails with errNoTerminal, so nothing is ever agreed to silently.
var askLine = func(s Streams, prompt string) (string, error) {
	f, ok := terminalFile(s.In)
	if !ok {
		return "", errNoTerminal
	}
	fmt.Fprint(s.Err, prompt)
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) { // /dev/null is a character device too: nobody is there to answer
			fmt.Fprintln(s.Err)
			return "", errNoTerminal
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// askYesNo asks a yes/no question; anything but y or yes is a no.
func askYesNo(s Streams, prompt string) (bool, error) {
	got, err := askLine(s, prompt)
	if err != nil {
		return false, err
	}
	got = strings.ToLower(got)
	return got == "y" || got == "yes", nil
}
