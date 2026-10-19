package view

import (
	"fmt"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// StateLine is a conversation's state in one line: what it is, who said it when and with what
// note (peer text, cut to one line), and the states it went through.
func StateLine(v msg.StateView) string {
	if v.State == "" {
		return "state: none (nobody offered or took anything in this conversation)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "state: %s (said by %s at %s", v.State, Name(v.By, msg.MaxIdentity), v.At.Local().Format("15:04:05"))
	if v.Note != "" {
		fmt.Fprintf(&b, `: "%s"`, OneLine(Sanitise(v.Note), msg.MaxStateNote))
	}
	b.WriteString(")")
	if len(v.History) > 1 {
		b.WriteString("  " + v.Path())
	}
	if v.Final() {
		b.WriteString("  [final]")
	}
	return b.String()
}
