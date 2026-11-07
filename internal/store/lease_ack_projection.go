package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

type historyPosition struct {
	origin string
	seq    uint64
}

func (h *leaseHistory) visitAcks(id string, vv map[string]uint64, read func(string, uint64) (msg.Record, error), visit func(msg.Record) error) error {
	var cursor []byte
	for {
		page, next, err := h.ackPage(id, cursor, 256)
		if err != nil {
			return err
		}
		for _, position := range page {
			if vv != nil && position.seq > vv[position.origin] {
				continue
			}
			r, err := read(position.origin, position.seq)
			if err != nil {
				return err
			}
			if r.Ack == nil || ackTarget(*r.Ack) != id {
				return errors.New("lease history: acknowledgement target differs from raw authority")
			}
			if err := visit(r); err != nil {
				return err
			}
		}
		if next == nil {
			return nil
		}
		cursor = next
	}
}

// A current lease GET needs one authenticated withdrawal proof, not the full
// history of every receipt on the lease. Older snapshots fall back to their
// captured VV only when the first withdrawal arrived after they began.
func (h *leaseHistory) withdrawnAt(id, from string, vv map[string]uint64, read func(string, uint64) (msg.Record, error)) (bool, error) {
	proof, err := h.withdrawalProof(id, from)
	if err != nil || proof == nil {
		return false, err
	}
	origin, seq, err := decodeHistoryKey(proof)
	if err != nil {
		return false, err
	}
	if vv != nil && seq > vv[origin] {
		_, found, err := h.foldedRetraction(id, from, vv, read)
		return found, err
	}
	r, err := read(origin, seq)
	if err != nil {
		return false, err
	}
	if r.Ack == nil {
		return false, errors.New("lease history: withdrawal proof is not an acknowledgement")
	}
	target, _, ok := msg.RetractedID(r.Ack.ID)
	if !ok || target != id || !msg.MayRetract(r.Ack.By, from) {
		return false, errors.New("lease history: withdrawal proof differs from raw authority")
	}
	return true, nil
}

func (h *leaseHistory) withdrawalProof(id, from string) ([]byte, error) {
	var proof []byte
	err := h.db.View(func(tx *bolt.Tx) error {
		leases, proofs := tx.Bucket(historyLeases), tx.Bucket(historyManagedRetracted)
		if leases == nil || proofs == nil {
			return errors.New("lease history: withdrawal state indexes are missing")
		}
		var ref internalLeaseRef
		if value := leases.Get([]byte(id)); value == nil || json.Unmarshal(value, &ref) != nil || ref.ID != id || ref.From != from {
			return errors.New("lease history: withdrawal state owner differs")
		}
		proof = bytes.Clone(proofs.Get([]byte(id)))
		if ref.Withdrawn != (proof != nil) {
			return errors.New("lease history: expected withdrawal proof is missing or inconsistent")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return proof, nil
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
			if len(key) != len(prefix)+8 {
				return errors.New("lease history: acknowledgement reference differs")
			}
			origin, seq, err := decodeHistoryKey(value)
			if err != nil {
				return err
			}
			ref, err := historyReference(tx, origin, seq)
			if err != nil || ref.Offset < 0 || uint64(ref.Offset) != binary.BigEndian.Uint64(key[len(prefix):]) {
				return errors.New("lease history: acknowledgement arrival order differs")
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
	if _, managed, err := h.internalLease(id); err != nil {
		return retraction{}, false, err
	} else if managed {
		if _, err := h.withdrawalProof(id, from); err != nil {
			return retraction{}, false, err
		}
	}
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
