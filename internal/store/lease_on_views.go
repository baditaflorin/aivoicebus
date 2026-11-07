package store

// On keeps no managed message/reader/identity payload in the Store's resident
// maps. Selected public outputs belong to their caller and are read through
// from the same authenticated raw references as replication.
import (
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"sort"
	"time"
)

type managedAckView struct {
	receipts map[string]receiptSummary
	settles  map[string]time.Time
}

func (s *Store) managedAcksLocked(id string) managedAckView {
	out := managedAckView{receipts: map[string]receiptSummary{}, settles: map[string]time.Time{}}
	if !s.leaseOnLocked() || s.historyErr != nil {
		return out
	}
	err := s.history.visitAcks(id, s.logCounts, s.history.read, func(r msg.Record) error {
		a := *r.Ack
		if _, _, ok := msg.RetractedID(a.ID); ok {
			return nil
		}
		if _, ok := msg.AmendedID(a.ID); ok {
			return nil
		}
		if _, ok := msg.SettledID(a.ID); ok {
			if msg.IsHuman(a.By) && msg.ValidIdentity(a.By) {
				host := msg.HostOf(a.By)
				old, known := out.settles[host]
				if !known || a.At.Before(old) {
					out.settles[host] = a.At
				}
			}
			return nil
		}
		if a.ID != id {
			return nil
		}
		state, known := out.receipts[a.By]
		cur := state.Receipt
		if !known || a.At.Before(cur.At) || (cur.Via == msg.ViaHookCount && a.Via != msg.ViaHookCount) {
			at := a.At
			if known && cur.Via != msg.ViaHookCount && cur.At.Before(at) {
				at = cur.At
			}
			state.Receipt = msg.Receipt{By: a.By, At: at, Via: a.Via}
		}
		if !known && a.Via == msg.ViaHookCount {
			state.CountOnly = true
		}
		if a.Via != msg.ViaHookCount {
			state.CountOnly = false
		}
		out.receipts[a.By] = state
		return nil
	})
	if err != nil {
		s.historyErr = err
		return managedAckView{receipts: map[string]receiptSummary{}, settles: map[string]time.Time{}}
	}
	return out
}

func (s *Store) receiptViewLocked(id string) map[string]receiptSummary {
	if s.leaseOnLocked() {
		return s.managedAcksLocked(id).receipts
	}
	out := make(map[string]receiptSummary, len(s.acks[id]))
	for by := range s.acks[id] {
		rc, ok := s.ackAt[id][by]
		if !ok {
			rc = msg.Receipt{By: by}
		}
		out[by] = receiptSummary{Receipt: rc, CountOnly: s.countOnly[id][by]}
	}
	return out
}

func (s *Store) visitManagedLocked(minExpiry time.Time, visit func(internalLeaseRef, msg.Message)) {
	if !s.leaseOnLocked() || s.historyErr != nil {
		return
	}
	if minExpiry.IsZero() || minExpiry.Year() < 0 {
		minExpiry = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	var after []byte
	for {
		refs, next, err := s.history.internalLeasePage(minExpiry, after, 256)
		if err != nil {
			s.historyErr = err
			return
		}
		for _, ref := range refs {
			// During replay the sidecar already knows the full file; public views
			// must only see the captured/applied version vector.
			if ref.Seq > s.logCounts[ref.Origin] {
				continue
			}
			m, err := s.history.readInternalLease(ref)
			if err != nil {
				s.historyErr = err
				return
			}
			visit(ref, m)
			if s.historyErr != nil {
				return
			}
		}
		if next == nil {
			return
		}
		after = next
	}
}

func (s *Store) transientSentLocked() map[string]time.Time {
	out := make(map[string]time.Time, len(s.lastSent))
	for from, at := range s.lastSent {
		out[from] = at
	}
	if s.leaseOnLocked() {
		cutoff := s.now().UTC().Add(-s.leaseRetention.Keep)
		s.visitManagedLocked(cutoff, func(ref internalLeaseRef, m msg.Message) {
			if ref.Expiry.After(cutoff) && m.At.After(out[m.From]) {
				out[m.From] = m.At
			}
		})
	}
	return out
}

type orderedMessage struct {
	offset  int64
	message msg.Message
}

func (s *Store) allMessagesOnLocked(since *time.Time, views bool) []msg.Message {
	var ordered []orderedMessage
	for i, m := range s.msgs {
		if m.IsTest() || (since != nil && m.At.Before(*since)) {
			continue
		}
		_, offset, err := s.history.firstPosition(m.ID)
		if err != nil {
			s.historyErr = err
			return nil
		}
		if views {
			m = s.viewLocked(i)
		}
		ordered = append(ordered, orderedMessage{offset, m})
	}
	s.visitManagedLocked(time.Time{}, func(ref internalLeaseRef, m msg.Message) {
		if since != nil && m.At.Before(*since) {
			return
		}
		if views {
			m = s.amendViewLocked(s.tombstoneLocked(m), "")
		}
		ordered = append(ordered, orderedMessage{ref.Offset, m})
	})
	if s.historyErr != nil {
		return nil
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].offset < ordered[j].offset })
	out := make([]msg.Message, 0, len(ordered))
	for _, row := range ordered {
		out = append(out, row.message)
	}
	return out
}

func (s *Store) managedInboxLocked(me string, all bool, labels msg.Labels, ordinary []msg.Message, now time.Time) []msg.Message {
	if !s.leaseOnLocked() {
		return ordinary
	}
	out := ordinary
	// Only a prefix target from the older constant owner can reach another
	// runner session. Nonce self-addresses otherwise never reach another inbox.
	s.visitManagedLocked(time.Time{}, func(ref internalLeaseRef, m msg.Message) {
		if m.From == me || !msg.MatchesLabeled(m.To, me, labels) {
			return
		}
		r, withdrawn := s.retractionLocked(m.ID, m.From)
		if !all && (withdrawn || m.Expired(now) || s.closedForLocked(m, me) || s.supersededLocked(m) || s.replacedLocked(m)) {
			return
		}
		if !all {
			acks := s.managedAcksLocked(m.ID)
			if _, read := acks.receipts[me]; read {
				return
			}
			if _, settled := acks.settles[msg.HostOf(me)]; settled && msg.Settleable(m) {
				return
			}
		}
		if withdrawn {
			m = tombstone(m, r)
		}
		out = append(out, s.amendViewLocked(m, me))
	})
	if s.historyErr != nil {
		return nil
	}
	// Public output owns the selected rows. Store retains none of them.
	type positioned struct {
		message msg.Message
		offset  int64
	}
	ordered := make([]positioned, 0, len(out))
	for _, m := range out {
		_, offset, err := s.history.firstPosition(m.ID)
		if err != nil {
			s.historyErr = err
			return nil
		}
		ordered = append(ordered, positioned{m, offset})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].offset < ordered[j].offset })
	out = make([]msg.Message, 0, len(ordered))
	for _, row := range ordered {
		out = append(out, row.message)
	}
	return out
}
