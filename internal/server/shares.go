package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/sharestore"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// Shared spaces (msg/share.go): the node keeps sealed chunks and a log of versions and sealed keys,
// and never a key or a byte of plaintext. Agent tokens only: the observer reads none of it, and
// the bulk feed between nodes (/v1/share/sync) takes the shared token, like /v1/sync.

// maxShareChunkBody is the JSON of one sealed chunk: base64 of msg.ShareChunkMax and the wrapper.
const maxShareChunkBody = (msg.ShareChunkMax+2)/3*4 + 1024

// WithShares makes the node hold shared spaces in sh. ready says when it may take writes (as
// WithReady does for messages); nil means always.
func WithShares(sh *sharestore.Store, ready func() bool) Option {
	return func(c *handlerConfig) { c.shares, c.sharesReady = sh, ready }
}

// ThreadActivity is when thread id last had a message on st (zero if none is held), which keeps
// a shared space alive and lets a writer start one.
func ThreadActivity(st *store.Store) func(string) time.Time {
	return func(id string) time.Time {
		var last time.Time
		for _, m := range st.Thread(id) {
			if m.Thread == id && m.At.After(last) {
				last = m.At
			}
		}
		return last
	}
}

// shareError answers a refusal from the share store.
func shareError(w http.ResponseWriter, err error) {
	var conflict *sharestore.ErrConflict
	var full *sharestore.ErrFull
	var missing *sharestore.ErrMissing
	switch {
	case errors.As(err, &conflict):
		refuse(w, msg.RefusedConflict, err.Error(), http.StatusConflict)
	case errors.As(err, &full), errors.Is(err, sharestore.ErrTooMany):
		refuse(w, msg.RefusedShareFull, err.Error(), http.StatusInsufficientStorage)
	case errors.As(err, &missing):
		refuse(w, msg.RefusedShareMissing, err.Error(), http.StatusBadRequest)
	case errors.Is(err, sharestore.ErrUnknownThread):
		refuse(w, msg.RefusedUnknown, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, sharestore.ErrNotSealed):
		refuse(w, msg.RefusedBadRequest, err.Error(), http.StatusBadRequest)
	case strings.HasPrefix(err.Error(), "bad request:"):
		refuse(w, msg.RefusedBadRequest, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// shareRoutes adds /v1/share/...: upload (a sealed chunk), chunk (one back), put (a version), key
// (a sealed key), index (a thread's space) and sync (the feed between nodes).
func shareRoutes(mux *http.ServeMux, cfg *handlerConfig, upload, agent, feedOnly func(principalHandler) http.HandlerFunc) {
	sh := cfg.shares
	js := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(v)
	}
	ready := func(w http.ResponseWriter) bool {
		if cfg.sharesReady != nil && !cfg.sharesReady() {
			refuse(w, msg.RefusedRecovering, "this node is recovering its shared spaces from its peers: try again in a minute, or use another node", http.StatusServiceUnavailable)
			return false
		}
		return true
	}
	thread := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		id := r.URL.Query().Get("thread")
		if !msg.ValidShareThread(id) {
			refuse(w, msg.RefusedBadRequest, "bad request: thread must be a message id (ten lowercase hex digits), as `aivoicebus inbox` shows it", http.StatusBadRequest)
			return "", false
		}
		return id, true
	}
	mux.HandleFunc("/v1/share/chunk", agent(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		id, ok := thread(w, r)
		if !ok {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed: POST a chunk to /v1/share/upload", http.StatusMethodNotAllowed)
			return
		}
		sha := r.URL.Query().Get("sha")
		data, found := sh.Chunk(id, sha)
		if !msg.ValidShareChunkID(sha) || !found {
			refuse(w, msg.RefusedGone, "gone: this node does not hold that chunk (it expired, or has not reached this node yet): try again in a minute, or ask another node", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		longWrite(w)
		w.Write(data)
	}))
	mux.HandleFunc("/v1/share/upload", upload(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		id, ok := thread(w, r)
		if !ok || !ready(w) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed: POST the chunk", http.StatusMethodNotAllowed)
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(fileUploadTime))
		var in msg.ShareChunkPut
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			refuse(w, msg.RefusedTooLarge, "the chunk is not one a node takes (at most 4 MiB, sealed): `aivoicebus share put` makes them", http.StatusRequestEntityTooLarge)
			return
		}
		sha, err := sh.PutChunk(id, in.Data)
		if err != nil {
			shareError(w, err)
			return
		}
		js(w, msg.ShareChunkDone{SHA256: sha})
	}))
	mux.HandleFunc("/v1/share/put", agent(func(w http.ResponseWriter, r *http.Request, who Principal) {
		if !ready(w) {
			return
		}
		var in msg.SharePut
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&in) != nil || !msg.ValidShareThread(in.Thread) {
			refuse(w, msg.RefusedBadRequest, "bad request: POST {thread, ver}; this aivoicebus and the node may be different releases: run `aivoicebus update`, then try again", http.StatusBadRequest)
			return
		}
		if !who.mayActAs(in.Ver.By) {
			who.deny(w)
			return
		}
		rec, err := sh.Put(in.Thread, in.Ver)
		if err != nil {
			shareError(w, err)
			return
		}
		js(w, rec)
	}))
	mux.HandleFunc("/v1/share/key", agent(func(w http.ResponseWriter, r *http.Request, who Principal) {
		if !ready(w) {
			return
		}
		var in msg.ShareKeyPut
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&in) != nil || !msg.ValidShareThread(in.Thread) {
			refuse(w, msg.RefusedBadRequest, "bad request: POST {thread, key}; this aivoicebus and the node may be different releases: run `aivoicebus update`, then try again", http.StatusBadRequest)
			return
		}
		if !who.mayActAs(in.Key.By) {
			who.deny(w)
			return
		}
		rec, err := sh.PutKey(in.Thread, in.Key)
		if err != nil {
			shareError(w, err)
			return
		}
		js(w, rec)
	}))
	mux.HandleFunc("/v1/share/index", agent(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		id, ok := thread(w, r)
		if !ok {
			return
		}
		idx, _ := sh.Index(id)
		js(w, idx)
	}))
	mux.HandleFunc("/v1/share/sync", feedOnly(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		vv := map[string]uint64{}
		_ = json.Unmarshal([]byte(r.URL.Query().Get("vv")), &vv)
		js(w, sh.Since(vv, syncPageRecords, syncPageBytes))
	}))
}

// shareBoard remembers, per peer, whether it holds shared spaces at all, and whether any peer
// has answered a pull, for ShareReady.
type shareBoard struct {
	mu       sync.Mutex
	answered bool
}

func (b *shareBoard) ok() {
	b.mu.Lock()
	b.answered = true
	b.mu.Unlock()
}

// ShareReady says whether a node may take share writes: it has share records of its own, it has
// no peers, or a peer has answered (which a peer from before shared spaces does with a 404).
func shareReady(sh *sharestore.Store, b *shareBoard, peers int) func() bool {
	return func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return peers == 0 || !sh.Empty() || b.answered
	}
}

// pullShares takes from peer what this node lacks of the shared spaces: records first, then the
// chunks any version held here still needs (from this peer, which may itself still lack some).
// old is true for a peer that answered 404: a release from before shared spaces.
func pullShares(sh *sharestore.Store, peer, token string, out io.Writer) (n int, old bool, err error) {
	c := &client.Client{URLs: []string{peer}, Token: token, Timeout: 2 * time.Minute}
	for {
		recs, err := c.ShareSync(sh.VV())
		if client.IsNotFound(err) {
			return n, true, nil
		}
		if err != nil {
			return n, false, err
		}
		got, _, err := sh.Merge(recs)
		n += got
		if err != nil {
			return n, false, err
		}
		if got == 0 {
			break
		}
	}
	for _, ref := range sh.Missing() {
		data, err := c.ShareChunk(ref.Thread, ref.SHA)
		if err != nil {
			if client.IsNotFound(err) {
				continue // not there yet either
			}
			return n, false, err
		}
		if err := sh.PutReplica(ref.Thread, ref.SHA, data); err != nil {
			fmt.Fprintf(out, "share: chunk %s of thread %s from %s: %v\n", ref.SHA[:12], ref.Thread, peer, err)
		}
	}
	return n, false, nil
}

// ReplicateShares keeps sh in step with every peer until ctx ends, and drops what expired. A peer
// from before shared spaces is asked again only every 20 ticks.
func ReplicateShares(ctx context.Context, sh *sharestore.Store, peers []string, token string, every time.Duration, board *shareBoard, out io.Writer) {
	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			t := time.NewTicker(every)
			defer t.Stop()
			skip := 0
			for {
				if skip > 0 {
					skip--
				} else if _, old, err := pullShares(sh, peer, token, out); err == nil {
					board.ok()
					if old {
						skip = 20
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}(peer)
	}
	sweep := time.NewTicker(time.Minute)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-sweep.C:
			sh.Sweep()
		}
	}
}
