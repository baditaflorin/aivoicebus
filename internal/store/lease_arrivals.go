package store

// LastHour is local arrival order, not message send time. Keep its exact
// ordering/cutoff semantics without one resident timestamp per message. In On all non-test arrivals
// share this tape, so a clock rollback cannot split its prefix semantics across
// ordinary and managed records.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	historyArrivalTape  = []byte("local-managed-arrival-tape")
	historyArrivalMeta  = []byte("local-managed-arrival-count")
	historyArrivalCount = []byte("count")
)

type arrivalSegment struct {
	At        time.Time
	Delta     int64
	Monotonic bool
	Count     uint64
}

func hasMonotonic(t time.Time) bool { return t != t.Round(0) }

func (s *Store) noteManagedArrivalLocked(at time.Time) {
	if s.managedArrivalHot == nil {
		s.managedArrivalHot = new([256]arrivalSegment)
	}
	if !s.managedArrivalBaseSet {
		s.managedArrivalBase = at
		s.managedArrivalBaseSet = true
	}
	segment := arrivalSegment{At: at.UTC(), Delta: int64(at.Sub(s.managedArrivalBase)), Monotonic: hasMonotonic(at) && hasMonotonic(s.managedArrivalBase), Count: 1}
	if n := s.managedArrivalN; n > 0 {
		last := &s.managedArrivalHot[n-1]
		if last.At.Equal(segment.At) && last.Monotonic == segment.Monotonic && (!segment.Monotonic || last.Delta == segment.Delta) {
			last.Count++
			return
		}
	}
	if s.managedArrivalN == len(s.managedArrivalHot) {
		if err := s.flushManagedArrivalsLocked(); err != nil {
			s.historyErr = err
			return
		}
	}
	s.managedArrivalHot[s.managedArrivalN] = segment
	s.managedArrivalN++
}

func arrivalDiskCount(tx *bolt.Tx) (uint64, error) {
	b := tx.Bucket(historyArrivalMeta)
	if b == nil {
		return 0, errors.New("lease history: local arrival metadata is missing")
	}
	value := b.Get(historyArrivalCount)
	if len(value) != 8 {
		return 0, errors.New("lease history: local arrival count is malformed")
	}
	return binary.BigEndian.Uint64(value), nil
}

func putArrivalCount(tx *bolt.Tx, count uint64) error {
	var value [8]byte
	binary.BigEndian.PutUint64(value[:], count)
	return tx.Bucket(historyArrivalMeta).Put(historyArrivalCount, value[:])
}

func (s *Store) flushManagedArrivalsLocked() error {
	if s.managedArrivalN == 0 {
		return nil
	}
	if s.history == nil {
		return errors.New("lease history: local arrival index is unavailable")
	}
	err := s.history.db.Update(func(tx *bolt.Tx) error {
		if err := requireHistoryBuckets(tx); err != nil {
			return err
		}
		count, err := arrivalDiskCount(tx)
		if err != nil {
			return err
		}
		b := tx.Bucket(historyArrivalTape)
		for _, segment := range s.managedArrivalHot[:s.managedArrivalN] {
			key, err := b.NextSequence()
			if err != nil {
				return err
			}
			var encodedKey [8]byte
			binary.BigEndian.PutUint64(encodedKey[:], key)
			value, err := json.Marshal(segment)
			if err != nil {
				return err
			}
			if err := b.Put(encodedKey[:], value); err != nil {
				return err
			}
			if ^uint64(0)-count < segment.Count {
				return errors.New("lease history: arrival count overflow")
			}
			count += segment.Count
		}
		return putArrivalCount(tx, count)
	})
	if err == nil {
		clear(s.managedArrivalHot[:s.managedArrivalN])
		s.managedArrivalN = 0
	}
	return err
}

func (segment arrivalSegment) after(cutoff, base time.Time) bool {
	if segment.Monotonic && hasMonotonic(cutoff) && hasMonotonic(base) {
		return segment.Delta > int64(cutoff.Sub(base))
	}
	return segment.At.After(cutoff)
}

func (s *Store) managedLastHourLocked(cutoff time.Time) (uint64, error) {
	if err := s.flushManagedArrivalsLocked(); err != nil {
		return 0, err
	}
	var count uint64
	var prune bool
	err := s.history.db.View(func(tx *bolt.Tx) error {
		var err error
		count, err = arrivalDiskCount(tx)
		if err != nil {
			return err
		}
		b := tx.Bucket(historyArrivalTape)
		if b == nil {
			return errors.New("lease history: arrival tape is missing")
		}
		key, value := b.Cursor().First()
		if key == nil {
			if count != 0 {
				return errors.New("lease history: arrival count differs from tape")
			}
			return nil
		}
		var segment arrivalSegment
		if err := json.Unmarshal(value, &segment); err != nil {
			return err
		}
		prune = !segment.after(cutoff, s.managedArrivalBase)
		return nil
	})
	if err != nil || !prune {
		return count, err
	}
	for {
		removed := 0
		err = s.history.db.Update(func(tx *bolt.Tx) error {
			var err error
			count, err = arrivalDiskCount(tx)
			if err != nil {
				return err
			}
			c := tx.Bucket(historyArrivalTape).Cursor()
			for key, value := c.First(); key != nil && removed < 256; key, value = c.First() {
				var segment arrivalSegment
				if err := json.Unmarshal(value, &segment); err != nil {
					return err
				}
				if segment.after(cutoff, s.managedArrivalBase) {
					break
				}
				if segment.Count > count {
					return errors.New("lease history: arrival count underflow")
				}
				count -= segment.Count
				if err := c.Delete(); err != nil {
					return err
				}
				removed++
			}
			return putArrivalCount(tx, count)
		})
		if err != nil || removed < 256 {
			return count, err
		}
	}
}

// A test-lane rewrite changes the raw/index generation, not local arrival
// history. Copy this small local tape in bounded pages before publishing it.
func copyArrivalTape(old, next *leaseHistory) error {
	if old == nil {
		return nil
	}
	var after []byte
	var sequence, count uint64
	for {
		type row struct{ key, value []byte }
		var rows []row
		err := old.db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(historyArrivalTape)
			if b == nil {
				return errors.New("lease history: previous arrival tape is missing")
			}
			var err error
			count, err = arrivalDiskCount(tx)
			if err != nil {
				return err
			}
			sequence = b.Sequence()
			c := b.Cursor()
			key, value := c.First()
			if len(after) != 0 {
				key, value = c.Seek(after)
				if bytes.Equal(key, after) {
					key, value = c.Next()
				}
			}
			for ; key != nil && len(rows) < 256; key, value = c.Next() {
				rows = append(rows, row{bytes.Clone(key), bytes.Clone(value)})
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		if err := next.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(historyArrivalTape)
			for _, r := range rows {
				if err := b.Put(r.key, r.value); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		after = rows[len(rows)-1].key
	}
	return next.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(historyArrivalTape).SetSequence(sequence); err != nil {
			return err
		}
		return putArrivalCount(tx, count)
	})
}
