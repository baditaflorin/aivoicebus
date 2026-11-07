package store

import (
	"strconv"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func (s *Store) leaseOnLocked() bool {
	return s.leaseRetention.Mode == LeaseRetentionOn && s.history != nil
}

func (s *Store) internalIDLocked(id string) bool {
	if !s.leaseOnLocked() {
		return false
	}
	_, managed, err := s.history.internalLease(id)
	if err != nil {
		s.historyErr = err
	}
	return managed
}

func (s *Store) deferAckLocked(a msg.Ack, key string) bool {
	if !s.leaseOnLocked() {
		return false
	}
	id := ackTarget(a)
	if id == "" {
		return false
	}
	managed := s.internalIDLocked(id)
	_, ordinary := s.idx[id]
	if s.historyErr == nil && !managed && ordinary {
		return false
	}
	if _, _, retract := msg.RetractedID(a.ID); retract {
		// Keep the preexisting bounded future-clock arrival clamp, without
		// retaining target/reader/retractor maps for unknown or internal IDs.
		if _, err := s.managedClaimDueLocked(key, a.At, true); err != nil {
			s.historyErr = err
		}
	}
	return true
}

func (s *Store) hydrateOrdinaryAcksLocked(id string) error {
	if !s.leaseOnLocked() {
		return nil
	}
	if _, already := s.idx[id]; already {
		return nil
	}
	pending, err := s.history.deferredTarget(id)
	if err != nil || !pending {
		return err
	}
	err = s.history.visitAcks(id, s.logCounts, s.history.read, func(r msg.Record) error {
		s.applyAckViewLocked(*r.Ack, r.Origin+"/"+strconv.FormatUint(r.Seq, 10))
		return nil
	})
	if err != nil {
		return err
	}
	return s.history.clearDeferredTarget(id)
}

func (s *Store) applyAckViewLocked(a msg.Ack, key string) {
	if mid, leak, ok := msg.RetractedID(a.ID); ok {
		s.addRetractionLocked(mid, a.By, a.At, leak, key)
	} else if aid, ok := msg.AmendedID(a.ID); ok {
		s.addAmendClaimLocked(aid, a.By, a.At)
	} else if sid, ok := msg.SettledID(a.ID); ok {
		s.addSettleLocked(sid, a.By, a.At)
	} else {
		s.addAck(a)
	}
	if a.ID == QuarantineID && a.By == QuarantineBy {
		s.quarantined++
	}
}
