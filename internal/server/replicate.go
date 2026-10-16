package server

import (
	"context"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// Replicate keeps st in step with every peer until ctx ends. Each peer has its
// own loop, so a slow or dead one never delays the others. A down peer is retried
// on the next tick; nothing is lost because the peer's log is the source for its
// own origin and every node serves what it holds. board, which may be nil,
// records how each peer is doing.
func Replicate(ctx context.Context, st *store.Store, peers []string, token string, every time.Duration, board *PeerBoard) {
	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			t := time.NewTicker(every)
			defer t.Stop()
			var presenceAt time.Time // the newest sighting taken from this peer
			lastGossip := time.Time{}
			for {
				pull(st, peer, token, board)
				if time.Since(lastGossip) >= 5*every { // sessions change slowly: look at them every few pulls
					lastGossip = time.Now()
					presenceAt = gossip(st, peer, token, presenceAt)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}(peer)
	}
	wg.Wait()
}

// SyncLoop pulls from one peer, forever.
func SyncLoop(st *store.Store, peer, token string, every time.Duration) {
	Replicate(context.Background(), st, []string{peer}, token, every, nil)
}

// PullOnce fetches from peer until this store has everything the peer has,
// and returns how many records were new.
func PullOnce(st *store.Store, peer, token string) (int, error) {
	return pull(st, peer, token, nil)
}

// pull is PullOnce that also tells board what it saw: whether the peer answered,
// how far behind this node still was, the peer's clock against ours, and its release.
func pull(st *store.Store, peer, token string, board *PeerBoard) (int, error) {
	c := &client.Client{URLs: []string{peer}, Token: token}
	total := 0
	fail := func(err error) (int, error) {
		board.Update(peer, func(p *peerHealth) { failPeer(p, err) })
		return total, err
	}
	for {
		recs, err := c.Sync(st.VV())
		if err != nil {
			return fail(err)
		}
		n, err := st.Merge(recs)
		total += n
		if err != nil {
			return fail(err)
		}
		if n == 0 {
			break
		}
	}
	var lag, skewMs int64
	ver := c.Server.Version
	t0 := time.Now()
	if h, err := c.Health(); err == nil { // a node without /v1/health leaves lag and skew unknown (0)
		t1 := time.Now()
		lag = lagOf(h.Log, st.VV())
		skewMs = h.Now.Sub(t0.Add(t1.Sub(t0) / 2)).Milliseconds()
		if h.Version != "" {
			ver = h.Version
		}
	}
	board.Update(peer, func(p *peerHealth) { okPeer(p, time.Now(), total, lag, skewMs, ver) })
	return total, nil
}

// lagOf is how many records the peer holds that we do not.
func lagOf(peer, ours map[string]uint64) int64 {
	var lag int64
	for origin, n := range peer {
		if n > ours[origin] {
			lag += int64(n - ours[origin])
		}
	}
	return lag
}

// gossip takes the sessions the peer has seen after since into this node's roster
// and returns the newest sighting it was given, to ask for only what is newer next time.
func gossip(st *store.Store, peer, token string, since time.Time) time.Time {
	c := &client.Client{URLs: []string{peer}, Token: token}
	entries, err := c.RosterSince(since)
	if err != nil { // a peer without ?since= or a down one is retried next time
		return since
	}
	st.MergePresence(entries)
	newest := since
	for _, e := range entries {
		if e.Seen.After(newest) && e.Seen.Before(time.Now().Add(maxClockAhead)) {
			newest = e.Seen
		}
	}
	return newest.Add(-time.Second) // a second of overlap: a sighting made as we asked is not missed
}

const maxClockAhead = 5 * time.Minute
