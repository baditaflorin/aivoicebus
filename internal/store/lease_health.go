package store

import (
	"bytes"
	"errors"
	"os"

	bolt "go.etcd.io/bbolt"
)

var requiredHistoryBuckets = [][]byte{
	historyRecords, historyIDs, historyLeases, historyLeaseExpiry, historyAckTargets,
	historyLeaseLive, historyLeaseWindow, historyReceipts, historyTestReceipts,
	historyRetractionBy, historyManagedRetracted, historyManagedInert, historyDeferredTargets,
	historyReceiptCounts, historyArrivalTape, historyArrivalMeta, historyNoticeStates, historyNoticeReserved, historyReaderTargets, historyClaimDue,
}

func requireHistoryBuckets(tx *bolt.Tx) error {
	for _, name := range requiredHistoryBuckets {
		if tx.Bucket(name) == nil {
			return errors.New("lease history: required index is unavailable")
		}
	}
	return nil
}

func (s *Store) HealthChecked() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthLocked()
}

func (s *Store) healthLocked() error {
	if s.historyErr != nil {
		return s.historyErr
	}
	if s.history == nil {
		return nil
	}
	err := s.history.authorityHealth()
	if err != nil {
		s.historyErr = err
	}
	return err
}

func (h *leaseHistory) authorityHealth() error {
	fd, err := h.raw.Stat()
	path, pathErr := os.Lstat(h.rawPath)
	if err != nil || pathErr != nil || !path.Mode().IsRegular() || !historyOwned(path) || !os.SameFile(fd, path) || fd.Size() != h.rawEnd {
		return errors.New("lease history: raw authority is unavailable or changed")
	}
	return h.db.View(requireHistoryBuckets)
}

func (h *leaseHistory) firstPosition(id string) (historyPosition, int64, error) {
	var position historyPosition
	var offset int64
	err := h.db.View(func(tx *bolt.Tx) error {
		ids := tx.Bucket(historyIDs)
		if ids == nil {
			return errors.New("lease history: ID index is missing")
		}
		key := bytes.Clone(ids.Get([]byte(id)))
		if key == nil {
			return errors.New("lease history: held message ID is missing")
		}
		var err error
		position.origin, position.seq, err = decodeHistoryKey(key)
		if err != nil {
			return err
		}
		ref, err := historyReference(tx, position.origin, position.seq)
		if err != nil {
			return err
		}
		offset = ref.Offset
		return nil
	})
	return position, offset, err
}
