package view

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// A broadcast is handed to every session that ever starts, and most of those sessions have no
// use for a long one: it was meant for two or three owners, or it is hours old and already
// dealt with. The hooks show such a message as its headline: the usual header, the first line of
// its body (HeadlineRunes at most) and the command that reads the rest. Only what the hooks put
// into a session's context changes. The message is stored and sent whole, and `inbox`, `thread`,
// `get` and the MCP tools still show all of it.
const (
	// HeadlineOver is the longest broadcast body, in runes as a reader sees it, that a hook still
	// shows as its usual cut. A longer one is shown as its headline.
	HeadlineOver = 600
	// HeadlineRunes is how much of a headline's first line is kept.
	HeadlineRunes = 160
	// ReplayAge is how old a broadcast must be, when a session starts, to count as a replay: news
	// that session was not there for. A replay is shown as its headline whatever its length.
	ReplayAge = 6 * time.Hour
)

// Headliner says whether a message is shown as its headline. body is the message's text as a
// reader would see it (rendered and sanitised), so a length rule counts what is really there.
type Headliner func(m msg.Message, body string) bool

// Headlines is the rule the hooks use for the messages of one inbox read, batch (every message
// the read returned, shown or not, so a thread that one message of the batch closes counts as
// closed for the others), at time now. A message is shown as its headline when it is a
// broadcast (msg.IsBroadcast: a message to one session, or to one tool on one machine, is
// never shortened), is not urgent (its sender asked that it be seen), and is
//   - longer than HeadlineOver runes, or
//   - a replay: older than ReplayAge, or
//   - in a thread that batch shows to be over: ended by one of its messages, or moved to done or
//     declined by a state message the handshake rules accept (msg.ThreadState).
//
// A message whose headline would not be shorter than what the hook shows otherwise (a
// short one-liner) keeps its usual form: BriefWith decides that, so a headline never costs more.
func Headlines(batch []msg.Message, now time.Time) Headliner {
	over := overThreads(batch)
	return func(m msg.Message, body string) bool {
		switch {
		case !msg.IsBroadcast(m.To), m.Priority() == msg.PrioUrgent, m.Retracted != nil:
			return false
		case over[m.Thread]:
			return true
		case !m.At.IsZero() && now.Sub(m.At) > ReplayAge:
			return true
		}
		return utf8.RuneCountInString(body) > HeadlineOver
	}
}

// overThreads is the threads of batch that batch itself shows to be finished. A thread with one
// message in the batch proves nothing (the message that closed it may have been read, or not
// sent here), so it is never over.
func overThreads(batch []msg.Message) map[string]bool {
	byThread := map[string][]msg.Message{}
	for _, m := range batch {
		byThread[m.Thread] = append(byThread[m.Thread], m)
	}
	over := map[string]bool{}
	for id, ms := range byThread {
		if id == "" || len(ms) < 2 { // messages that name no thread are not one conversation
			continue
		}
		if msg.ThreadState(ms).Final() {
			over[id] = true
			continue
		}
		for _, m := range ms {
			if m.Kind == msg.KindEnd && m.Retracted == nil {
				over[id] = true
				break
			}
		}
	}
	return over
}

// headlineOf is body (already sanitised) as one line: its first non-blank line, cut to
// HeadlineRunes, then the command that reads the rest. The command is left off when nothing was
// left out. ok is false when body has no text to quote.
func headlineOf(body, id string) (line string, ok bool) {
	rest := false
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		if line != "" {
			rest = true
			break
		}
		line = l
	}
	if line == "" {
		return "", false
	}
	if r := []rune(line); len(r) > HeadlineRunes {
		line = string(r[:graphemeCut(r, HeadlineRunes)]) + "…"
		rest = true
	}
	if rest {
		line += fmt.Sprintf(" [aivoicebus thread %s for the rest]", OneLine(id, msg.MaxRef))
	}
	return line, true
}
