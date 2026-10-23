package tasks

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// `ask` sends a question to one session and waits for its answer. Which session is decided here,
// from the roster, so the CLI and the MCP tool pick alike: one listening now (in a wait, or a
// worker holding a read open) before one that was only seen, and never a guess between two that
// nothing tells apart.

// The limits an ask starts its conversation with, and how long it waits for someone to take it.
const (
	AskMaxTurns    = 4
	AskDeadline    = 15 * time.Minute
	AskClaimWithin = 2 * time.Minute
	AskWithin      = outbox.LiveWithin // a session seen this recently is live (send refuses a stale one at the same limit)
	AskAlike       = 2 * time.Minute   // two sessions seen this close together are not told apart by it
)

// ErrNobody is an ask with no live session to go to.
var ErrNobody = errors.New("nobody there") // nextstep: the callers say what happens instead

// ErrAlike is an ask that would have to guess between sessions (Pick.Alike lists them).
var ErrAlike = errors.New("sessions look alike") // nextstep: the callers list the sessions and how to pick one

// PickOpts says how to pick: Any takes any of several that look alike, Pick (1-based) names one
// of them, Within is how recently a session must have been seen to count as live.
type PickOpts struct {
	Me     string
	Any    bool
	Pick   int
	Within time.Duration
	Now    time.Time
	Rand   func(n int) int // for Any among several listening; nil takes the first
}

// Pick is where an ask goes and why. Alike is set with ErrAlike, in --pick order.
type Pick struct {
	To        string
	Why       string
	Listening bool
	Alike     []msg.RosterEntry
}

// ForTaskFrom is roster with Listening set to whether each session's wait would take a task from
// me right now (msg.RosterEntry.Accepts): one waiting for a reply in its own thread is not
// listening for an ask, one waiting for tasks only, or for me, is. The roster is not changed.
func ForTaskFrom(roster []msg.RosterEntry, me string) []msg.RosterEntry {
	probe := msg.Message{Kind: msg.KindTask, From: me}
	out := make([]msg.RosterEntry, len(roster))
	for i, e := range roster {
		e.Listening = e.Accepts(probe)
		out[i] = e
	}
	return out
}

// IsSession reports whether to names exactly one session (host/tool.session).
func IsSession(to string) bool {
	h, a, ok := strings.Cut(to, "/")
	return ok && h != "*" && h != "" && !msg.IsSelector(to) && strings.Contains(a, ".") && !strings.Contains(a, "*")
}

// PickSession picks the one session an ask for target goes to: a full session id as named; else
// the one listening now; else, with none listening, the one seen most recently. Several listening,
// or several seen within AskAlike of the latest, look alike: ErrAlike, unless Any or Pick.
func PickSession(roster []msg.RosterEntry, target string, o PickOpts) (Pick, error) {
	if o.Within <= 0 {
		o.Within = AskWithin
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	ago := func(e msg.RosterEntry) string { return o.Now.Sub(e.Seen).Round(time.Second).String() + " ago" }
	roster = ForTaskFrom(roster, o.Me)
	if IsSession(target) {
		if o.Pick > 0 {
			return Pick{}, fmt.Errorf("--pick chooses among sessions that look alike; %s already names one: run it again without --pick", target)
		}
		for _, e := range roster {
			if e.Me != target {
				continue
			}
			if e.Listening {
				return Pick{To: target, Listening: true, Why: "named; listening now, it reads the question within a second"}, nil
			}
			if b := e.Busy(); b != "" && o.Now.Sub(e.Seen) <= o.Within {
				return Pick{To: target, Why: "named; busy, " + b + ": it sees the question on its next prompt"}, nil
			}
			if o.Now.Sub(e.Seen) <= o.Within {
				return Pick{To: target, Why: "named; not listening, seen " + ago(e) + ": it sees the question on its next prompt"}, nil
			}
		}
		return Pick{}, fmt.Errorf("%w: %s was not seen in the last %s", ErrNobody, target, o.Within)
	}
	var live []msg.RosterEntry
	for _, e := range roster {
		if e.Me != o.Me && msg.MatchesLabeled(target, e.Me, e.Labels) && (e.Listening || o.Now.Sub(e.Seen) <= o.Within) {
			live = append(live, e)
		}
	}
	if len(live) == 0 {
		return Pick{}, fmt.Errorf("%w: no session at %s was seen in the last %s", ErrNobody, target, o.Within)
	}
	var tier []msg.RosterEntry
	for _, e := range live {
		if e.Listening {
			tier = append(tier, e)
		}
	}
	listening := len(tier) > 0
	if !listening {
		sort.Slice(live, func(i, j int) bool { return live[i].Seen.After(live[j].Seen) })
		for _, e := range live {
			if live[0].Seen.Sub(e.Seen) <= AskAlike {
				tier = append(tier, e)
			}
		}
	}
	sort.Slice(tier, func(i, j int) bool { return tier[i].Me < tier[j].Me }) // the --pick order, the same on every run
	how := "seen within " + AskAlike.String() + " of each other, none listening"
	if listening {
		how = "listening now"
	}
	var e msg.RosterEntry
	var why string
	switch {
	case o.Pick > len(tier):
		return Pick{Alike: tier}, fmt.Errorf("--pick %d: there are %d to pick from (%s): run it again with one of them", o.Pick, len(tier), how)
	case o.Pick > 0:
		e, why = tier[o.Pick-1], fmt.Sprintf("--pick %d of %d %s", o.Pick, len(tier), how)
	case len(tier) == 1 && listening:
		e, why = tier[0], "listening now, it reads the question within a second"
	case len(tier) == 1:
		e, why = tier[0], fmt.Sprintf("nobody there is listening; seen most recently (%s), it sees the question on its next prompt", ago(tier[0]))
	case !o.Any:
		return Pick{Alike: tier}, ErrAlike
	case listening:
		n := 0
		if o.Rand != nil {
			n = o.Rand(len(tier))
		}
		e, why = tier[n], fmt.Sprintf("--any: picked from %d listening now", len(tier))
	default:
		e = live[0]
		why = fmt.Sprintf("--any: nobody listening; seen most recently (%s) of %d", ago(e), len(tier))
	}
	return Pick{To: e.Me, Why: why, Listening: e.Listening}, nil
}

// TakenBy is who has m in hand and how: the session that claimed it, or, for a message to one
// session, that session once it read it (a session asked a question may answer without
// claiming). known is false when the nodes could not say.
func TakenBy(c *client.Client, m msg.Message) (by, how string, known bool) {
	r, err := c.Receipts(m.ID)
	if err != nil {
		return "", "", false
	}
	if r.Owner != nil {
		return r.Owner.By, "claimed it", true
	}
	if IsSession(m.To) {
		for _, rd := range r.Reads {
			if rd.By == m.To {
				return rd.By, "read it", true
			}
		}
	}
	return "", "", true
}
