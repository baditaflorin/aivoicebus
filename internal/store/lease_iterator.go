package store

import (
	"context"
	"errors"
	"io"
	"maps"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

type leaseArrival struct {
	lease  msg.Lease
	offset int64
}

// LeaseIterator owns only a bounded page of scoped references/payloads, plus
// caller-owned ordinary lease output. It pins file/index generations, never a
// bbolt transaction while its caller writes to a network connection.
type LeaseIterator struct {
	mu          sync.Mutex
	store       *Store
	pin         *historyPin
	ctx         context.Context
	at          time.Time
	vv          map[string]uint64
	rawEnd      int64
	ordinary    []leaseArrival
	ordinaryAt  int
	refs        []internalLeaseRef
	refsAt      int
	after       []byte
	lastPage    bool
	managedDone bool
	next        *leaseArrival
	expected    uint64
	seen        uint64
	closed      bool
}

// Preparation serializes time selection, pin registration and window refresh.
// No Store lock is retained after this method returns.
func (s *Store) NewLeaseIterator(ctx context.Context) (*LeaseIterator, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthLocked(); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	it := &LeaseIterator{store: s, ctx: ctx, at: now, vv: maps.Clone(s.logCounts)}
	if s.history != nil {
		pin, err := s.history.pin(ctx, now)
		if err != nil {
			return nil, err
		}
		it.pin, it.ctx, it.rawEnd = pin, pin.ctx, s.history.rawEnd
		if err := s.history.refreshLiveWindow(now); err != nil {
			_ = pin.Close()
			s.historyErr = err
			return nil, err
		}
		if err := it.prepareCountLocked(); err != nil {
			cancelled := pin.ctx.Err() != nil
			_ = pin.Close()
			if !cancelled {
				s.historyErr = err
			}
			return nil, err
		}
	}
	for _, index := range s.leases {
		if err := it.ctx.Err(); err != nil {
			if it.pin != nil {
				_ = it.pin.Close()
			}
			return nil, err
		}
		m := s.msgs[index]
		if s.forged[m.ID] || m.Expired(now) || s.retractedLocked(m.ID, m.From) {
			continue
		}
		if it.pin != nil {
			_, managed, err := it.pin.h.internalLease(m.ID)
			if err != nil {
				_ = it.pin.Close()
				s.historyErr = err
				return nil, err
			}
			if managed {
				continue
			}
		}
		lease, ok := msg.LeaseOf(m)
		if !ok {
			continue
		}
		offset := int64(index)
		if it.pin != nil {
			_, value, err := it.pin.h.firstPosition(m.ID)
			if err != nil {
				_ = it.pin.Close()
				s.historyErr = err
				return nil, err
			}
			offset = value
		}
		it.ordinary = append(it.ordinary, leaseArrival{lease, offset})
	}
	if it.pin == nil {
		it.managedDone = true
	}
	return it, nil
}

func (it *LeaseIterator) prepareCountLocked() error {
	p := it.pin
	p.mu.Lock()
	defer p.mu.Unlock()
	var cursor []byte
	var visited uint64
	var total uint64
	first := true
	for {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		page, next, count, err := p.h.liveLeasePage(cursor, it.rawEnd, 256)
		if err != nil {
			return err
		}
		if !first && total != count {
			return errors.New("lease history: live count changed during serialized preparation")
		}
		total = count
		first = false
		for _, ref := range page {
			m, err := it.readManagedLocked(ref)
			if err != nil {
				return err
			}
			visited++
			if !m.Expired(it.at) {
				it.expected++
			}
		}
		if next == nil {
			break
		}
		cursor = next
	}
	if visited != total {
		return errors.New("lease history: live candidate count differs from index")
	}
	return nil
}

func (it *LeaseIterator) readManagedLocked(ref internalLeaseRef) (msg.Message, error) {
	if err := it.pin.h.checkInternalReference(ref); err != nil {
		return msg.Message{}, err
	}
	r, err := it.pin.readLocked(ref.Origin, ref.Seq)
	if err != nil {
		return msg.Message{}, err
	}
	return it.pin.h.internalPayload(ref, r)
}

func (it *LeaseIterator) nextManagedLocked() (*leaseArrival, error) {
	if it.next != nil || it.managedDone {
		return it.next, nil
	}
	for {
		if it.refsAt == len(it.refs) {
			if it.lastPage {
				it.managedDone = true
				if it.seen != it.expected {
					return nil, errors.New("lease history: snapshot lost a selected candidate")
				}
				return nil, nil
			}
			page, next, _, err := it.pin.h.liveLeasePage(it.after, it.rawEnd, 256)
			if err != nil {
				return nil, err
			}
			it.refs, it.refsAt, it.after, it.lastPage = page, 0, next, next == nil
			if len(page) == 0 {
				continue
			}
		}
		ref := it.refs[it.refsAt]
		it.refsAt++
		m, err := it.readManagedLocked(ref)
		if err != nil {
			return nil, err
		}
		if m.Expired(it.at) {
			continue
		}
		it.seen++
		withdrawn, err := it.pin.h.withdrawnAt(m.ID, m.From, it.vv, it.pin.readLocked)
		if err != nil {
			return nil, err
		}
		if withdrawn {
			continue
		}
		lease, ok := msg.LeaseOf(m)
		if !ok {
			return nil, errors.New("lease history: selected payload is not a lease")
		}
		it.next = &leaseArrival{lease, ref.Offset}
		return it.next, nil
	}
}

// Next returns at most 256 leases. A partial page is discarded on any error;
// the caller must not close a JSON array after an error other than io.EOF.
func (it *LeaseIterator) Next() ([]msg.Lease, error) {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.closed {
		return nil, context.Canceled
	}
	if err := it.ctx.Err(); err != nil {
		return nil, err
	}
	if it.pin != nil {
		it.pin.mu.Lock()
		defer it.pin.mu.Unlock()
	}
	var out []msg.Lease
	bytes := 0
	for len(out) < 256 {
		if err := it.ctx.Err(); err != nil {
			return nil, err
		}
		managed, err := it.nextManagedLocked()
		if err != nil {
			it.markError(err)
			return nil, err
		}
		var value *leaseArrival
		useOrdinary := it.ordinaryAt < len(it.ordinary) && (managed == nil || it.ordinary[it.ordinaryAt].offset < managed.offset)
		if useOrdinary {
			value = &it.ordinary[it.ordinaryAt]
		} else {
			value = managed
		}
		if value == nil {
			if len(out) == 0 {
				return nil, io.EOF
			}
			break
		}
		size := len(value.lease.Key) + len(value.lease.By) + len(value.lease.ID) + len(value.lease.Note) + 256
		if bytes+size > 8<<20 && len(out) > 0 {
			break
		}
		bytes += size
		out = append(out, value.lease)
		if useOrdinary {
			it.ordinaryAt++
		} else {
			it.next = nil
		}
	}
	return out, nil
}

func (it *LeaseIterator) markError(err error) {
	if it.ctx.Err() != nil {
		return
	}
	it.store.mu.Lock()
	defer it.store.mu.Unlock()
	if it.pin != nil && it.store.history == it.pin.h && it.store.historyErr == nil {
		it.store.historyErr = err
	}
}

func (it *LeaseIterator) Context() context.Context { return it.ctx }

func (it *LeaseIterator) Close() error {
	it.mu.Lock()
	if it.closed {
		it.mu.Unlock()
		return nil
	}
	it.closed = true
	it.refs = nil
	it.ordinary = nil
	it.next = nil
	p := it.pin
	it.mu.Unlock()
	if p != nil {
		return p.Close()
	}
	return nil
}

func (s *Store) LeasesChecked() ([]msg.Lease, error) {
	it, err := s.NewLeaseIterator(context.Background())
	if err != nil {
		return nil, err
	}
	defer it.Close()
	out := []msg.Lease{}
	for {
		page, err := it.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
}
