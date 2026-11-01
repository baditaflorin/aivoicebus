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
	st := StyleOf(w)
	kind := ""
	if r.Kind != "" {
		kind = " " + st.paint(kindRole(r.Kind), "["+r.Kind+"]")
	}
	fmt.Fprintf(w, "%s%s from %s to %s, sent %s\n", st.ID(r.ID), kind, st.From(Name(r.From, msg.MaxIdentity)), Name(r.To, msg.MaxIdentity), st.Dim(WhenSec(r.At, time.Now())))
	if r.Reach != nil {
		fmt.Fprintf(w, "  it could reach %d session(s) when it was sent\n", *r.Reach)
	}
	if len(r.Reads) == 0 {
		fmt.Fprintln(w, "  "+st.Warn("not read yet by anyone"))
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
			if msg.OneSession(r.To) && rd.By != r.To && r.To != "" && rd.By != r.From { // read by another session than the one it names
				how += "; in place of " + Name(r.To, msg.MaxIdentity) + " (it was handed on, or acknowledged by hand)"
			}
			fmt.Fprintf(w, "    %-40s %s, %s\n", Name(rd.By, msg.MaxIdentity), when, how)
		}
	}
	switch {
	case r.Kind == msg.KindTask && r.Owner != nil:
		fmt.Fprintf(w, "  %s by %s at %s\n", st.Good("taken"), st.From(Name(r.Owner.By, msg.MaxIdentity)), WhenSec(r.Owner.At, time.Now()))
	case r.Kind == msg.KindTask:
		fmt.Fprintf(w, "  %s `%s` shows who is there\n", st.Warn("not taken yet:"), st.Cmd("aivoicebus ping "+Name(r.To, msg.MaxIdentity)))
	}
	if d := OfferDigest(r, time.Now()); d != "" {
		fmt.Fprintln(w, "  "+d)
	}
	if r.Replies > 0 {
		fmt.Fprintf(w, "  %d repl%s: %s\n", r.Replies, map[bool]string{true: "y", false: "ies"}[r.Replies == 1], st.Cmd("aivoicebus thread "+r.ID))
	}
	if r.Expired {
		fmt.Fprintln(w, "  "+st.Dim("expired: it is no longer delivered"))
	}
}
