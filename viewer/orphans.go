package viewer

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Orphan is a message to one session that has been gone for msg.GoneAfter, and what became of it:
// nobody has read it, or a session other than the one it names did (the nodes hand a marked message to
// the session that replaces the one that ended, see msg/successor.go). The lane is the view's answer
// to "was my message dropped?", which nothing else on the page shows.
type Orphan struct {
	ID        string    `json:"id"`
	Thread    string    `json:"thread"`
	From      string    `json:"from"`
	FromTitle string    `json:"from_title,omitempty"`
	To        string    `json:"to"`
	ToTitle   string    `json:"to_title,omitempty"`
	At        time.Time `json:"at"`
	Line      string    `json:"line"` // the first line of the text, as a reader is shown it
	// LastSeen is when the session it names was last seen; absent when the roster has forgotten it.
	LastSeen *time.Time `json:"last_seen,omitempty"`
	// Handover: the sender let the nodes hand it to the session that replaces the one it names.
	Handover bool `json:"handover"`
	// Successor is who would get it now when it is a Handover nobody has read; empty when nobody
	// qualifies yet (it waits for the next session in that repository).
	Successor string `json:"successor,omitempty"`
	// ReadBy is the session that read it in place of the one it names, when one did.
	ReadBy string `json:"read_by,omitempty"`
}

// orphanKinds are the messages that can be left unread by a session that ended: notes, questions,
// answers and tasks. A claim, progress, a lease and an end are the board's own bookkeeping.
func orphanKind(k string) bool {
	return k == "" || k == msg.KindQuestion || k == msg.KindAnswer || k == msg.KindTask
}

// orphanWindow is how long a handed-on message stays in the lane after it was read: long enough to
// see that it got through, short enough that the lane is about what needs a look.
const orphanWindow = 24 * time.Hour

const maxOrphans = 200

// Orphans lists the messages in ms to a session that is gone (see msg.Gone) and that its own session
// never read, newest first: unread ones, and ones another session read within orphanWindow. A session
// the roster does not know counts as gone. receipts says who read a message (store.Receipts).
func Orphans(roster []msg.RosterEntry, ms []msg.Message, receipts func(id string) (msg.Receipts, bool), now time.Time) []Orphan {
	known := map[string]msg.RosterEntry{}
	for _, e := range roster {
		known[e.Me] = e
	}
	var out []Orphan
	for i := len(ms) - 1; i >= 0 && len(out) < maxOrphans; i-- {
		m := ms[i]
		if m.Retracted != nil || !orphanKind(m.Kind) || !msg.OneSession(m.To) || msg.IsProbe(m.To) || msg.IsSystemSender(m.From) || m.IsTest() || m.Expired(now) {
			continue
		}
		if e, ok := known[m.To]; ok && !msg.Gone(e, now) {
			continue
		} else if !ok && !strings.Contains(m.To, ".") {
			continue // host/word is a tool or a name no roster entry wears, not a session that is gone
		}
		rc, ok := receipts(m.ID)
		if !ok {
			continue
		}
		o := Orphan{ID: clean(m.ID, msg.MaxRef), Thread: clean(m.Thread, msg.MaxRef), From: clean(m.From, nameShown), FromTitle: msg.CleanTitle(m.FromTitle),
			To: clean(m.To, nameShown), At: m.At, Line: firstLine(m), Handover: m.Inheritable()}
		if e, ok := known[m.To]; ok {
			t := e.Seen
			o.LastSeen, o.ToTitle = &t, msg.CleanTitle(e.Title)
		}
		readers := slices.DeleteFunc(slices.Clone(rc.Reads), func(r msg.Receipt) bool { return r.By == m.From })
		switch {
		case slices.ContainsFunc(readers, func(r msg.Receipt) bool { return r.By == m.To }):
			continue // the session it names read it: it is not orphaned
		case len(readers) > 0:
			if now.Sub(readers[0].At) > orphanWindow {
				continue
			}
			o.ReadBy = clean(readers[0].By, nameShown)
		case o.Handover && o.LastSeen != nil:
			others := slices.DeleteFunc(slices.Clone(roster), func(e msg.RosterEntry) bool { return e.Me == m.From })
			if next, ok := msg.Successor(known[m.To], others, nil, now); ok {
				o.Successor = clean(next.Me, nameShown)
			}
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// firstLine is the first non-empty line of a message as a reader would see it, cut to one short line.
func firstLine(m msg.Message) string {
	if msg.IsSealed(m.Body) {
		return "(a private message)"
	}
	for _, l := range strings.Split(m.Body, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return clean(l, 140)
		}
	}
	return ""
}
