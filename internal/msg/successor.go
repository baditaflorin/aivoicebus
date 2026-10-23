package msg

import (
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// Successors. A session ends without reading what was written to it (a reply to its own question,
// most often), and the session that replaces it never sees the message. A message the sender marked
// "may go to the successor" is handed, by the node that is asked, to the session that replaces the
// one it names, once that one has been gone for GoneAfter. Nothing about it is a field of the wire
// format. What marks the message is its id, as with the test lane:
//
//	5c + seven random hex digits + one check digit  (the sum of the nine digits before it, mod 16)
//
// The mark takes 12 of the id's 40 bits, which leaves 28 random ones: a node keeps the first of two
// messages that share an id and ignores the second, so the more bits the mark takes, the likelier
// that happens (the test lane's 20 are too few for a mark that replies carry by default). An older
// client's random id has this shape one time in 4096, and then it is only a message its sender did
// not mean to hand on, handed on once, when everything else in Inheritable holds.
//
// It is still ten lowercase hex digits, so ValidID, the replication check, the signature (the id is
// signed, so no node can add or strip the mark) and the id-prefix resolver are unchanged. A node
// that does not know the shape delivers the message to the session it names and to nobody else,
// as it always did. A mark is a sender's wish; Inheritable says what a node honours.
//
// The hand-over is a copy. The session named keeps its own copy and reads it if it comes back (a
// session in a long run with no prompt looks gone after GoneAfter), and what records that the
// successor has it is its read receipt, which every node already replicates: a message that
// somebody else read is no longer unread, so a second successor is not given it.
const (
	SuccessorIDPrefix = "5c"

	// GoneAfter is how long a session is not heard from, outside a wait, before its mail may be
	// handed to its successor. Shorter than the 30 minutes a send to one session tolerates.
	GoneAfter = 15 * time.Minute

	// FeatureSuccessor is what a node advertises when it hands marked mail to a successor. A client
	// that sends a mark to a mesh with an older node says so: that node does not hand it on.
	FeatureSuccessor = "successor"
)

// NewSuccessorID returns a fresh id that marks a message as one the sender lets go to the
// successor of the session it names (see above).
func NewSuccessorID() string {
	b := make([]byte, 4)
	randRead(b)
	nine := SuccessorIDPrefix + hex.EncodeToString(b)[:9-len(SuccessorIDPrefix)]
	return nine + string(testCheck(nine))
}

// IsSuccessorID reports whether id has the shape of a marked id.
func IsSuccessorID(id string) bool {
	if len(id) != 10 || !strings.HasPrefix(id, SuccessorIDPrefix) {
		return false
	}
	for i := 0; i < len(id); i++ {
		if nibble(id[i]) < 0 {
			return false
		}
	}
	return testCheck(id[:9]) == id[9]
}

// ForSuccessor reports whether the sender marked m as one that may go to the successor of the
// session it names. Alone it does not make m inheritable (see Inheritable).
func (m Message) ForSuccessor() bool { return IsSuccessorID(m.ID) }

// IsPersistentSession reports whether to names a session by a long-lived name (codex.build1 or a
// name that is no tool's), which whoever wears it after a restart is: its mail waits for the name.
func IsPersistentSession(to string) bool {
	_, agent, ok := strings.Cut(to, "/")
	if !ok {
		return false
	}
	if !strings.Contains(agent, ".") {
		return OneSession(to) // host/build1
	}
	return ValidPersistentName(strings.ToLower(agent)) == nil
}

// Inheritable reports whether a node may hand m to the successor of the session it is addressed to,
// whatever the roster says: it is marked; it is for exactly one session by its automatic name; it is
// a note, a question or an answer (never a task, a claim, progress, a lease or an end, which are the
// bookkeeping of one conversation); it is not for the person on the machine, even when it was sent
// to one session by name; it is not a test, a join probe or a node's own notice.
func (m Message) Inheritable() bool {
	switch m.Kind {
	case "", KindQuestion, KindAnswer:
	default:
		return false
	}
	return m.ForSuccessor() && OneSession(m.To) && !IsPersistentSession(m.To) && !IsProbe(m.To) &&
		!IsSystemSender(m.From) && !PersonTagged(m) && !m.IsTest() && !strings.EqualFold(m.From, m.To)
}

// InheritedFrom is whom a message in the inbox of me was written to, when it was not me: the
// session it was handed on from. Only a message to one exact session that does not reach me by the
// ordinary rules counts: a message to host/tool reaches every session of that tool (and host/name
// every session whose agent starts with name), and nobody's mail was handed on there.
func InheritedFrom(m Message, me string) (string, bool) {
	if !OneSession(m.To) || MatchesLabeled(m.To, me, nil) {
		return "", false
	}
	return m.To, true
}

// Gone reports whether a session has not been heard from for GoneAfter and is not in a wait.
// A sighting ahead of now (a skewed clock) is not gone.
func Gone(e RosterEntry, now time.Time) bool {
	return !e.Listening && now.Sub(e.Seen) >= GoneAfter
}

func toolOf(e RosterEntry) string {
	if t := e.Labels["tool"]; t != "" {
		return strings.ToLower(t)
	}
	_, agent, _ := strings.Cut(e.Me, "/")
	tool, _, _ := strings.Cut(strings.ToLower(agent), ".")
	return tool
}

// sameWork reports whether b works where a worked: the same tool and the same repository, which
// must be named (two sessions in no repository have nothing in common), and the same origin when
// both say what theirs is (two repositories can share a name).
func sameWork(a, b RosterEntry) bool {
	repo := a.Labels["repo"]
	if repo == "" || repo != b.Labels["repo"] || toolOf(a) != toolOf(b) {
		return false
	}
	o1, o2 := a.Labels["origin"], b.Labels["origin"]
	return o1 == "" || o2 == "" || o1 == o2
}

func shareRole(a, b RosterEntry) bool {
	mine := RolesOf(b.Labels)
	for _, r := range RolesOf(a.Labels) {
		for _, x := range mine {
			if r == x {
				return true
			}
		}
	}
	return false
}

// Successor chooses the session that takes over from gone, among the roster's entries: live (not
// Gone), on the same machine (a role or a repository label is a session's own word, and only the
// sessions of one machine may already read each other's mail), never gone itself or a join probe;
// one that claims a role gone claimed before one that works in its repository; and among those the
// newest, by when since says each was first heard of (zero for an unknown one, which sorts last),
// then the most recently seen, then the name. A session with no repository and no role has no
// successor. A persistent name is not looked up here: see IsPersistentSession.
func Successor(gone RosterEntry, roster []RosterEntry, since func(me string) time.Time, now time.Time) (RosterEntry, bool) {
	host := HostOf(gone.Me)
	type cand struct {
		e    RosterEntry
		role bool
		at   time.Time
	}
	var cs []cand
	for _, e := range roster {
		if e.Me == gone.Me || HostOf(e.Me) != host || IsProbe(e.Me) || Gone(e, now) {
			continue
		}
		role := shareRole(gone, e)
		if !role && !sameWork(gone, e) {
			continue
		}
		c := cand{e: e, role: role}
		if since != nil {
			c.at = since(e.Me)
		}
		cs = append(cs, c)
	}
	if len(cs) == 0 {
		return RosterEntry{}, false
	}
	sort.Slice(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		switch {
		case a.role != b.role:
			return a.role
		case !a.at.Equal(b.at):
			return a.at.After(b.at)
		case !a.e.Seen.Equal(b.e.Seen):
			return a.e.Seen.After(b.e.Seen)
		}
		return a.e.Me < b.e.Me
	})
	return cs[0].e, true
}
