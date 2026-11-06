package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

// The scope owns only open generations/streams, never a heap entry per record.
// Cancelling it releases generation pins independently of network writes. Each
// read transaction ends before its page can be sent over the network.
type historyScope struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	err    error
}

func newHistoryScope() *historyScope {
	ctx, cancel := context.WithCancel(context.Background())
	return &historyScope{ctx: ctx, cancel: cancel}
}

func (s *historyScope) report(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	} // bounded even if multiple retired generations fail
}

func (s *historyScope) result() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

type historyLife struct {
	mu       sync.Mutex
	refs     int
	pins     map[*historyPin]time.Time
	retiring bool
	closing  bool
	started  bool
	done     chan struct{}
	err      error
	scope    *historyScope
}

func (h *leaseHistory) bindScope(scope *historyScope) {
	h.life.mu.Lock()
	defer h.life.mu.Unlock()
	h.life.scope = scope
	scope.wg.Add(1)
}

func (h *leaseHistory) retire(retiring bool) {
	h.life.mu.Lock()
	defer h.life.mu.Unlock()
	h.life.retiring = retiring
}

func (h *leaseHistory) retired() bool {
	h.life.mu.Lock()
	defer h.life.mu.Unlock()
	return h.life.retiring
}

func (h *leaseHistory) minimumSelectionTime(current time.Time) time.Time {
	h.life.mu.Lock()
	defer h.life.mu.Unlock()
	for _, selectedAt := range h.life.pins {
		if selectedAt.Before(current) {
			current = selectedAt
		}
	}
	return current
}

// Requesting close never waits for a reader while Store.mu is held. An existing
// snapshot owns the old raw FD and bbolt mapping until it releases its pin.
func (h *leaseHistory) close() error {
	h.life.mu.Lock()
	h.life.closing = true
	start := h.life.refs == 0 && !h.life.started
	if start {
		h.life.started = true
	}
	err := h.life.err
	h.life.mu.Unlock()
	if start {
		return h.finishClose()
	}
	return err
}

func (h *leaseHistory) finishClose() error {
	err := errors.Join(h.db.Close(), h.raw.Close())
	h.life.mu.Lock()
	h.life.err = err
	scope := h.life.scope
	close(h.life.done)
	h.life.mu.Unlock()
	if scope != nil {
		scope.report(err)
		scope.wg.Done()
	}
	return err
}

func (h *leaseHistory) releasePin() {
	h.life.mu.Lock()
	h.life.refs--
	if h.life.refs < 0 {
		h.life.mu.Unlock()
		panic("lease history: negative generation pins")
	}
	start := h.life.refs == 0 && h.life.closing && !h.life.started
	if start {
		h.life.started = true
	}
	h.life.mu.Unlock()
	if start {
		_ = h.finishClose()
	}
}

type historyPin struct {
	mu        sync.Mutex
	h         *leaseHistory
	ctx       context.Context
	cancel    context.CancelFunc
	stopScope func() bool
	closed    bool
}

func (h *leaseHistory) pin(parent context.Context, selectedAt time.Time) (*historyPin, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	h.life.mu.Lock()
	if h.life.closing || h.life.retiring {
		h.life.mu.Unlock()
		return nil, errors.New("lease history: generation is retiring")
	}
	scope := h.life.scope
	if scope == nil || scope.ctx.Err() != nil {
		h.life.mu.Unlock()
		return nil, errors.New("lease history: generation is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	p := &historyPin{h: h, ctx: ctx, cancel: cancel}
	if h.life.pins == nil {
		h.life.pins = map[*historyPin]time.Time{}
	}
	h.life.pins[p] = selectedAt
	h.life.refs++
	h.life.mu.Unlock()
	p.stopScope = context.AfterFunc(scope.ctx, cancel)
	go func() { <-ctx.Done(); _ = p.Close() }()
	return p, nil
}

func (p *historyPin) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.cancel()
	p.stopScope()
	p.mu.Unlock()
	p.h.life.mu.Lock()
	delete(p.h.life.pins, p)
	p.h.life.mu.Unlock()
	p.h.releasePin()
	return nil
}

func (p *historyPin) read(origin string, seq uint64) (msg.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return msg.Record{}, errors.New("lease history: snapshot is closed")
	}
	if err := p.ctx.Err(); err != nil {
		return msg.Record{}, err
	}
	var ref historyRef
	err := p.h.db.View(func(tx *bolt.Tx) error {
		var err error
		ref, err = historyReference(tx, origin, seq)
		return err
	})
	if err != nil {
		return msg.Record{}, err
	}
	return p.h.readReference(origin, seq, ref, true)
}
