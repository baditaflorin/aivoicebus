// Package term decides whether the human-facing output is drawn in colour, and draws it. It is the
// one place that imports the colour library (github.com/fatih/color) and the terminal check
// (github.com/mattn/go-isatty); the rest of the program asks for a role (see view.Role) and never
// writes an escape sequence.
//
// Colour is only for a person at a terminal: stdout is a terminal, NO_COLOR is unset (no-color.org),
// TERM is not "dumb". `--color=always|never` and AIVOICEBUS_COLOR override that. The hooks, the MCP
// server and an agent's session never get colour, whatever is set: what they print is read by a
// model, and an escape sequence in its context is noise at best.
package term

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/fatih/color"
	"github.com/mattn/go-isatty"

	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Mode is what the person asked for.
type Mode int

const (
	Auto   Mode = iota // colour when stdout is a terminal and nothing says no
	Always             // colour even into a pipe (a pager: aivoicebus queue | less -R)
	Never              // plain text
)

// EnvMode is the environment variable that sets the mode; --color on the command line beats it.
const EnvMode = "AIVOICEBUS_COLOR"

// ParseMode reads auto, always or never (also: yes/true/1/on, no/false/0/off; empty is auto).
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return Auto, nil
	case "always", "yes", "true", "1", "on":
		return Always, nil
	case "never", "no", "false", "0", "off", "none":
		return Never, nil
	}
	return Auto, fmt.Errorf("%q is not a colour mode: use auto, always or never", s)
}

// Enabled says whether to draw colour. flag is the --color mode given on the command line (Auto when
// none was), env reads the environment, tty says whether the destination is a terminal. Order: the
// flag, then AIVOICEBUS_COLOR, then NO_COLOR (when set and not empty), then TERM=dumb, then tty.
func Enabled(flag Mode, env func(string) string, tty bool) bool {
	m := flag
	if m == Auto {
		// An AIVOICEBUS_COLOR that is not a mode is ignored: a typo is never a reason for colour.
		if v, err := ParseMode(env(EnvMode)); err == nil {
			m = v
		}
	}
	switch m {
	case Always:
		return true
	case Never:
		return false
	}
	return env("NO_COLOR") == "" && env("TERM") != "dumb" && tty
}

// IsTerminal reports whether w is a terminal (a console, or a Cygwin/MSYS pty).
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd()))
}

// Wrap is w drawn in colour: the renderers (view, status) see the palette on it and use it.
// Everything written to the result reaches w unchanged.
func Wrap(w io.Writer) io.Writer { return view.WithPainter(w, Palette{}) }

// Palette is the colours, one per role. Only the 16 basic ANSI colours, bold and faint, so it reads on
// a light and a dark background and follows the terminal's own theme.
type Palette struct{}

// each style is forced on (EnableColor): whether to colour was decided above, not by the library's
// own check of os.Stdout.
var styles = map[view.Role]*color.Color{
	view.RoleHeading: attrs(color.Bold),
	view.RoleID:      attrs(color.Bold),
	view.RoleFrom:    attrs(color.FgGreen),
	view.RoleDim:     attrs(color.Faint),
	view.RoleTask:    attrs(color.Bold, color.FgMagenta),
	view.RoleUrgent:  attrs(color.Bold, color.FgRed),
	view.RoleWarn:    attrs(color.Bold, color.FgYellow),
	view.RoleBad:     attrs(color.Bold, color.FgRed),
	view.RoleGood:    attrs(color.FgGreen),
	view.RoleCmd:     attrs(color.Bold, color.FgCyan),
}

func attrs(a ...color.Attribute) *color.Color {
	c := color.New(a...)
	c.EnableColor()
	return c
}

// Paint implements view.Painter. The text is drawn on one line of its own: a control character in it
// (an escape that got this far) is dropped, so a styled piece can never carry a sequence of its own.
func (Palette) Paint(r view.Role, s string) string {
	c := styles[r]
	if c == nil || s == "" {
		return s
	}
	return c.Sprint(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\t' {
			return -1
		}
		return r
	}, s))
}
