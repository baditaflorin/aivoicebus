package store

import (
	"slices"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Mail handed to a successor (msg/successor.go). The node that answers a read decides, from what it
// knows of the roster: a message marked for the successor, to one session that has been gone for
// msg.GoneAfter and never read by anybody, is in the inbox of the session that replaces it. Nothing
// is written for it: the successor's own read receipt is what stops it from being handed on again,
// and the session it was written to keeps its copy.

// indexSuccessorLocked records message i (m) when a node may hand it to a successor: by machine, then by
// the session it names, so a read looks only at the sessions of its own machine that have such mail.
func (s *Store) indexSuccessorLocked(i int, m msg.Message) {
	if !m.Inheritable() {
		return
	}
	host := msg.HostOf(m.To)
	if s.succ[host] == nil {
		s.succ[host] = map[string][]int{}
	}
	s.succ[host][m.To] = append(s.succ[host][m.To], i)
}

// entryLocked is what this node knows of one identity as a roster entry: when it was last seen
// (a send counts, as in Roster), its labels, and whether it is in a wait.
func (s *Store) entryLocked(me string, now time.Time) (msg.RosterEntry, bool) {
	v, known := s.seen[me]
	e := msg.RosterEntry{Me: me, Seen: v.at, Labels: v.labels, Title: v.title}
	e.Listening, e.Waits = s.listenLocked(me, now)
	if at, sent := s.lastSent[me]; sent && at.After(e.Seen) {
		e.Seen = at
		known = true
	}
	return e, known
}

// hostRosterLocked is the roster entries of one machine, for choosing a successor among them.
func (s *Store) hostRosterLocked(host string, now time.Time) []msg.RosterEntry {
	var out []msg.RosterEntry
	for me := range s.seen {
		if msg.HostOf(me) != host {
			continue
		}
		if e, ok := s.entryLocked(me, now); ok {
			out = append(out, e)
		}
	}
	return out
}

// readByOthersLocked reports whether anybody but the sender has read m (the quarantine placeholder
// is nobody).
func (s *Store) readByOthersLocked(m msg.Message) bool {
	for by := range s.acks[m.ID] {
		if by != m.From && by != QuarantineBy {
			return true
		}
	}
	return false
}

// inheritedLocked lists the messages, by index, that are in me's inbox because the session they name
// is gone and me is its successor: unread ones, or with all every one still held. Reading is the
// same as for any message of the inbox, and the loop that calls this applies its filters (withdrawn,
// expired, replaced, acknowledged).
func (s *Store) inheritedLocked(me string, all bool, now time.Time) []int {
	byTo := s.succ[msg.HostOf(me)]
	if len(byTo) == 0 {
		return nil
	}
	var roster []msg.RosterEntry
	var out []int
	taken := func(gone msg.RosterEntry, from string) string { // who takes over, the sender never being the one
		others := slices.DeleteFunc(slices.Clone(roster), func(e msg.RosterEntry) bool { return e.Me == from })
		next, _ := msg.Successor(gone, others, func(who string) time.Time { return s.seen[who].since }, now)
		return next.Me
	}
	for to, idxs := range byTo {
		var pending []int
		for _, i := range idxs {
			if m := s.msgs[i]; m.From != me && (all || (!s.readByOthersLocked(m) && !m.Expired(now))) {
				pending = append(pending, i)
			}
		}
		if len(pending) == 0 {
			continue
		}
		gone, known := s.entryLocked(to, now)
		if !known || !msg.Gone(gone, now) {
			continue
		}
		if roster == nil {
			roster = s.hostRosterLocked(msg.HostOf(me), now)
		}
		successor := map[string]string{} // by sender
		for _, i := range pending {
			from := s.msgs[i].From
			if _, done := successor[from]; !done {
				successor[from] = taken(gone, from)
			}
			if successor[from] == me {
				out = append(out, i)
			}
		}
	}
	slices.Sort(out)
	return out
}
