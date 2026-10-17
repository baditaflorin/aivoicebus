package msg

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// A lease says "this session is working on KEY until UNTIL": a file, a branch, a deploy, anything
// two agents must not do at once. It is a message of kind lease, addressed to its own sender (so it
// reaches no inbox), whose body is the key and an optional note, and whose ttl is how long it
// lasts. Releasing it is retracting it. The earliest live lease on a key holds it, by the time its
// node stamped on it and then by id, the rule tasks use, so every node names the same holder; a
// renewal by the holder extends it. A lease is advisory: it tells, it does not stop anyone.

// LeaseSince is the first release whose nodes hold leases (an older one would quarantine them).
const LeaseSince = "0.6.0"

var reLeaseKey = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{1,120}$`)

// ValidLeaseKey reports whether k can name a lease: 1 to 120 letters, digits and . _ : / @ + -.
func ValidLeaseKey(k string) bool { return reLeaseKey.MatchString(k) }

// LeaseBody is the body of a lease message.
func LeaseBody(key, note string) string {
	if note = strings.TrimSpace(note); note != "" {
		return key + "\n" + note
	}
	return key
}

// Lease is one live lease message, as GET /v1/leases lists it.
type Lease struct {
	Key   string    `json:"key"`
	By    string    `json:"by"`
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Until time.Time `json:"until"`
	Note  string    `json:"note,omitempty"`
}

// LeaseOf reads a lease message; false when m is not one or its key is not valid.
func LeaseOf(m Message) (Lease, bool) {
	if m.Kind != KindLease || m.Exp == nil {
		return Lease{}, false
	}
	key, note, _ := strings.Cut(m.Body, "\n")
	if !ValidLeaseKey(key) {
		return Lease{}, false
	}
	return Lease{Key: key, By: m.From, ID: m.ID, At: m.At, Until: *m.Exp, Note: strings.TrimSpace(note)}, true
}

// Holders is who holds each key among live leases: the earliest one, extended to the latest
// Until of that holder's live leases on the key. Sorted by key.
func Holders(live []Lease) []Lease {
	byKey := map[string]Lease{}
	for _, l := range live {
		cur, ok := byKey[l.Key]
		if !ok || (TakeRef{ID: l.ID, At: l.At}).Earlier(TakeRef{ID: cur.ID, At: cur.At}) {
			byKey[l.Key] = l
		}
	}
	out := make([]Lease, 0, len(byKey))
	for k, h := range byKey {
		for _, l := range live {
			if l.Key == k && l.By == h.By && l.Until.After(h.Until) {
				h.Until = l.Until
			}
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
