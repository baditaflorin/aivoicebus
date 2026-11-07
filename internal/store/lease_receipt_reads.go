package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

// Summaries make count/exact-reader queries bounded even when a single target
// has years of receipts. Both the selected receipt and the full-read override
// carry independently verified raw references, never an unverified empty view.
func (h *leaseHistory) verifyReceipt(id, by string, state receiptSummary) error {
	check := func(proof []byte) (msg.Ack, error) {
		origin, seq, err := decodeHistoryKey(proof)
		if err != nil {
			return msg.Ack{}, err
		}
		r, err := h.read(origin, seq)
		if err != nil {
			return msg.Ack{}, err
		}
		if r.Ack == nil || r.Ack.ID != id || r.Ack.By != by {
			return msg.Ack{}, errors.New("lease history: receipt proof differs")
		}
		return *r.Ack, nil
	}
	a, err := check(state.Proof)
	if err != nil {
		return err
	}
	if state.Receipt.By != by || !state.Receipt.At.Equal(a.At) || state.Receipt.Via != a.Via {
		return errors.New("lease history: selected receipt differs from raw proof")
	}
	if state.CountOnly {
		if a.Via != msg.ViaHookCount || len(state.FullProof) != 0 {
			return errors.New("lease history: count-only receipt proof differs")
		}
	} else {
		full, err := check(state.FullProof)
		if err != nil {
			return err
		}
		if full.Via == msg.ViaHookCount {
			return errors.New("lease history: full receipt proof differs")
		}
	}
	return nil
}
func (h *leaseHistory) exactReceipt(id, by string) (receiptSummary, bool, error) {
	var state receiptSummary
	found := false
	err := h.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(historyReceipts)
		if b == nil {
			return errors.New("lease history: receipt summary index missing")
		}
		v := b.Get([]byte(id + "\x00" + by))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &state)
	})
	if err == nil && found {
		err = h.verifyReceipt(id, by, state)
	}
	return state, found, err
}
func (h *leaseHistory) visitReceipts(id string, visit func(string, receiptSummary)) error {
	prefix := []byte(id + "\x00")
	var after []byte
	for {
		type row struct {
			by    string
			state receiptSummary
		}
		var rows []row
		var next []byte
		err := h.db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(historyReceipts)
			if b == nil {
				return errors.New("lease history: receipt summary index missing")
			}
			c := b.Cursor()
			k, v := c.Seek(prefix)
			if after != nil {
				k, v = c.Seek(after)
				if bytes.Equal(k, after) {
					k, v = c.Next()
				}
			}
			for ; bytes.HasPrefix(k, prefix); k, v = c.Next() {
				var state receiptSummary
				if err := json.Unmarshal(v, &state); err != nil {
					return err
				}
				rows = append(rows, row{string(k[len(prefix):]), state})
				if len(rows) == 256 {
					next = bytes.Clone(k)
					break
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := h.verifyReceipt(id, row.by, row.state); err != nil {
				return err
			}
			visit(row.by, row.state)
		}
		if next == nil {
			return nil
		}
		after = next
	}
}
func (s *Store) receiptCountLocked(id string, exclude ...string) int {
	count := 0
	err := s.history.visitReceipts(id, func(by string, _ receiptSummary) {
		for _, identity := range exclude {
			if by == identity {
				return
			}
		}
		count++
	})
	if err != nil {
		s.historyErr = err
		return 0
	}
	return count
}
