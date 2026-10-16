package msg

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxTitle is the longest session title, in characters. A title is what a tool calls the session
// ("Fix the hub login redirect"): a few words a person reads, never routed on.
const MaxTitle = 80

// ParamTitle is the inbox query parameter that carries a session's current title.
const ParamTitle = "title"

// CleanTitle makes s fit to show as a session title: valid UTF-8, no control or invisible
// characters (a zero-width joiner, a bidi override, a blank letter are dropped; tabs, newlines and
// other separators become one plain space), runs of spaces collapsed, at most MaxTitle characters.
// It returns "" when nothing readable is left. A title is text a session chose for itself, so
// whoever reads it must still treat it as untrusted (see view.Title).
func CleanTitle(s string) string {
	var b strings.Builder
	n := 0
	pendingSpace := false
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
			continue
		case unicode.IsSpace(r) || unicode.IsControl(r):
			pendingSpace = b.Len() > 0
			continue
		case invisible(r):
			continue
		}
		if pendingSpace {
			b.WriteByte(' ')
			n++
			pendingSpace = false
		}
		if n >= MaxTitle {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// replMaxTitle bounds the title of a replicated message in bytes: MaxTitle characters of up to four
// bytes. Only a bound: a node shows every title cleaned again, so what a peer stored is inert.
const replMaxTitle = MaxTitle * 4
