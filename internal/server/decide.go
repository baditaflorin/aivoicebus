package server

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// /v1/decide is the one route that takes no token: a phone button (ntfy) cannot hold the mesh
// token, and must not. What it may do is narrow: record a decision in the thread of a write-mode
// worker's approval request, and only with the one-time code whose SHA-256 that request carries,
// once per request, at most decideLimit times an hour on this node, within decideMaxAge of the
// request. The worker checks the code again; nobody else acts on what this records.

const (
	decideLimit  = 60
	decideMaxAge = 2 * time.Hour
)

type decider struct {
	mu      sync.Mutex
	used    []time.Time
	decided map[string]bool
}

func (d *decider) allow(id string, now time.Time) (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	keep := d.used[:0]
	for _, t := range d.used {
		if now.Sub(t) < time.Hour {
			keep = append(keep, t)
		}
	}
	d.used = keep
	switch {
	case len(d.used) >= decideLimit:
		return false, "too many decisions on this node this hour"
	case d.decided[id]:
		return false, "this request was decided already"
	}
	d.used = append(d.used, now)
	return true, ""
}

func (d *decider) done(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.decided == nil {
		d.decided = map[string]bool{}
	}
	d.decided[id] = true
}

func decideHandler(s *store.Store) http.HandlerFunc {
	d := &decider{}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		id, say, code := q.Get("id"), q.Get("say"), q.Get("code")
		if !msg.ValidID(id) || (say != "approve" && say != "deny") || len(code) != 32 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		now := time.Now().UTC()
		var req msg.Message
		for _, m := range s.Thread(id) {
			if m.ID == id {
				req = m
			}
		}
		if req.ID == "" || req.Retracted != nil || !strings.HasPrefix(req.Body, "[approval needed]") || now.Sub(req.At) > decideMaxAge {
			http.Error(w, "no such open approval request on this node", http.StatusNotFound)
			return
		}
		if !msg.PhoneCodeMatches(req.Body, code) {
			http.Error(w, "the code does not match the request", http.StatusForbidden)
			return
		}
		if ok, why := d.allow(id, now); !ok {
			http.Error(w, why, http.StatusTooManyRequests)
			return
		}
		if _, err := s.Send(msg.Message{ID: msg.NewID(), From: msg.PhoneIdentity, To: req.From, Re: req.ID, Thread: req.Thread,
			Prio: msg.PrioUrgent, Body: msg.PhoneDecisionBody(say, code), At: now}); err != nil {
			http.Error(w, "could not record it: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		d.done(id)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("recorded: " + say + ". The worker on " + msg.HostOf(req.From) + " reads it within seconds.\n"))
	}
}
