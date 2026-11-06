package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

var (
	historyReceipts         = []byte("receipt-summaries")
	historyTestReceipts     = []byte("test-receipt-count")
	historyRetractionBy     = []byte("retraction-identities")
	historyManagedRetracted = []byte("internal-retracted-ids")
	historyManagedInert     = []byte("internal-inert-count")
	historyDeferredTargets  = []byte("deferred-ack-targets")
)

type receiptSummary struct {
	Receipt   msg.Receipt
	CountOnly bool
}

type retractionSummary struct {
	At   time.Time
	Leak bool
}

func putAckMetadata(tx *bolt.Tx, a msg.Ack) error {
	target := ackTarget(a)
	if target != "" && tx.Bucket(historyIDs).Get([]byte(target)) == nil {
		if err := tx.Bucket(historyDeferredTargets).Put([]byte(target), []byte{1}); err != nil {
			return err
		}
	}
	if id, leak, ok := msg.RetractedID(a.ID); ok {
		b := tx.Bucket(historyRetractionBy)
		key := []byte(id + "\x00" + a.By)
		value := b.Get(key)
		first := value == nil
		var old retractionSummary
		if !first {
			if err := json.Unmarshal(value, &old); err != nil {
				return err
			}
		}
		if first || a.At.Before(old.At) {
			old.At = a.At
		}
		old.Leak = old.Leak || leak
		encoded, err := json.Marshal(old)
		if err != nil {
			return err
		}
		if err := b.Put(key, encoded); err != nil {
			return err
		}
		if first {
			var ref internalLeaseRef
			if value := tx.Bucket(historyLeases).Get([]byte(id)); value != nil {
				if err := json.Unmarshal(value, &ref); err != nil {
					return err
				}
				return countManagedRetraction(tx, id, ref.From, a.By)
			}
		}
		return nil
	}
	if _, ok := msg.AmendedID(a.ID); ok {
		return nil
	}
	if _, ok := msg.SettledID(a.ID); ok {
		return nil
	}
	// This includes unknown and quarantine receipts, exactly as addAck does.
	// The table is derived disk state; no target/reader pair needs a heap map.
	b := tx.Bucket(historyReceipts)
	key := []byte(a.ID + "\x00" + a.By)
	var state receiptSummary
	value := b.Get(key)
	first := value == nil
	if !first {
		if err := json.Unmarshal(value, &state); err != nil {
			return err
		}
	}
	cur := state.Receipt
	if first || a.At.Before(cur.At) || (cur.Via == msg.ViaHookCount && a.Via != msg.ViaHookCount) {
		at := a.At
		if !first && cur.Via != msg.ViaHookCount && cur.At.Before(at) {
			at = cur.At
		}
		state.Receipt = msg.Receipt{By: a.By, At: at, Via: a.Via}
	}
	if first && a.Via == msg.ViaHookCount {
		state.CountOnly = true
	}
	if a.Via != msg.ViaHookCount {
		state.CountOnly = false
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := b.Put(key, encoded); err != nil {
		return err
	}
	if first {
		if err := b.SetSequence(b.Sequence() + 1); err != nil {
			return err
		}
		if msg.IsTestID(a.ID) {
			b = tx.Bucket(historyTestReceipts)
			return b.SetSequence(b.Sequence() + 1)
		}
	}
	return nil
}

func countManagedRetraction(tx *bolt.Tx, id, from, by string) error {
	if msg.MayRetract(by, from) {
		b := tx.Bucket(historyManagedRetracted)
		if b.Get([]byte(id)) != nil {
			return nil
		}
		if err := b.Put([]byte(id), []byte{1}); err != nil {
			return err
		}
		return b.SetSequence(b.Sequence() + 1)
	}
	b := tx.Bucket(historyManagedInert)
	return b.SetSequence(b.Sequence() + 1)
}

func seedManagedRetractionCounts(tx *bolt.Tx, ref internalLeaseRef) error {
	prefix := []byte(ref.ID + "\x00")
	c := tx.Bucket(historyRetractionBy).Cursor()
	for key, _ := c.Seek(prefix); bytes.HasPrefix(key, prefix); key, _ = c.Next() {
		if err := countManagedRetraction(tx, ref.ID, ref.From, string(key[len(prefix):])); err != nil {
			return err
		}
	}
	return nil
}

type internalCounters struct {
	messages, receipts, testReceipts, retracted, inert uint64
}

func (h *leaseHistory) counters() (internalCounters, error) {
	var out internalCounters
	err := h.db.View(func(tx *bolt.Tx) error {
		for name, dest := range map[string]*uint64{
			string(historyLeases): &out.messages, string(historyReceipts): &out.receipts,
			string(historyTestReceipts): &out.testReceipts, string(historyManagedRetracted): &out.retracted, string(historyManagedInert): &out.inert,
		} {
			b := tx.Bucket([]byte(name))
			if b == nil {
				return errors.New("lease history: counter index is missing")
			}
			*dest = b.Sequence()
		}
		return nil
	})
	return out, err
}

func (h *leaseHistory) deferredTarget(id string) (bool, error) {
	found := false
	err := h.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(historyDeferredTargets)
		if b == nil {
			return errors.New("lease history: pending target index is missing")
		}
		found = b.Get([]byte(id)) != nil
		return nil
	})
	return found, err
}

func (h *leaseHistory) clearDeferredTarget(id string) error {
	return h.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(historyDeferredTargets)
		if b == nil {
			return errors.New("lease history: pending target index is missing")
		}
		return b.Delete([]byte(id))
	})
}
