package view

import (
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// When is how a time is shown to a person: in this machine's local time, the clock alone when it
// is today, else the date and the zone too ("Oct 17 14:03 EEST"), so a time from another day
// cannot pass for one from today, nor a local time for a UTC one. Machine-read output (JSON, the
// nodes' API) stays UTC RFC 3339. The zero time is "never".
func When(t, now time.Time) string { return when(t, now, "15:04") }

// WhenSec is When to the second, for logs and receipts.
func WhenSec(t, now time.Time) string { return when(t, now, "15:04:05") }

func when(t, now time.Time, clock string) string {
	if t.IsZero() {
		return "never"
	}
	t, now = t.Local(), now.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format(clock)
	}
	layout := "Jan 2 " + clock + " MST"
	if t.Year() != now.Year() {
		layout = "2006 " + layout
	}
	return t.Format(layout)
}

// Session is how a session is named wherever one is listed or chosen: its id, the title its tool
// gave it, and whether it is listening right now (in a wait: it reads a message within a second)
// or when it was last seen. Short hashes alone are easy to mix up: a hand-off once went to the
// wrong session of a machine because two ids differed only in their last four characters.
//
//	mac14-m1pro-mbp/claude.aac5 "Aivoicebus standby" (listening)
//	mac14-m1pro-mbp/claude.632e "Fix the login redirect" (seen Oct 17 14:03 EEST)
func Session(e msg.RosterEntry, now time.Time) string {
	var b strings.Builder
	b.WriteString(Name(e.Me, msg.MaxIdentity))
	b.WriteString(TitleSuffix(e.Title))
	if tool := e.Labels["tool"]; tool != "" && !strings.HasPrefix(agentOf(e.Me), strings.ToLower(tool)) {
		b.WriteString(" [" + Name(tool, 24) + "]") // an id named by AIVOICEBUS_NAME does not say its tool
	}
	b.WriteString(" (" + SessionState(e, now) + ")")
	return b.String()
}

// SessionState is "listening" or "seen <when>", the state Session shows in brackets.
func SessionState(e msg.RosterEntry, now time.Time) string {
	if e.Listening {
		return "listening"
	}
	return "seen " + When(e.Seen, now)
}

func agentOf(me string) string {
	_, agent, _ := strings.Cut(strings.ToLower(me), "/")
	return agent
}
