// Package cli maps `aivoicebus <command>` onto the other packages. It owns
// argument parsing, output wording and exit codes, and nothing else.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Streams are the process's standard streams, injectable for tests.
type Streams struct {
	In       io.Reader
	Out, Err io.Writer
}

// command runs one subcommand. A returned error is reported as "error: ..."
// with the exit code exitCode gives it (1 unless it is a kind a script can act on), unless it is an
// exitError or client.ErrNoURL. With --json the failure is one JSON object on stderr instead.
type command func(args []string, s Streams) error

var commands map[string]command

func init() {
	commands = map[string]command{
		"serve":   serve,
		"send":    send,
		"inbox":   inbox,
		"ack":     ack,
		"retract": retractCmd,
		"thread":  thread,
		"who":     who,
		"whoami":  whoami,
		"name":    nameCmd,
		"role":    roleCmd,
		"hook":    hookCmd,
		"setup":   setup,
		"doctor":  doctor,
		"join":    join,
		"mesh":    meshCmd,
		"nodes":   nodesCmd,
		"quiet":   quietCmd,
		"reply":   reply,
		"mcp":     mcpCmd,
		"export":  exportCmd,
		"import":  importCmd,
		"usage":   usageCmd,
		"version": versionCmd,
		"update":  updateCmd,
		"release": releaseCmd,
		"secret":  secretCmd,
		"get":     getCmd,
		"tail":    tailCmd,

		"uninstall": uninstallCmd,
		"trust-key": trustKeyCmd,
		"help":      helpCmd,
		"view":      viewCmd,
		"task":      taskCmd,
		"claim":     claimCmd,
		"queue":     queueCmd,
		"wait":      waitCmd,
		"ping":      pingCmd,
		"status":    statusCmd,
		"worker":    workerCmd,
	}
}

// exitError carries an exit code for failures already reported to the user
// (flag errors, -h).
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// Run executes one aivoicebus invocation and returns its exit code.
func Run(args []string, s Streams) int {
	config.LoadEnvFile()
	if len(args) < 1 {
		fmt.Fprintln(s.Err, "usage: aivoicebus <command> [flags]   (aivoicebus help lists the commands)")
		return 2
	}
	if args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
		printHelp(s.Out)
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintln(s.Err, "error:", fmt.Errorf("unknown command %q", args[0]))
		return 1
	}
	err := cmd(args[1:], s)
	code := exitCode(err)
	var ee exitError
	switch {
	case err == nil, errors.As(err, &ee):
	case wantsJSON(args[1:]):
		jsonFailure(s.Err, err, code)
	case errors.Is(err, client.ErrNoURL):
		fmt.Fprintln(s.Err, err)
	default:
		fmt.Fprintln(s.Err, "error:", view.Sanitise(err.Error()))
	}
	return code
}

// parseFlags parses args into fs; flag errors are printed by the flag package
// and surface as exit code 2 (0 for -h).
func parseFlags(fs *flag.FlagSet, args []string, s Streams) error {
	// Under --json a flag error is the JSON error object Run prints, not the flag package's text.
	asJSON := wantsJSON(args) && !slices.ContainsFunc(args, func(a string) bool { return a == "-h" || a == "-help" || a == "--help" })
	fs.SetOutput(s.Err)
	if asJSON {
		fs.SetOutput(io.Discard)
	}
	switch err := fs.Parse(args); {
	case err == nil:
		return nil
	case errors.Is(err, flag.ErrHelp):
		return exitError{0}
	case asJSON:
		return outbox.Usagef("%v", err)
	default:
		return exitError{2}
	}
}

// wantsHelp reports whether args ask for help (-h, -help, --help) as their first word, for the
// commands that take no flags and so have no flag set to print a usage from.
func wantsHelp(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help")
}

func newFlagSet(name string) *flag.FlagSet { return flag.NewFlagSet(name, flag.ContinueOnError) }

// stdinOrNil returns r unless it is an interactive terminal (or unusable), in
// which case there is no piped input to read.
func stdinOrNil(r io.Reader) io.Reader {
	if f, ok := r.(*os.File); ok {
		if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice != 0 {
			return nil
		}
	}
	return r
}
