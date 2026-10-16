package secrets

import "strings"

// MinCanaryLen is the shortest value a Canaries set will hold. A shorter string could
// turn up in ordinary text and refuse a harmless message; every real token is longer.
const MinCanaryLen = 16

// What a Canaries set says it found, for Refusal. The value itself is never named.
const (
	KindMeshToken   = "token this mesh accepts"
	KindLocalSecret = "secret from this machine's aivoicebus config"
)

// Canaries is a set of secrets that are known exactly (the tokens a node accepts, the files
// under ~/.config/aivoicebus), so a body that holds one can be refused with no guessing and
// no false positive. The patterns in Find only recognise shapes; this recognises the very
// values that must never be sent. The server and the CLI both use it.
type Canaries struct {
	kind string
	vals []string
}

// NewCanaries starts an empty set whose findings are reported as kind.
func NewCanaries(kind string) *Canaries { return &Canaries{kind: kind} }

// Add puts v in the set. Surrounding space is dropped, and a value shorter than
// MinCanaryLen is ignored.
func (c *Canaries) Add(v string) {
	if v = strings.TrimSpace(v); len(v) >= MinCanaryLen {
		c.vals = append(c.vals, v)
	}
}

// Find reports whether body contains any value of the set. The finding names the kind of
// secret, never the value, so the refusal cannot spread the secret it is about.
func (c *Canaries) Find(body string) (Finding, bool) {
	if c == nil {
		return Finding{}, false
	}
	for _, v := range c.vals {
		if strings.Contains(body, v) {
			return Finding{Kind: c.kind, Strong: true}, true
		}
	}
	return Finding{}, false
}
