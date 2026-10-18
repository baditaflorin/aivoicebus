package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// The node remembers, for each machine, which kind of token last authenticated as it: every
// request made with a machine token, and every send or inbox read as an identity on that machine
// with any token. It is how the machine that handed a machine its own token (`aivoicebus token
// hand`) proves that the machine now uses it, without taking the machine's word for it: the
// answer to the hand-over must have been sent with the machine token. It is kept in memory only,
// per node, and says nothing about a token beyond its kind and its name in the token file.

// maxSeenHosts bounds the record: a shared token can send as any made-up machine name.
const maxSeenHosts = 1024

var reSeenHost = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

type seenBoard struct {
	mu    sync.Mutex
	hosts map[string]*msg.TokenSeen
	now   func() time.Time
}

func newSeenBoard() *seenBoard {
	return &seenBoard{hosts: map[string]*msg.TokenSeen{}, now: time.Now}
}

// kindOf is the kind of token p authenticated with: machine or shared (the observer cannot act as anyone).
func kindOf(p Principal) string {
	if p.Host != "" {
		return "machine"
	}
	return "shared"
}

// note records that who authenticated as host; sendID is the message it sent, "" for any other request.
func (b *seenBoard) note(host string, who Principal, sendID string) {
	host = strings.ToLower(host)
	if who.Observer || !reSeenHost.MatchString(host) {
		return
	}
	now := b.now().UTC()
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.hosts[host]
	if e == nil {
		if len(b.hosts) >= maxSeenHosts {
			b.dropOldest()
		}
		e = &msg.TokenSeen{Host: host}
		b.hosts[host] = e
	}
	e.Kind, e.Name, e.At = kindOf(who), who.Name, now
	if sendID != "" {
		e.SendID, e.SendKind, e.SendName, e.SendAt = sendID, kindOf(who), who.Name, now
	}
}

// dropOldest forgets the machine heard from longest ago. The caller holds mu.
func (b *seenBoard) dropOldest() {
	var oldest string
	var at time.Time
	for h, e := range b.hosts {
		if oldest == "" || e.At.Before(at) {
			oldest, at = h, e.At
		}
	}
	delete(b.hosts, oldest)
}

func (b *seenBoard) get(host string) msg.TokenSeen {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.hosts[host]; e != nil {
		return *e
	}
	return msg.TokenSeen{Host: host}
}

// handler answers GET /v1/token/seen?host=H with what this node last saw of H.
func (b *seenBoard) handler(node string) principalHandler {
	return func(w http.ResponseWriter, r *http.Request, _ Principal) {
		host := strings.ToLower(r.URL.Query().Get("host"))
		if !reSeenHost.MatchString(host) {
			http.Error(w, "bad request: host is a machine name (lowercase letters, digits, . _ -)", http.StatusBadRequest)
			return
		}
		e := b.get(host)
		e.Node = node
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(e)
	}
}
