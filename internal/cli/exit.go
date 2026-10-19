package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Exit codes. A script can tell what to do next from the code alone: fix the command (2), look
// elsewhere (3), retry later (4), fix the token (5), or read the message (1).
const (
	exitOK          = 0
	exitFailure     = 1 // the command ran and failed, or `mesh`/`doctor` found a problem
	exitUsage       = 2 // bad flags, a missing or ambiguous argument, no node configured
	exitNotFound    = 3 // the message, thread or id asked for is not there
	exitUnreachable = 4 // no node answered: the mesh may be away
	exitRejected    = 5 // a node refused the token or what it may do
	exitTimeout     = 6 // `wait` (or --wait): nothing arrived in the time given
)

// exitReplaced is `worker`'s code when its binary was replaced: a service restarts it (EX_TEMPFAIL).
const exitReplaced = 75

// exitCode is the code for an error a command returned.
func exitCode(err error) int {
	var ee exitError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ee):
		return ee.code
	case errors.Is(err, client.ErrNoURL), errors.Is(err, outbox.ErrAmbiguous), errors.Is(err, outbox.ErrUsage):
		return exitUsage
	case errors.Is(err, outbox.ErrNotFound):
		return exitNotFound
	case client.IsUnreachable(err):
		return exitUnreachable
	case client.IsRejected(err):
		return exitRejected
	}
	return exitFailure
}

// wantsJSON reports whether args ask for --json, so a failure can be reported as JSON too
// (before any flag set has parsed them).
func wantsJSON(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		a = strings.TrimLeft(a, "-")
		return a == "json" || a == "json=true"
	})
}

// takeJSON removes a --json given after the positional arguments (the flag package stops
// parsing at the first one, so `thread ID --json` would otherwise be two arguments).
func takeJSON(args []string, asJSON *bool) []string {
	var out []string
	for _, a := range args {
		if a == "--json" || a == "-json" {
			*asJSON = true
			continue
		}
		out = append(out, a)
	}
	return out
}

// isTerminal reports whether r is an interactive terminal. /dev/null is a character device too,
// and it is what an agent's tool call often gets as stdin: nobody is there.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}

// writeJSON prints v as indented JSON, the shape every --json output has.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// jsonFailure is what a failing command prints on stderr under --json: stdout stays empty, so a
// script that parses it never meets half an answer.
func jsonFailure(w io.Writer, err error, code int) {
	var se *client.StatusError
	reason := ""
	if errors.As(err, &se) {
		reason = se.Refused() // a node's refusal, as one stable word a script can act on
	}
	b, _ := json.Marshal(struct {
		Error  string `json:"error"`
		Exit   int    `json:"exit"`
		Reason string `json:"reason,omitempty"`
	}{view.Sanitise(err.Error()), code, reason})
	fmt.Fprintf(w, "%s\n", b)
}
