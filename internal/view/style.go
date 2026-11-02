package view

import (
	"io"
	"regexp"
)

// How the output is drawn for a person at a terminal.
//
// The renderers here write plain text. A writer that carries a Painter (see WithPainter) makes them
// draw the parts that are the program's own (headers, tags, the commands to type, the untrusted
// banner) in colour; every other writer, which is what the hooks, the MCP server, a pipe and a file
// get, gets exactly the text it always did. The library that draws the colours lives in
// internal/term, so nothing here depends on it, and the viewer package stays free of it.
//
// A Painter is only ever given text that is already safe: a name or id that went through Name or
// OneLine, or a fixed word of the program's. What a peer wrote in a body is never passed to one, so
// a message cannot choose its own styling.

// Role says what a piece of output is, not how it looks: the Painter decides that.
type Role int

const (
	RolePlain   Role = iota
	RoleHeading      // a title: bold
	RoleID           // a message or thread id: bold
	RoleFrom         // who sent it
	RoleDim          // secondary: separators, times, thread ids, explanations
	RoleTask         // the [task] tag
	RoleUrgent       // the [urgent] tag
	RoleWarn         // a caution: the untrusted banner, a changed key, a state to look at
	RoleBad          // an error or a failed check: a bad signature, a problem to act on
	RoleGood         // something fine: listening, up to date
	RoleCmd          // a command to type
)

// Painter draws s in the style of role r. It never gets text a peer wrote that was not made safe first.
type Painter interface {
	Paint(r Role, s string) string
}

// painted is a writer that carries a Painter.
type painted struct {
	io.Writer
	p Painter
}

// WithPainter is w, with p to draw what the renderers write to it. A nil p leaves w as it is.
func WithPainter(w io.Writer, p Painter) io.Writer {
	if p == nil {
		return w
	}
	return painted{w, p}
}

// Style is how one writer's output is drawn; its zero value draws nothing.
type Style struct{ p Painter }

// StyleOf is the style of w: plain unless w came from WithPainter.
func StyleOf(w io.Writer) Style {
	if pw, ok := w.(painted); ok {
		return Style{pw.p}
	}
	return Style{}
}

// On reports whether anything is drawn.
func (s Style) On() bool { return s.p != nil }

func (s Style) paint(r Role, text string) string {
	if s.p == nil || text == "" {
		return text
	}
	return s.p.Paint(r, text)
}

func (s Style) Heading(t string) string { return s.paint(RoleHeading, t) }
func (s Style) ID(t string) string      { return s.paint(RoleID, t) }
func (s Style) From(t string) string    { return s.paint(RoleFrom, t) }
func (s Style) Dim(t string) string     { return s.paint(RoleDim, t) }
func (s Style) Task(t string) string    { return s.paint(RoleTask, t) }
func (s Style) Urgent(t string) string  { return s.paint(RoleUrgent, t) }
func (s Style) Warn(t string) string    { return s.paint(RoleWarn, t) }
func (s Style) Bad(t string) string     { return s.paint(RoleBad, t) }
func (s Style) Good(t string) string    { return s.paint(RoleGood, t) }
func (s Style) Cmd(t string) string     { return s.paint(RoleCmd, t) }

// Notice is a line of the program's own that starts with "[aivoicebus]": the tag dim, the rest as
// given. It is for a line without its own emphasis; use the parts for one that has.
func (s Style) Notice(rest string) string { return s.Dim("[aivoicebus]") + rest }

// UntrustedBanner is the banner that precedes peer text, drawn as the warning it is.
func (s Style) UntrustedBanner() string { return s.Warn(UntrustedBanner) }

// cmdRe finds the commands in one of the program's own sentences: "aivoicebus ..." up to the next
// parenthesis, comma or full stop that ends the command, and the two other things a fix names
// (a deploy script, sudo). It is only run on text the program wrote, never on a peer's.
var cmdRe = regexp.MustCompile(`(?:aivoicebus|sudo|deploy/\S+)[^(),]*[^(), .]`)

// Commands is text with each command in it drawn as a command (see cmdRe), the rest as it is.
func (s Style) Commands(text string) string {
	if s.p == nil {
		return text
	}
	var out []byte
	last := 0
	for _, loc := range cmdRe.FindAllStringIndex(text, -1) {
		out = append(out, text[last:loc[0]]...)
		out = append(out, s.Cmd(text[loc[0]:loc[1]])...)
		last = loc[1]
	}
	return string(append(out, text[last:]...))
}
