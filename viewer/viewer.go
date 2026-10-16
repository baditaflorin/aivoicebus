// Package viewer is the read-only view of a mesh that `aivoicebus view` serves: session cards (who is
// around, what each is called and working on) and the conversations between sessions. It keeps its
// own replica of the board, fed by /v1/sync with an observer token, in the node's own store, so it
// applies exactly the rules a node does (retractions, quarantine, forged system messages) and copies
// none of them. It is a package of its own, outside internal/, so a dashboard can mount the same
// handler. Experimental until 1.0: the JSON shapes may change.
package viewer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// Options configure Open.
type Options struct {
	// Client reaches the nodes. Give it an observer token: the viewer only reads.
	Client *client.Client
	// Dir holds the replica's log. It holds every message in clear text: a private directory the
	// caller removes when done, unless it means to keep an archive.
	Dir string
	// Every is how often the nodes are asked for news (default 3 seconds).
	Every time.Duration
	// Title is the page's name (default "aivoicebus viewer").
	Title string
}

// Viewer follows a mesh and serves what it holds.
type Viewer struct {
	st    *store.Store
	c     *client.Client
	every time.Duration
	title string

	mu       sync.Mutex
	rev      int64     // bumped whenever the replica or the roster changes, so a page knows to reload
	lastPull time.Time // when the nodes last answered
	lastErr  string
	roster   []msg.RosterEntry
}

// Open makes a viewer over the replica in o.Dir. It does not contact the nodes; Run (or Pull) does.
func Open(o Options) (*Viewer, error) {
	if o.Dir == "" {
		return nil, errors.New("viewer: no directory for the replica")
	}
	st, err := store.Open(o.Dir, "viewer")
	if err != nil {
		return nil, err
	}
	if o.Every <= 0 {
		o.Every = 3 * time.Second
	}
	if o.Title == "" {
		o.Title = "aivoicebus viewer"
	}
	return &Viewer{st: st, c: o.Client, every: o.Every, title: o.Title}, nil
}

// maxRounds bounds one Pull: a node answers a page at a time, and a long log takes several.
const maxRounds = 400

// Pull asks the nodes once for what the replica lacks, page by page, and for the roster.
func (v *Viewer) Pull() error {
	if v.c == nil {
		return nil // a demo: nothing to follow
	}
	changed := 0
	var err error
	for round := 0; round < maxRounds; round++ {
		var recs []msg.Record
		if recs, err = v.c.SyncAll(v.st.VV()); err != nil {
			break
		}
		n, merr := v.st.Merge(recs)
		if merr != nil {
			err = merr
			break
		}
		changed += n
		if n == 0 {
			break
		}
	}
	var roster []msg.RosterEntry
	if err == nil {
		roster, err = v.c.Roster()
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err != nil {
		v.lastErr = err.Error()
		return err
	}
	v.lastErr, v.lastPull = "", time.Now()
	if changed > 0 || !sameRoster(roster, v.roster) {
		v.rev++
	}
	v.roster = roster
	return nil
}

func sameRoster(a, b []msg.RosterEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Me != b[i].Me || !a[i].Seen.Equal(b[i].Seen) || a[i].Title != b[i].Title || a[i].Labels.String() != b[i].Labels.String() {
			return false
		}
	}
	return true
}

// Run follows the mesh until ctx ends.
func (v *Viewer) Run(ctx context.Context) {
	t := time.NewTicker(v.every)
	defer t.Stop()
	for {
		_ = v.Pull()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// snapshot is what one page load reads, under one lock for the roster.
func (v *Viewer) snapshot() (roster []msg.RosterEntry, rev int64, last time.Time, lastErr string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]msg.RosterEntry(nil), v.roster...), v.rev, v.lastPull, v.lastErr
}

// titles maps each identity to the title the roster has for it.
func titles(roster []msg.RosterEntry) map[string]string {
	t := map[string]string{}
	for _, e := range roster {
		if e.Title != "" {
			t[e.Me] = e.Title
		}
	}
	return t
}
