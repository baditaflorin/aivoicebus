package cli

import (
	"os"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/term"
)

// takeColour removes the global colour flags from the front of args (before the command, the only
// place they go): --color=auto|always|never (or `--color always`) and --no-color. It returns the
// mode they ask for, Auto when there are none.
func takeColour(args []string) ([]string, term.Mode, error) {
	mode := term.Auto
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "--no-color" || a == "--no-colour":
			mode, args = term.Never, args[1:]
		case a == "--color" || a == "--colour":
			if len(args) < 2 {
				return nil, mode, outbox.Usagef("--color needs a mode: --color=auto, --color=always or --color=never")
			}
			m, err := term.ParseMode(args[1])
			if err != nil {
				return nil, mode, outbox.Usagef("--color: %v", err)
			}
			mode, args = m, args[2:]
		case strings.HasPrefix(a, "--color=") || strings.HasPrefix(a, "--colour="):
			m, err := term.ParseMode(a[strings.IndexByte(a, '=')+1:])
			if err != nil {
				return nil, mode, outbox.Usagef("--color: %v", err)
			}
			mode, args = m, args[1:]
		default:
			return args, mode, nil
		}
	}
	return args, mode, nil
}

// withColour is s with its stdout drawn in colour when that is right for command (see term.Enabled):
// never for the hooks, the MCP server or a command run in an agent's session, whatever is set, because
// a model reads those. Everything else is unchanged: a pipe, a file and a test get the plain text.
func withColour(command string, mode term.Mode, s Streams) Streams {
	if agentReader(command) || !term.Enabled(mode, os.Getenv, term.IsTerminal(s.Out)) {
		return s
	}
	s.Out = term.Wrap(s.Out)
	return s
}
