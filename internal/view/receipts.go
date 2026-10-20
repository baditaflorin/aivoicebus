package view

import (
	"fmt"
	"io"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// viaWords says how a reader was shown a message, for status.
var viaWords = map[string]string{
	msg.ViaHook:      "shown at a session start or prompt",
	msg.ViaHookCount: "only counted at a session start (fyi)",
	msg.ViaInbox:     "listed by aivoicebus inbox",
	msg.ViaCLI:       "read with the CLI (wait, claim, ack)",
	msg.ViaReply:     "answered it",
}

// Receipts writes what msg.Receipts says about a message, the way `aivoicebus status` and the
// MCP status tool show it: who read it and when, who took a task, how many replies.
func Receipts(w io.Writer, r msg.Receipts) {
	kind := ""
	if r.Kind != "" {
		kind = " [" + r.Kind + "]"
	}
	fmt.Fprintf(w, "%s%s from %s to %s, sent %s\n", r.ID, kind, Name(r.From, msg.MaxIdentity), Name(r.To, msg.MaxIdentity), WhenSec(r.At, time.Now()))
	if r.Reach != nil {
		fmt.Fprintf(w, "  it could reach %d session(s) when it was sent\n", *r.Reach)
	}
	if len(r.Reads) == 0 {
		fmt.Fprintln(w, "  not read yet by anyone")
	} else {
		fmt.Fprintf(w, "  read by %d:\n", len(r.Reads))
		for _, rd := range r.Reads {
			when := "time not known"
			if !rd.At.IsZero() {
				when = WhenSec(rd.At, time.Now()) + " (" + time.Since(rd.At).Round(time.Second).String() + " ago)"
			}
			how := viaWords[rd.Via]
			if how == "" {
				how = OneLine(rd.Via, 20)
			}
			fmt.Fprintf(w, "    %-40s %s, %s\n", Name(rd.By, msg.MaxIdentity), when, how)
		}
	}
	switch {
	case r.Kind == msg.KindTask && r.Owner != nil:
		fmt.Fprintf(w, "  taken by %s at %s\n", Name(r.Owner.By, msg.MaxIdentity), WhenSec(r.Owner.At, time.Now()))
	case r.Kind == msg.KindTask:
		fmt.Fprintf(w, "  not taken yet: `aivoicebus ping %s` shows who is there\n", Name(r.To, msg.MaxIdentity))
	}
	if r.Replies > 0 {
		fmt.Fprintf(w, "  %d repl%s: aivoicebus thread %s\n", r.Replies, map[bool]string{true: "y", false: "ies"}[r.Replies == 1], r.ID)
	}
	if r.Expired {
		fmt.Fprintln(w, "  expired: it is no longer delivered")
	}
}
