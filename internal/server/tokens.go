package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/secrets"
)

// Principal is who a bearer token belongs to.
type Principal struct {
	Name     string // what the token is called in metrics ("shared" for an unnamed one)
	Host     string // if set, the token may only act as this machine: send, ack and read mail as host/...
	Observer bool   // read-only: the replication feed, roster, threads, health
}

// Fingerprint is a short, non-reversible tag for a token, safe to show in logs
// and metrics so operators can tell which token a request used.
func Fingerprint(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:3])
}

type tokenEntry struct {
	token []byte
	who   Principal
}

// TokenSet is every token a node accepts. More than one lets a token be rotated
// without a flag day (accept old and new, move the machines, drop the old one),
// and lets each machine have its own token that can be revoked alone.
type TokenSet struct {
	entries []tokenEntry
}

// MinTokenLen is the shortest token a node will accept.
const MinTokenLen = 24

// NewTokenSet starts a set with the shared token every node has always had.
func NewTokenSet(shared string) *TokenSet {
	ts := &TokenSet{}
	ts.Add(shared, Principal{Name: "shared"})
	return ts
}

// Add accepts token for p.
func (ts *TokenSet) Add(token string, p Principal) {
	if token == "" {
		return
	}
	ts.entries = append(ts.entries, tokenEntry{token: []byte(token), who: p})
}

// Lookup finds the principal that owns got. It compares against every entry
// every time, so how long it takes says nothing about which one was close.
func (ts *TokenSet) Lookup(got []byte) (Principal, bool) {
	var found Principal
	ok := 0
	for _, e := range ts.entries {
		if subtle.ConstantTimeCompare(got, e.token) == 1 {
			found = e.who
			ok = 1
		}
	}
	return found, ok == 1
}

// Primary is the first unrestricted agent token: the one this node uses to
// authenticate to its peers, so every node must accept it.
func (ts *TokenSet) Primary() string {
	for _, e := range ts.entries {
		if !e.who.Observer && e.who.Host == "" {
			return string(e.token)
		}
	}
	return ""
}

// Has reports whether token is one of the set's tokens.
func (ts *TokenSet) Has(token string) bool {
	for _, e := range ts.entries {
		if string(e.token) == token {
			return true
		}
	}
	return false
}

// Canaries holds every token of the set, so a message that carries one can be refused: a token
// in a message would be stored on every node and shown in the dashboard, where it cannot be taken back.
func (ts *TokenSet) Canaries() *secrets.Canaries {
	c := secrets.NewCanaries(secrets.KindMeshToken)
	for _, e := range ts.entries {
		c.Add(string(e.token))
	}
	return c
}

// Names lists the distinct token names, for metrics.
func (ts *TokenSet) Names() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range ts.entries {
		if !seen[e.who.Name] {
			seen[e.who.Name] = true
			out = append(out, e.who.Name)
		}
	}
	return out
}

// ParseTokens reads a node's token file, one token per line:
//
//	longrandomtoken                 the shared token (any number of them, to rotate)
//	laptop-a:longrandomtoken that machine's own token: it may only act as laptop-a/...
//	~backup:longrandomtoken         a named token with no machine restriction, to tell it apart in metrics
//
// Blank lines and lines starting with # are ignored. A single bare token, which is
// what every node had before, parses as the shared token.
func ParseTokens(text string) (*TokenSet, error) {
	ts := &TokenSet{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := Principal{Name: "shared"}
		bare := true
		tok := line
		if name, rest, ok := strings.Cut(line, ":"); ok {
			bare = false
			name = strings.TrimSpace(name)
			tok = strings.TrimSpace(rest)
			switch {
			case name == "":
				return nil, fmt.Errorf("token file line %d: empty name before ':'", i+1)
			case strings.HasPrefix(name, "~"):
				p = Principal{Name: strings.TrimPrefix(name, "~")}
			default:
				p = Principal{Name: name, Host: strings.ToLower(name)}
			}
		}
		if len(tok) < MinTokenLen {
			return nil, fmt.Errorf("token file line %d: a token is shorter than %d characters", i+1, MinTokenLen)
		}
		for _, e := range ts.entries {
			if string(e.token) == tok {
				return nil, fmt.Errorf("token file line %d: the same token appears twice", i+1)
			}
		}
		if bare { // told apart in metrics by fingerprint, so the old and the new token of a rotation are two series
			p.Name = "shared-" + Fingerprint(tok)
		}
		ts.Add(tok, p)
	}
	if len(ts.entries) == 0 {
		return nil, fmt.Errorf("token file holds no token")
	}
	if ts.Primary() == "" {
		return nil, fmt.Errorf("token file needs at least one unrestricted token for replication")
	}
	return ts, nil
}
