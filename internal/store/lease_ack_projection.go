package store

import (
	"bytes"
	"errors"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

type historyPosition struct {
	origin string
	seq    uint64
}

func (h *leaseHistory) ackPage(id string, after []byte, limit int) ([]historyPosition, []byte, error) {
	if limit < 1 || limit > 256 {
		return nil, nil, errors.New("lease history: invalid acknowledgement page size")
	}
	prefix := []byte(id + "\x00")
	var out []historyPosition
	var next []byte
	err := h.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(historyAckTargets)
		if b == nil {
			return errors.New("lease history: acknowledgement index is missing")
		}
		c := b.Cursor()
		key, value := c.Seek(prefix)
		if len(after) != 0 {
			if !bytes.HasPrefix(after, prefix) {
				return errors.New("lease history: acknowledgement cursor differs")
			}
			key, value = c.Seek(after)
			if bytes.Equal(key, after) {
				key, value = c.Next()
			}
		}
		for ; bytes.HasPrefix(key, prefix); key, value = c.Next() {
			if !bytes.Equal(key[len(prefix):], value) {
				return errors.New("lease history: acknowledgement reference differs")
			}
			origin, seq, err := decodeHistoryKey(value)
			if err != nil {
				return err
			}
			out = append(out, historyPosition{origin, seq})
			if len(out) == limit {
				next = bytes.Clone(key)
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, next, nil
}

// A snapshot folds only records at or below its captured version vector.
// No reader/retractor identity is retained in a Store heap map. The effective
// retraction is constant-sized and uses the existing protocol's authority/ties.
func (h *leaseHistory) foldedRetraction(id, from string, vv map[string]uint64, read func(string, uint64) (msg.Record, error)) (retraction, bool, error) {
	var effective retraction
	found := false
	var cursor []byte
	for {
		page, next, err := h.ackPage(id, cursor, 256)
		if err != nil {
			return retraction{}, false, err
		}
		for _, position := range page {
			if vv != nil && position.seq > vv[position.origin] {
				continue
			}
			r, err := read(position.origin, position.seq)
			if err != nil {
				return retraction{}, false, err
			}
			if r.Ack == nil || ackTarget(*r.Ack) != id {
				return retraction{}, false, errors.New("lease history: acknowledgement target differs from raw authority")
			}
			mid, leak, ok := msg.RetractedID(r.Ack.ID)
			if ok && mid == id && msg.MayRetract(r.Ack.By, from) {
				effective.fold(!found, r.Ack.By, r.Ack.At, leak)
				found = true
			}
		}
		if next == nil {
			break
		}
		cursor = next
	}
	return effective, found, nil
}
