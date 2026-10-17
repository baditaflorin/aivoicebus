package hook

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// The collision radar tells a session at its start that other sessions are working in the
// same repository, so two agents find out before they edit the same files rather than after.
// It reads the roster the nodes keep from every inbox read (and gossip between them): no new
// endpoint, so it works against any node that has /v1/roster.
//
// It is a hint, not a lock, and it misses sessions: one that has not read an inbox for 30
// minutes, one that never read from any node this client can reach, one whose node has not
// gossiped yet, two sessions of a tool started without a session id (they share an identity,
// so each takes the other for itself), and two clones of one repository that have no origin
// remote and different directory names. It can also name a session of another fork when
// neither clone has an origin remote. Silence therefore never means "alone".
const (
	radarWindow = 30 * time.Minute       // a session seen longer ago than this has probably ended
	radarSkew   = 2 * time.Minute        // a sighting further in the future is a wrong clock: ignored
	radarGrace  = 250 * time.Millisecond // how long the roster may lag the inbox read
	radarSlow   = time.Second            // an inbox read slower than this leaves the roster no extra time
	radarListed = 3                      // sessions named; the rest are counted
	radarIDLen  = 48                     // runes of a peer's identity printed
	radarBrLen  = 40                     // runes of a peer's branch printed
)

// unnamed stands for an identity that is not shaped like one: the roster is whatever
// sessions claimed, and the line is printed in the hook's own voice, so free text is not
// quoted into it.
const unnamed = "(unnamed session)"

// identityShape is host/agent[.session] in the characters an honest client produces.
var identityShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}/[a-z0-9][a-z0-9._-]{0,39}$`)

// radar is a roster read running beside the inbox read.
type radar struct {
	got   chan radarSeen
	start time.Time
}

// radarSeen is what the radar's reads returned: the roster, and the live leases (none from a node
// that predates leases).
type radarSeen struct {
	entries []msg.RosterEntry
	leases  []msg.Lease
}

// startRadar asks every node for the roster, in the background, on a client of its own (so
// it never races the inbox read's). The answers are merged (msg.MergeRosters), so this is
// the view of every node that answered, not of one. It returns nil, and costs nothing, when
// this session declares no repo.
func startRadar(labels msg.Labels, timeout time.Duration, now time.Time) *radar {
	if labels["repo"] == "" {
		return nil
	}
	c, err := client.FromEnv()
	if err != nil {
		return nil
	}
	c.Timeout = timeout
	c.Warn = io.Discard
	r := &radar{got: make(chan radarSeen, 1), start: now}
	go func() {
		defer func() { _ = recover() }()
		entries, err := c.Roster()
		if err != nil {
			entries = nil
		}
		leases, _ := c.Leases()
		r.got <- radarSeen{entries, leases}
	}()
	return r
}

// print writes the radar's lines to out. The roster request was made together with the
// inbox read, so it is normally there already; if not, the session start waits for it at
// most radarGrace more, and only when the inbox read was healthy and quick: a failed one
// says the nodes are not answering, and a slow one (radarSlow) has already cost the session
// start enough. It never waits past the hook's own deadline either. A roster that is late
// or failed prints nothing.
func (r *radar) print(out io.Writer, me string, labels msg.Labels, deadline time.Duration, inboxFailed bool, inboxTook time.Duration) {
	if r == nil {
		return
	}
	wait := radarGrace
	if inboxFailed || inboxTook >= radarSlow {
		wait = 0
	}
	wait = max(0, min(wait, time.Until(r.start.Add(deadline-radarGrace))))
	select {
	case seen := <-r.got:
		for _, line := range radarLines(seen.entries, me, labels, time.Now()) {
			fmt.Fprintln(out, line)
		}
		for _, line := range leaseLines(seen.leases, me, labels["repo"]) {
			fmt.Fprintln(out, line)
		}
	case <-time.After(wait):
	}
}

// sameRepo reports whether a session with labels theirs works in the repository mine does:
// by owner/name when both declare an origin (so two repositories that share a name do not
// match), else by name.
func sameRepo(mine, theirs msg.Labels) bool {
	if mine["origin"] != "" && theirs["origin"] != "" {
		return mine["origin"] == theirs["origin"]
	}
	return theirs["repo"] == mine["repo"]
}

// radarLines is what the radar says about entries (one line, or none): the other sessions
// seen within radarWindow that declare this session's repo, newest first, at most
// radarListed of them. Every string a peer chose goes through a strict shape or
// view.OneLine and is cut, because the roster is whatever sessions claimed about themselves.
func radarLines(entries []msg.RosterEntry, me string, labels msg.Labels, now time.Time) []string {
	if labels["repo"] == "" {
		return nil
	}
	var peers []msg.RosterEntry
	for _, e := range entries {
		age := now.Sub(e.Seen)
		if e.Me == me || !sameRepo(labels, e.Labels) || age > radarWindow || age < -radarSkew {
			continue
		}
		peers = append(peers, e)
	}
	if len(peers) == 0 {
		return nil
	}
	slices.SortStableFunc(peers, func(a, b msg.RosterEntry) int { return b.Seen.Compare(a.Seen) })
	var parts []string
	for _, e := range peers[:min(len(peers), radarListed)] {
		var notes []string
		// An honest node cleans labels as a session declares them; one learnt by gossip
		// from a peer may not have been, so clean it again here.
		switch br := msg.CleanValue(e.Labels["branch"]); {
		case br == "":
		case br == labels["branch"]:
			notes = append(notes, "same branch")
		default:
			notes = append(notes, "branch "+view.OneLine(br, radarBrLen))
		}
		machine := msg.CleanValue(e.Labels["machine"])
		if machine == "" {
			machine = msg.CleanValue(msg.HostOf(e.Me))
		}
		if machine != "" && machine == labels["machine"] {
			notes = append(notes, "this machine")
		}
		notes = append(notes, ago(now.Sub(e.Seen)))
		id := unnamed
		if identityShape.MatchString(e.Me) {
			id = view.OneLine(e.Me, radarIDLen)
		}
		parts = append(parts, id+view.TitleSuffix(e.Title)+" ("+strings.Join(notes, ", ")+")")
	}
	line := fmt.Sprintf("[aivoicebus] %d other session(s) on this repo: %s", len(peers), strings.Join(parts, ", "))
	if more := len(peers) - radarListed; more > 0 {
		line += fmt.Sprintf(", +%d more (`aivoicebus who @repo=%s`)", more, labels["repo"])
	}
	return []string{line + "."}
}

// ago is d as a short age: "just now", "4m ago".
func ago(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	return fmt.Sprintf("%dm ago", int(d/time.Minute))
}

// leaseLines names the leases other sessions hold (see `aivoicebus lease`), those whose key
// mentions this repository first, at most radarListed of them.
func leaseLines(live []msg.Lease, me, repo string) []string {
	var mine, other []msg.Lease
	for _, h := range msg.Holders(live) {
		if h.By == me {
			continue
		}
		if repo != "" && strings.Contains(strings.ToLower(h.Key), strings.ToLower(repo)) {
			mine = append(mine, h)
		} else {
			other = append(other, h)
		}
	}
	all := append(mine, other...)
	if len(all) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("[aivoicebus] Held by other sessions (leases; check before you edit these):")
	for i, h := range all {
		if i == radarListed {
			fmt.Fprintf(&b, " and %d more (aivoicebus lease list)", len(all)-i)
			break
		}
		who := unnamed
		if identityShape.MatchString(h.By) {
			who = h.By
		}
		fmt.Fprintf(&b, " %s by %s until %s;", view.OneLine(h.Key, 120), who, h.Until.Local().Format("15:04"))
	}
	return []string{strings.TrimSuffix(b.String(), ";")}
}
