package store

import (
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Messages that replace messages (msg/wants.go): a message that names another in [supersedes: ID], or
// carries the same [key: K] to the same target from the same machine as an earlier one, settles the
// earlier one for every reader that has not read it. Both are judged when mail is read, from the
// messages themselves, so replication order does not matter, a retracted replacement replaces nothing
// (the earlier one is delivered again) and nothing is ever erased.

// indexWantsLocked notes what a held message says it replaces. Messages without such a line cost a
// substring test.
func (s *Store) indexWantsLocked(m msg.Message) {
	if !strings.Contains(m.Body, "\n[supersedes: ") && !strings.Contains(m.Body, "\n[key: ") {
		return
	}
	w := msg.WantsOf(m)
	if w.Supersedes != "" {
		s.supers[w.Supersedes] = append(s.supers[w.Supersedes], m.ID)
	}
	if w.Key != "" {
		k := msg.KeyIdent(m, w.Key)
		s.keyed[k] = append(s.keyed[k], m.ID)
		s.keyOf[m.ID] = k
	}
}

// replacedLocked reports whether a later message replaces m.
func (s *Store) replacedLocked(m msg.Message) bool {
	if len(s.supers) == 0 && len(s.keyed) == 0 {
		return false
	}
	valid := func(id string) bool {
		b, ok := s.byIDLocked(id)
		return ok && msg.Replaces(b, m) && !s.forged[b.ID] && !s.retractedLocked(b.ID, b.From)
	}
	for _, id := range s.supers[m.ID] {
		if valid(id) {
			return true
		}
	}
	if k, ok := s.keyOf[m.ID]; ok {
		for _, id := range s.keyed[k] {
			if id != m.ID && valid(id) {
				return true
			}
		}
	}
	return false
}
