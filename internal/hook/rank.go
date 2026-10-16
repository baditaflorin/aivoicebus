package hook

import (
	"cmp"
	"slices"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// maxBroadcasts is how many broadcasts a session start prints. A broadcast reaches
// every session that ever starts, so a few greetings and release notes would
// otherwise fill all maxShown slots and push the one message addressed to this
// session behind "+N more unread".
const maxBroadcasts = 2

// How wide the audience of a target is, narrowest first. From tierSlice on a
// message is a broadcast.
const (
	tierSession  = iota // host/tool.session, or exactly this identity: one session
	tierHostTool        // host/tool: one tool on one machine
	tierSlice           // @selector, a whole host, host/*: a slice of the fleet
	tierTool            // */tool: every session of a tool
	tierAll             // all, */*: everyone
)

// tier is how wide the audience of target to is, as seen by identity me. It reads the
// address only: how many sessions a selector reaches depends on labels the hook
// cannot see, so a selector counts as a slice of the fleet, never as one session.
func tier(to, me string) int {
	switch {
	case to == me:
		return tierSession
	case to == "all":
		return tierAll
	case msg.IsSelector(to):
		return tierSlice
	}
	host, agent, hasAgent := strings.Cut(to, "/")
	switch {
	case !hasAgent: // a whole host
		return tierSlice
	case agent == "*" && host == "*":
		return tierAll
	case agent == "*": // host/*
		return tierSlice
	case strings.Contains(agent, "."): // host/tool.session, whichever host
		return tierSession
	case host == "*":
		return tierTool
	}
	return tierHostTool
}

// broadcast reports whether target to addresses an audience instead of this
// session: all, */tool, a selector (@repo=x), a whole host and host/*. A message to
// one session (host/tool.session, or exactly me) or to one tool on one machine
// (host/tool) is directed.
func broadcast(to, me string) bool { return tier(to, me) >= tierSlice }

// rank orders the messages of one priority class for a session start: the narrowest
// audience first, so a message to this session comes before one to everybody.
// Directed messages keep their arrival order (the oldest ask is the longest
// waiting); broadcasts go newest first, so the news that is still current is the news
// that gets a slot. Equal timestamps keep arrival order.
func rank(ms []msg.Message, me string) []msg.Message {
	out := slices.Clone(ms)
	slices.SortStableFunc(out, func(a, b msg.Message) int {
		ta, tb := tier(a.To, me), tier(b.To, me)
		if c := cmp.Compare(ta, tb); c != 0 {
			return c
		}
		if ta >= tierSlice {
			return b.At.Compare(a.At)
		}
		return 0
	})
	return out
}

// choose takes the messages a session start prints from ranked, which is ordered
// most important first: at most maxShown in all and at most maxBroadcasts of them
// broadcasts, so directed messages never compete with broadcasts for a slot.
// more counts the directed messages left out (by maxShown), waiting the
// broadcasts left out (by either cap). Whatever is left out is not shown and so
// stays unread.
func choose(ranked []msg.Message, me string) (shown []msg.Message, more, waiting int) {
	slots := 0
	for _, m := range ranked {
		bc := broadcast(m.To, me)
		switch {
		case len(shown) == maxShown || (bc && slots == maxBroadcasts):
			if bc {
				waiting++
			} else {
				more++
			}
		default:
			if bc {
				slots++
			}
			shown = append(shown, m)
		}
	}
	return shown, more, waiting
}

// countUrgent is how many of ms are marked urgent.
func countUrgent(ms []msg.Message) int {
	n := 0
	for _, m := range ms {
		if m.Priority() == msg.PrioUrgent {
			n++
		}
	}
	return n
}
