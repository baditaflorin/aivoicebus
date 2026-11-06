package store

// Derived lease relations contain only references and small metadata. Payloads,
// signatures, ordinary messages and every wire record stay in the raw log.
// This increment builds/validates the projection; it does not enable On mode.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

var (
	historyLeases      = []byte("internal-leases")
	historyLeaseExpiry = []byte("internal-leases-by-expiry")
	historyAckTargets  = []byte("ack-target-records")
)

type internalLeaseRef struct {
	ID     string
	Origin string
	Seq    uint64
	Offset int64
	From   string
	At     time.Time
	Expiry time.Time
}

func internalLeaseEligible(m msg.Message, o LeaseRetentionOptions) bool {
	if m.Kind != msg.KindLease || m.To != m.From || m.Re != "" || m.Thread != m.ID ||
		m.IsTest() || m.Exp == nil || !m.Exp.After(m.At) || m.TTL != 0 ||
		m.MaxTurns != 0 || m.Deadline != 0 || m.BudgetChars != 0 || m.Depth != 0 {
		return false
	}
	host, agent, ok := strings.Cut(m.From, "/")
	if !ok || !msg.ValidIdentity(m.From) {
		return false
	}
	allowed := false
	for _, configured := range o.Hosts {
		if strings.EqualFold(host, configured) {
			allowed = true
			break
		}
	}
	if !allowed || !internalLeaseAgent(agent) {
		return false
	}
	l, ok := msg.LeaseOf(m)
	if !ok || !strings.HasPrefix(l.Key, "deploy:prod:") || len(l.Key) == len("deploy:prod:") {
		return false
	}
	return m.At.Year() >= 0 && m.At.Year() <= 9999 && m.Exp.Year() >= 0 && m.Exp.Year() <= 9999
}

func internalLeaseAgent(agent string) bool {
	if agent == "fleet-runner" { // the previous constant owner, kept for old history
		return true
	}
	value, ok := strings.CutPrefix(agent, "fleet-runner.")
	if !ok || len(value) != 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Fixed-width UTC dates sort correctly without UnixNano's year-2262 overflow.
func leaseExpiryKey(t time.Time, id string) ([]byte, error) {
	t = t.UTC()
	if t.Year() < 0 || t.Year() > 9999 {
		return nil, errors.New("lease history: expiry is outside the wire time range")
	}
	return []byte(t.Format("20060102150405.000000000") + "\x00" + id), nil
}

func ackTarget(a msg.Ack) string {
	if id, _, ok := msg.RetractedID(a.ID); ok {
		return id
	}
	if id, ok := msg.AmendedID(a.ID); ok {
		return id
	}
	if id, ok := msg.SettledID(a.ID); ok {
		return id
	}
	if msg.ValidID(a.ID) {
		return a.ID
	}
	return ""
}

func putLeaseProjection(tx *bolt.Tx, r msg.Record, offset int64, o LeaseRetentionOptions) error {
	key := historyKey(r.Origin, r.Seq)
	if m := r.Msg; m != nil && internalLeaseEligible(*m, o) && bytes.Equal(tx.Bucket(historyIDs).Get([]byte(m.ID)), key) {
		leases := tx.Bucket(historyLeases)
		if leases.Get([]byte(m.ID)) == nil {
			ref := internalLeaseRef{ID: m.ID, Origin: r.Origin, Seq: r.Seq, Offset: offset, From: m.From, At: m.At, Expiry: *m.Exp}
			value, err := json.Marshal(ref)
			if err != nil {
				return err
			}
			expiryKey, err := leaseExpiryKey(*m.Exp, m.ID)
			if err != nil {
				return err
			}
			if err := leases.Put([]byte(m.ID), value); err != nil {
				return err
			}
			if err := leases.SetSequence(leases.Sequence() + 1); err != nil {
				return err
			}
			if err := tx.Bucket(historyLeaseExpiry).Put(expiryKey, []byte(m.ID)); err != nil {
				return err
			}
			floor, err := leaseWindowFloor(tx)
			if err != nil {
				return err
			}
			if ref.Expiry.After(floor) {
				if err := putLiveLease(tx, ref, true); err != nil {
					return err
				}
			}
			if err := seedManagedRetractionCounts(tx, ref); err != nil {
				return err
			}
		}
	}
	if a := r.Ack; a != nil {
		if id := ackTarget(*a); id != "" {
			// Keep all target relations on disk, including a claim that arrived
			// before its message. No unknown target needs a permanent heap slot.
			if err := tx.Bucket(historyAckTargets).Put(append([]byte(id+"\x00"), key...), key); err != nil {
				return err
			}
		}
		return putAckMetadata(tx, *a)
	}
	return nil
}

func decodeHistoryKey(key []byte) (string, uint64, error) {
	if len(key) < 10 || key[len(key)-9] != 0 {
		return "", 0, errors.New("lease history: invalid record reference")
	}
	return string(key[:len(key)-9]), binary.BigEndian.Uint64(key[len(key)-8:]), nil
}

func (h *leaseHistory) messageByID(id string) (msg.Message, bool, error) {
	var key []byte
	err := h.db.View(func(tx *bolt.Tx) error {
		key = bytes.Clone(tx.Bucket(historyIDs).Get([]byte(id)))
		return nil
	})
	if err != nil || len(key) == 0 {
		return msg.Message{}, false, err
	}
	origin, seq, err := decodeHistoryKey(key)
	if err != nil {
		return msg.Message{}, false, err
	}
	r, err := h.read(origin, seq)
	if err != nil {
		return msg.Message{}, false, err
	}
	if r.Msg == nil || r.Msg.ID != id {
		return msg.Message{}, false, errors.New("lease history: ID reference differs from raw authority")
	}
	return *r.Msg, true, nil
}

func (h *leaseHistory) internalLease(id string) (internalLeaseRef, bool, error) {
	var ref internalLeaseRef
	var found bool
	err := h.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket(historyLeases).Get([]byte(id))
		if value == nil {
			return nil
		}
		found = true
		return json.Unmarshal(value, &ref)
	})
	if err == nil && found && ref.ID != id {
		err = errors.New("lease history: internal ID index differs")
	}
	return ref, found, err
}

// Pages cap resident metadata, not the logical population. The caller continues
// until next is nil; 5000 live claims are not truncated to the cache budget.
func (h *leaseHistory) internalLeasePage(minExpiry time.Time, after []byte, limit int) ([]internalLeaseRef, []byte, error) {
	if limit < 1 || limit > 256 {
		return nil, nil, errors.New("lease history: page must contain 1 to 256 references")
	}
	start, err := leaseExpiryKey(minExpiry, "")
	if err != nil {
		return nil, nil, err
	}
	var out []internalLeaseRef
	var next []byte
	err = h.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(historyLeaseExpiry).Cursor()
		key, id := cursor.Seek(start)
		if len(after) != 0 {
			if bytes.Compare(after, start) < 0 {
				return errors.New("lease history: page cursor precedes selected range")
			}
			key, id = cursor.Seek(after)
			if bytes.Equal(key, after) {
				key, id = cursor.Next()
			}
		}
		for ; key != nil; key, id = cursor.Next() {
			var ref internalLeaseRef
			value := tx.Bucket(historyLeases).Get(id)
			if value == nil || json.Unmarshal(value, &ref) != nil || ref.ID != string(id) {
				return errors.New("lease history: expiry reference is missing or malformed")
			}
			want, err := leaseExpiryKey(ref.Expiry, ref.ID)
			if err != nil || !bytes.Equal(want, key) {
				return errors.New("lease history: expiry reference differs")
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
		return nil, nil, err // never expose a partial page on an index error
	}
	return out, next, nil
}

func (h *leaseHistory) readInternalLease(ref internalLeaseRef) (msg.Message, error) {
	err := h.db.View(func(tx *bolt.Tx) error {
		key := historyKey(ref.Origin, ref.Seq)
		if !bytes.Equal(tx.Bucket(historyIDs).Get([]byte(ref.ID)), key) {
			return errors.New("lease history: internal metadata does not name the first ID")
		}
		var rawRef historyRef
		if json.Unmarshal(tx.Bucket(historyRecords).Get(key), &rawRef) != nil || rawRef.Offset != ref.Offset {
			return errors.New("lease history: internal ordering differs from raw reference")
		}
		return nil
	})
	if err != nil {
		return msg.Message{}, err
	}
	r, err := h.read(ref.Origin, ref.Seq)
	if err != nil {
		return msg.Message{}, err
	}
	if r.Msg == nil || r.Msg.ID != ref.ID || r.Msg.From != ref.From || !r.Msg.At.Equal(ref.At) ||
		r.Msg.Exp == nil || !r.Msg.Exp.Equal(ref.Expiry) || !internalLeaseEligible(*r.Msg, h.retention) {
		return msg.Message{}, fmt.Errorf("lease history: internal metadata differs from raw authority")
	}
	return *r.Msg, nil
}
