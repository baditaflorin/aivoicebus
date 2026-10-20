package msg

import "strings"

// stuck: an agent says it is blocked instead of going silent. The thread's other side gets it as
// a progress message (a wait shows it and goes on), and the person, or a listening peer, gets a
// copy that starts with StuckPrefix.

const StuckPrefix = "[stuck] "

// Who a stuck agent asks for help, besides the thread's other side: its person (the default), any
// listening peer, or a listening session with a tool or capability (any other word).
const (
	NeedPerson = "person"
	NeedPeer   = "peer"
)

// StuckSummaryRunes is the most of a stuck reason a phone notification carries.
const StuckSummaryRunes = 80

// IsStuck reports whether body is a stuck message.
func IsStuck(body string) bool { return strings.HasPrefix(body, StuckPrefix) }
