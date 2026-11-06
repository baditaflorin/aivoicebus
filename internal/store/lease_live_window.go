package store

// The arrival-ordered live view is derived. Expired candidates may remain while
// an older request is pinned; retractions never remove candidates. The complete
// expiry/ID indexes and raw records survive every window move and clock change.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	historyLeaseLive   = []byte("internal-live-by-arrival")
	historyLeaseWindow = []byte("internal-live-window")
	historyWindowFloor = []byte("floor")
)

func liveLeaseKey(ref internalLeaseRef) ([]byte, error) {
	if ref.Offset < 0 {
		return nil, errors.New("lease history: negative arrival offset")
	}
	b := make([]byte, 8+len(ref.ID))
	binary.BigEndian.PutUint64(b, uint64(ref.Offset))
	copy(b[8:], ref.ID)
	return b, nil
}

func leaseWindowFloor(tx *bolt.Tx) (time.Time, error) {
	var floor time.Time
	b := tx.Bucket(historyLeaseWindow)
	if b == nil {
		return floor, errors.New("lease history: live window is missing")
	}
	value := b.Get(historyWindowFloor)
	if value == nil {
		return floor, errors.New("lease history: live window floor is missing")
	}
	err := json.Unmarshal(value, &floor)
	return floor, err
}

func putLiveLease(tx *bolt.Tx, ref internalLeaseRef, add bool) error {
	key, err := liveLeaseKey(ref)
	if err != nil {
		return err
	}
	b := tx.Bucket(historyLeaseLive)
	if b == nil {
		return errors.New("lease history: live arrival index is missing")
	}
	if add {
		if b.Get(key) != nil {
			return nil
		}
		if err := b.Put(key, []byte(ref.ID)); err != nil {
			return err
		}
		return b.SetSequence(b.Sequence() + 1)
	}
	if b.Get(key) == nil {
		return nil
	}
	if b.Sequence() == 0 {
		return errors.New("lease history: live view count underflow")
	}
	if err := b.Delete(key); err != nil {
		return err
	}
	return b.SetSequence(b.Sequence() - 1)
}

func (h *leaseHistory) refreshLiveWindow(now time.Time) error {
	wanted := h.minimumSelectionTime(now)
	var floor time.Time
	err := h.db.View(func(tx *bolt.Tx) error { var err error; floor, err = leaseWindowFloor(tx); return err })
	if err != nil || floor.Equal(wanted) {
		return err
	}
	start, end, add := floor, wanted, false
	if wanted.Before(floor) {
		start, end, add = wanted, floor, true
	}
	var cursor []byte
	finished := false
	for !finished {
		page, next, err := h.internalLeasePage(start, cursor, 256)
		if err != nil {
			return err
		}
		var changes []internalLeaseRef
		for _, ref := range page {
			if ref.Expiry.After(end) {
				finished = true
				break
			}
			// Metadata alone never authorizes dropping a potentially active hold.
			if _, err := h.readInternalLease(ref); err != nil {
				return err
			}
			if ref.Expiry.After(start) {
				changes = append(changes, ref)
			}
		}
		if len(changes) > 0 {
			if err := h.db.Update(func(tx *bolt.Tx) error {
				for _, ref := range changes {
					if err := putLiveLease(tx, ref, add); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if next == nil {
			break
		}
		cursor = next
	}
	return h.db.Update(func(tx *bolt.Tx) error {
		value, err := json.Marshal(wanted)
		if err != nil {
			return err
		}
		return tx.Bucket(historyLeaseWindow).Put(historyWindowFloor, value)
	})
}

// No read transaction, bbolt cursor, or mmap-backed bytes escape this page.
// after is an arrival key; rawEnd excludes records appended after the snapshot.
func (h *leaseHistory) liveLeasePage(after []byte, rawEnd int64, limit int) ([]internalLeaseRef, []byte, uint64, error) {
	if limit < 1 || limit > 256 {
		return nil, nil, 0, errors.New("lease history: invalid live page size")
	}
	var out []internalLeaseRef
	var next []byte
	var total uint64
	err := h.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(historyLeaseLive)
		leases := tx.Bucket(historyLeases)
		if b == nil || leases == nil {
			return errors.New("lease history: live indexes are missing")
		}
		total = b.Sequence()
		c := b.Cursor()
		key, id := c.First()
		if len(after) != 0 {
			key, id = c.Seek(after)
			if bytes.Equal(key, after) {
				key, id = c.Next()
			}
		}
		for ; key != nil; key, id = c.Next() {
			var ref internalLeaseRef
			if value := leases.Get(id); value == nil || json.Unmarshal(value, &ref) != nil || ref.ID != string(id) {
				return errors.New("lease history: live reference is missing or malformed")
			}
			want, err := liveLeaseKey(ref)
			if err != nil || !bytes.Equal(want, key) {
				return errors.New("lease history: live arrival reference differs")
			}
			if ref.Offset >= rawEnd {
				break
			}
			out = append(out, ref)
			if len(out) == limit {
				next = bytes.Clone(key)
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, 0, err
	}
	return out, next, total, nil
}
