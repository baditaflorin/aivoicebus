package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

var (
	historyNoticeStates   = []byte("internal-notice-states")
	historyNoticeReserved = []byte("internal-notice-reserved")
	historyReaderTargets  = []byte("receipt-targets-by-reader")
	historyClaimDue       = []byte("local-future-claim-due")
)

type managedNoticeState struct {
	Probe int
	ID    string
}

func updateManagedNotice(tx *bolt.Tx, mid string) (string, error) {
	b := tx.Bucket(historyNoticeStates)
	var state managedNoticeState
	if v := b.Get([]byte(mid)); v != nil {
		if err := json.Unmarshal(v, &state); err != nil {
			return "", err
		}
	}
	for {
		nid := msg.NoticeID(mid, state.Probe)
		if err := tx.Bucket(historyNoticeReserved).Put([]byte(nid), []byte(mid)); err != nil {
			return "", err
		}
		if tx.Bucket(historyIDs).Get([]byte(nid)) == nil {
			state.ID = nid
			encoded, err := json.Marshal(state)
			if err != nil {
				return "", err
			}
			return nid, b.Put([]byte(mid), encoded)
		}
		state.Probe++
	}
}

func putManagedNoticeProjection(tx *bolt.Tx, r msg.Record) error {
	id := ""
	if r.Msg != nil {
		id = r.Msg.ID
	}
	if r.Ack != nil {
		if target, _, ok := msg.RetractedID(r.Ack.ID); ok {
			id = target
		}
	}
	if id == "" {
		return nil
	}
	var ref internalLeaseRef
	if v := tx.Bucket(historyLeases).Get([]byte(id)); v != nil {
		if err := json.Unmarshal(v, &ref); err != nil {
			return err
		}
	}
	if !ref.Withdrawn {
		return nil
	}
	_, err := updateManagedNotice(tx, id)
	return err
}

func (h *leaseHistory) managedNoticeID(mid string) (string, error) {
	var id string
	var change bool
	err := h.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(historyNoticeStates)
		if b == nil {
			return errors.New("lease history: notice index missing")
		}
		var state managedNoticeState
		if v := b.Get([]byte(mid)); v == nil || json.Unmarshal(v, &state) != nil || state.ID != msg.NoticeID(mid, state.Probe) {
			return errors.New("lease history: notice state inconsistent")
		}
		id = state.ID
		change = tx.Bucket(historyIDs).Get([]byte(id)) != nil
		return nil
	})
	if err == nil && change {
		err = h.db.Update(func(tx *bolt.Tx) error { var e error; id, e = updateManagedNotice(tx, mid); return e })
	}
	return id, err
}
func (h *leaseHistory) reservedNoticeID(id string) (bool, error) {
	reserved := false
	err := h.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(historyNoticeReserved)
		if b == nil {
			return errors.New("lease history: notice reservations missing")
		}
		reserved = b.Get([]byte(id)) != nil
		return nil
	})
	return reserved, err
}
func (s *Store) reservedNoticeLocked(id string) bool {
	if s.noticeIDs[id] {
		return true
	}
	if !s.leaseOnLocked() {
		return false
	}
	held, err := s.history.reservedNoticeID(id)
	if err != nil {
		s.historyErr = err
	}
	return held
}

func (h *leaseHistory) readerTargetPage(reader string, after []byte) ([]string, []byte, error) {
	var ids []string
	var next []byte
	prefix := []byte(reader + "\x00")
	err := h.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(historyReaderTargets)
		if b == nil {
			return errors.New("lease history: reader target index missing")
		}
		c := b.Cursor()
		key, _ := c.Seek(prefix)
		if after != nil {
			key, _ = c.Seek(after)
			if bytes.Equal(key, after) {
				key, _ = c.Next()
			}
		}
		for ; bytes.HasPrefix(key, prefix); key, _ = c.Next() {
			ids = append(ids, string(key[len(prefix):]))
			if len(ids) == 256 {
				next = bytes.Clone(key)
				break
			}
		}
		return nil
	})
	return ids, next, err
}

// A future-dated claim's local first arrival is derived local metadata. It is
// not a wire record and does not grow the bounded legacy arrival heap map.
func (s *Store) managedClaimDueLocked(key string, at time.Time, record bool) (time.Time, error) {
	due := at
	err := s.history.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(historyClaimDue)
		if b == nil {
			return errors.New("lease history: claim arrival index missing")
		}
		if v := b.Get([]byte(key)); v != nil {
			return json.Unmarshal(v, &due)
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	if !at.After(due) && at.After(s.now().UTC()) {
		due = s.claimDueLocked(key, at)
		if record {
			b, err := json.Marshal(due)
			if err != nil {
				return time.Time{}, err
			}
			err = s.history.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(historyClaimDue).Put([]byte(key), b) })
			if err != nil {
				return time.Time{}, err
			}
		}
	}
	return due, nil
}

func (s *Store) managedNoticeLocked(me, mid string, now time.Time) (msg.Message, bool) {
	m, held := s.byIDLocked(mid)
	if !held || m.From == me {
		return msg.Message{}, false
	}
	view := s.managedAcksLocked(mid)
	receipt, read := view.receipts[me]
	if !read {
		return msg.Message{}, false
	}
	r, withdrawn := s.retractionLocked(mid, m.From)
	if !withdrawn || (m.Priority() == msg.PrioFYI && (!r.leak || receipt.CountOnly)) {
		return msg.Message{}, false
	}
	var due time.Time
	own := false
	err := s.history.visitAcks(mid, s.logCounts, s.history.read, func(record msg.Record) error {
		a := *record.Ack
		_, _, ok := msg.RetractedID(a.ID)
		if !ok || !msg.MayRetract(a.By, m.From) {
			return nil
		}
		if a.By == me {
			own = true
		}
		d, err := s.managedClaimDueLocked(record.Origin+"/"+strconv.FormatUint(record.Seq, 10), a.At, false)
		if err != nil {
			return err
		}
		if due.IsZero() || d.Before(due) {
			due = d
		}
		return nil
	})
	if err != nil {
		s.historyErr = err
		return msg.Message{}, false
	}
	if own || due.IsZero() || now.Sub(due) >= msg.RetractNoticeWindow {
		return msg.Message{}, false
	}
	nid, err := s.history.managedNoticeID(mid)
	if err != nil {
		s.historyErr = err
		return msg.Message{}, false
	}
	if _, acked := s.managedAcksLocked(nid).receipts[me]; acked {
		return msg.Message{}, false
	}
	exp := due.Add(msg.RetractNoticeWindow)
	prio := msg.PrioAsk
	if r.leak {
		prio = msg.PrioUrgent
	}
	return msg.Message{ID: nid, From: msg.NoticeFrom, To: me, Thread: m.Thread, Re: m.ID, At: due, Exp: &exp, Prio: prio, Body: msg.NoticeBody(r.by, m.ID, m.At, r.leak)}, true
}

func (s *Store) managedNoticesLocked(me string, now time.Time, ordinary []msg.Message) []msg.Message {
	if !s.leaseOnLocked() {
		return ordinary
	}
	out := slices.Clone(ordinary)
	var after []byte
	less := func(a, b msg.Message) int {
		if (a.Priority() == msg.PrioUrgent) != (b.Priority() == msg.PrioUrgent) {
			if a.Priority() == msg.PrioUrgent {
				return -1
			}
			return 1
		}
		if cmp := a.At.Compare(b.At); cmp != 0 {
			return cmp
		}
		if a.Re < b.Re {
			return -1
		}
		if a.Re > b.Re {
			return 1
		}
		return 0
	}
	for {
		ids, next, err := s.history.readerTargetPage(me, after)
		if err != nil {
			s.historyErr = err
			return nil
		}
		for _, id := range ids {
			if !s.internalIDLocked(id) {
				continue
			}
			if n, ok := s.managedNoticeLocked(me, id, now); ok {
				out = append(out, n)
				slices.SortFunc(out, less)
				if len(out) > msg.MaxNotices {
					out = out[:msg.MaxNotices]
				}
			}
		}
		if next == nil {
			break
		}
		after = next
	}
	if s.historyErr != nil {
		return nil
	}
	return out
}

func copyLocalHistoryBuckets(old, next *leaseHistory) error {
	if old == nil {
		return nil
	}
	for _, name := range [][]byte{historyNoticeStates, historyNoticeReserved, historyClaimDue} {
		var after []byte
		for {
			type pair struct{ key, value []byte }
			var rows []pair
			err := old.db.View(func(tx *bolt.Tx) error {
				b := tx.Bucket(name)
				if b == nil {
					return errors.New("lease history: local index missing")
				}
				c := b.Cursor()
				k, v := c.First()
				if after != nil {
					k, v = c.Seek(after)
					if bytes.Equal(k, after) {
						k, v = c.Next()
					}
				}
				for ; k != nil && len(rows) < 256; k, v = c.Next() {
					rows = append(rows, pair{bytes.Clone(k), bytes.Clone(v)})
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
				for _, row := range rows {
					if err := tx.Bucket(name).Put(row.key, row.value); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
			after = rows[len(rows)-1].key
		}
	}
	return nil
}
