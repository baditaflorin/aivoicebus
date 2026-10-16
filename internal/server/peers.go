package server

import (
	"sort"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

type peerHealth = msg.PeerHealth

// PeerBoard records how replication from each peer is going, for /v1/health and
// /metrics. It is written by the replication loops and read by the handlers.
type PeerBoard struct {
	mu    sync.Mutex
	peers map[string]*peerHealth
}

// NewPeerBoard makes a board that already knows its peers, all not yet reached.
func NewPeerBoard(peers []string) *PeerBoard {
	b := &PeerBoard{peers: map[string]*peerHealth{}}
	for _, p := range peers {
		b.peers[p] = &peerHealth{URL: p}
	}
	return b
}

// Update changes the entry for peer under the board's lock.
func (b *PeerBoard) Update(peer string, fn func(*peerHealth)) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.peers[peer]
	if p == nil {
		p = &peerHealth{URL: peer}
		b.peers[peer] = p
	}
	fn(p)
}

// Snapshot copies the board, sorted by URL.
func (b *PeerBoard) Snapshot() []peerHealth {
	if b == nil {
		return []peerHealth{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]peerHealth, 0, len(b.peers))
	for _, p := range b.peers {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

// ok marks a successful look at the peer.
func okPeer(p *peerHealth, now time.Time, pulled int, lag int64, skewMs int64, version string) {
	p.OK, p.Error, p.LastOK = true, "", now
	p.Pulled += int64(pulled)
	p.Lag, p.SkewMs = lag, skewMs
	if version != "" {
		p.Version = version
	}
}

func failPeer(p *peerHealth, err error) {
	p.OK, p.Error = false, err.Error()
	p.Failures++
}

// AnyOK reports whether at least one peer has answered a pull.
func (b *PeerBoard) AnyOK() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.peers {
		if p.OK {
			return true
		}
	}
	return false
}

// Ready says whether a node may take writes: it has records of its own (an
// ordinary restart), or it has no peers (a lone node), or a peer has answered
// (so anything this node wrote before losing its disk has been taken back).
// Reads are always served.
func Ready(st *store.Store, board *PeerBoard, peers int) func() bool {
	return func() bool { return peers == 0 || !st.Empty() || board.AnyOK() }
}
