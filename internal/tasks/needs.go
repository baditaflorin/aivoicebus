package tasks

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// `task --needs` and `ask --needs` send work to the session that can do it: one that has every
// capability asked for (msg.CapsOf: the repository it has checked out, its branch, tool, model,
// roles and declared words), preferring one listening now, then the one seen most recently. With
// none, nothing is sent and the closest candidates say what each lacks.

// ErrNoMatch is a --needs that no live session satisfies (NeedsPick.Closest lists the nearest).
var ErrNoMatch = errors.New("no live session has everything asked for") // nextstep: the callers list the closest candidates and what they lack

// MaxClosest is how many near misses a refusal lists.
const MaxClosest = 3

// Candidate is a session a --needs considered, with what it lacks and whether it is live.
type Candidate struct {
	msg.RosterEntry
	Lacks []string
	Live  bool
}

// NeedsPick is where the work goes and why, in one line; with ErrNoMatch, Closest is set instead.
type NeedsPick struct {
	To        string
	Why       string
	Listening bool
	Closest   []Candidate
}

// PickNeeds picks, among the sessions target reaches (all, a machine, host/tool), one that has
// every need: listening now first, then the most recently seen; a session is live when it listens
// or was seen within o.Within. Ties go to the most recently seen, so the same roster gives the
// same pick. Without one it returns ErrNoMatch with the MaxClosest nearest: fewest needs lacking,
// live before not, then most recent.
func PickNeeds(roster []msg.RosterEntry, target string, needs []string, o PickOpts) (NeedsPick, error) {
	if o.Within <= 0 {
		o.Within = AskWithin
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	var all, match []Candidate
	for _, e := range roster {
		if e.Me == o.Me || !msg.MatchesLabeled(target, e.Me, e.Labels) {
			continue
		}
		c := Candidate{RosterEntry: e, Lacks: msg.Lacks(e.Me, e.Labels, needs), Live: e.Listening || o.Now.Sub(e.Seen) <= o.Within}
		all = append(all, c)
		if c.Live && len(c.Lacks) == 0 {
			match = append(match, c)
		}
	}
	has := strings.Join(needs, ",")
	if len(match) == 0 {
		sort.SliceStable(all, func(i, j int) bool {
			a, b := all[i], all[j]
			switch {
			case len(a.Lacks) != len(b.Lacks):
				return len(a.Lacks) < len(b.Lacks)
			case a.Live != b.Live:
				return a.Live
			case a.Listening != b.Listening:
				return a.Listening
			case !a.Seen.Equal(b.Seen):
				return a.Seen.After(b.Seen)
			}
			return a.Me < b.Me
		})
		return NeedsPick{Closest: all[:min(len(all), MaxClosest)]},
			fmt.Errorf("%w: none of the %d session(s) at %s seen in the last %s has %s", ErrNoMatch, len(all), target, o.Within, has)
	}
	sort.SliceStable(match, func(i, j int) bool {
		a, b := match[i], match[j]
		if a.Listening != b.Listening {
			return a.Listening
		}
		if !a.Seen.Equal(b.Seen) {
			return a.Seen.After(b.Seen)
		}
		return a.Me < b.Me
	})
	e := match[0]
	of := ""
	if len(match) > 1 {
		of = fmt.Sprintf(" (of %d that have it)", len(match))
	}
	ago := o.Now.Sub(e.Seen).Round(time.Second).String() + " ago"
	why := fmt.Sprintf("has %s; listening now%s, it reads it within a second", has, of)
	if !e.Listening {
		why = fmt.Sprintf("has %s; none of those is listening, seen most recently (%s)%s: it sees it on its next prompt", has, ago, of)
	}
	return NeedsPick{To: e.Me, Why: why, Listening: e.Listening}, nil
}

// CandidateLine is one near miss for a refusal: the session, when it was seen, and what it lacks.
func CandidateLine(c Candidate, now time.Time) string {
	ago := now.Sub(c.Seen).Round(time.Second).String() + " ago"
	state := "seen " + ago
	switch {
	case c.Listening:
		state = "listening now"
	case !c.Live:
		state = "not live, seen " + ago
	}
	lacks := "has everything, but is not live"
	if len(c.Lacks) > 0 {
		lacks = "lacks " + strings.Join(c.Lacks, ",")
	}
	return fmt.Sprintf("%s (%s): %s", view.Name(c.Me, msg.MaxIdentity), state, lacks)
}
