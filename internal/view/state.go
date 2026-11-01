package view

import (
	"fmt"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// StateLine is a conversation's state in one line: what it is, who said it when and with what
// note (peer text, cut to one line), and the states it went through.
func StateLine(v msg.StateView) string { return StateLineIn(v, Style{}) }

// StateLineIn is StateLine drawn in st: the state itself stands out, who said it and when is secondary.
func StateLineIn(v msg.StateView, st Style) string {
	if v.State == "" {
		return st.Dim("state: none (nobody offered or took anything in this conversation)")
	}
	var b strings.Builder
	b.WriteString(st.Dim("state:") + " " + st.Heading(v.State))
	fmt.Fprintf(&b, " (said by %s at %s", Name(v.By, msg.MaxIdentity), WhenSec(v.At, time.Now()))
	if v.Note != "" {
		fmt.Fprintf(&b, `: "%s"`, OneLine(Sanitise(v.Note), msg.MaxStateNote))
	}
	b.WriteString(")")
	if len(v.History) > 1 {
		b.WriteString("  " + v.Path())
	}
	if v.Final() {
		b.WriteString("  " + st.Dim("[final]"))
	}
	return b.String()
}
