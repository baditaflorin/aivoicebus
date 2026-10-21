package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The node list is signed with the same key as a release, so clients learn about
// new nodes from the mesh without the mesh being able to lie to them: a node that
// is compromised cannot add an address to harvest the shared token, because it
// cannot sign. The list is a separate file from the release, with its own serial,
// so adding a node does not need a new binary.
//
//	nodes.json   the list
//	nodes.sig    base64 ed25519 signature over "aivoicebus/nodes\n" + nodes.json's bytes
//
// The list can also retire nodes: a machine still configured for a retired node stops trying it
// (see config.Resolve), so taking a node out of the mesh needs no edit on each machine. A client
// older than the field ignores it, so the schema stays 1.
const (
	NodesName     = "nodes.json"
	NodesSigName  = "nodes.sig"
	NodesSchema   = 1
	MaxNodes      = 32
	MaxRetired    = 64
	MaxNodesBytes = 64 << 10
	nodesDomain   = "aivoicebus/nodes\n" // a release signature is never valid as a node-list signature, or the reverse
)

// NodeEntry is one mesh node.
type NodeEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// RetiredEntry is a node taken out of the mesh.
type RetiredEntry struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Since string `json:"since,omitempty"` // RFC 3339: when it was taken out
}

// Nodes is the signed list of mesh nodes.
type Nodes struct {
	Schema int    `json:"schema"`
	Serial int64  `json:"serial"` // strictly increases; a client never goes back to an older list
	Date   string `json:"date,omitempty"`
	// ValidUntil (RFC 3339, signed with the rest) is when the list stops being trusted. A
	// serial alone cannot stop a compromised node serving an old but genuinely signed list to a
	// machine that has none saved, or serving the same list for ever: this bounds both.
	ValidUntil string      `json:"valid_until,omitempty"`
	Nodes      []NodeEntry `json:"nodes"`
	// Retired are nodes taken out of the mesh: never one of Nodes.
	Retired []RetiredEntry `json:"retired,omitempty"`
}

// DefaultNodesValidity is how long a freshly signed node list is valid.
const DefaultNodesValidity = 90 * 24 * time.Hour

// Expired reports whether the list is past its validity. A list that names no expiry never is.
func (n *Nodes) Expired(now time.Time) bool {
	t, err := time.Parse(time.RFC3339, n.ValidUntil)
	return n.ValidUntil != "" && (err != nil || now.After(t))
}

// Until is when the list stops being valid (zero when it has no expiry).
func (n *Nodes) Until() time.Time {
	t, _ := time.Parse(time.RFC3339, n.ValidUntil)
	return t
}

// ValidNodeURL reports whether u is a plain node address: http://host:port with
// nothing else, so a signed list cannot smuggle credentials, paths or queries.
func ValidNodeURL(u string) error {
	p, err := url.Parse(u)
	switch {
	case err != nil:
		return err
	case p.Scheme != "http" && p.Scheme != "https":
		return errors.New("must be http or https")
	case p.User != nil:
		return errors.New("must not carry credentials")
	case p.Hostname() == "" || p.Port() == "":
		return errors.New("must be host:port")
	case p.Path != "" || p.RawQuery != "" || p.Fragment != "":
		return errors.New("must have no path, query or fragment")
	}
	return nil
}

// ParseNodes decodes and validates a node list. Verify the signature first.
func ParseNodes(b []byte) (*Nodes, error) {
	if len(b) > MaxNodesBytes {
		return nil, errors.New("node list is too large")
	}
	var n Nodes
	if err := json.Unmarshal(b, &n); err != nil {
		return nil, fmt.Errorf("node list is not valid JSON: %w", err)
	}
	switch {
	case n.Schema != NodesSchema:
		return nil, fmt.Errorf("node list schema %d is not supported (want %d)", n.Schema, NodesSchema)
	case n.Serial <= 0:
		return nil, errors.New("node list has no serial")
	case len(n.Nodes) == 0 || len(n.Nodes) > MaxNodes:
		return nil, fmt.Errorf("node list must hold 1 to %d nodes", MaxNodes)
	}
	if n.ValidUntil != "" {
		if _, err := time.Parse(time.RFC3339, n.ValidUntil); err != nil {
			return nil, fmt.Errorf("node list valid_until %q is not an RFC 3339 time", n.ValidUntil)
		}
	}
	seen := map[string]bool{}
	for _, e := range n.Nodes {
		if err := validNodeName(e.Name); err != nil {
			return nil, err
		}
		if err := ValidNodeURL(e.URL); err != nil {
			return nil, fmt.Errorf("node %s: url %q: %v", e.Name, e.URL, err)
		}
		if seen[e.URL] {
			return nil, fmt.Errorf("node url %s appears twice", e.URL)
		}
		seen[e.URL] = true
	}
	if len(n.Retired) > MaxRetired {
		return nil, fmt.Errorf("node list may retire at most %d nodes", MaxRetired)
	}
	for _, e := range n.Retired {
		if err := validNodeName(e.Name); err != nil {
			return nil, err
		}
		if err := ValidNodeURL(e.URL); err != nil {
			return nil, fmt.Errorf("retired node %s: url %q: %v", e.Name, e.URL, err)
		}
		if e.Since != "" {
			if _, err := time.Parse(time.RFC3339, e.Since); err != nil {
				return nil, fmt.Errorf("retired node %s: since %q is not an RFC 3339 time", e.Name, e.Since)
			}
		}
		if seen[e.URL] { // a node both in the mesh and out of it, or retired twice
			return nil, fmt.Errorf("node url %s appears twice", e.URL)
		}
		seen[e.URL] = true
	}
	return &n, nil
}

func validNodeName(name string) error {
	if name == "" || len(name) > 64 || strings.ContainsAny(name, " \t\n/") {
		return fmt.Errorf("node name %q is not valid", name)
	}
	return nil
}

// URLs lists the node addresses in order.
func (n *Nodes) URLs() []string {
	out := make([]string, len(n.Nodes))
	for i, e := range n.Nodes {
		out[i] = strings.TrimRight(e.URL, "/")
	}
	return out
}

func nodesPayload(b []byte) []byte { return append([]byte(nodesDomain), b...) }

// SignNodes returns the base64 signature of a node list.
func SignNodes(b []byte, priv ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, nodesPayload(b)))
}

// VerifyNodes checks the signature, then parses and validates the list.
func VerifyNodes(b, sig []byte, keys []ed25519.PublicKey) (*Nodes, error) {
	if len(keys) == 0 {
		return nil, ErrNoTrustedKeys
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, ErrBadSignature
	}
	ok := false
	for _, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, nodesPayload(b), raw) {
			ok = true
		}
	}
	if !ok {
		return nil, ErrBadSignature
	}
	return ParseNodes(b)
}

// PackNodesOptions describe a node list to sign into a directory.
type PackNodesOptions struct {
	Dir    string
	Serial int64 // 0 means the current time in seconds, which always increases
	Nodes  []NodeEntry
	// Retired are nodes taken out of the mesh, which machines still configured for them stop trying.
	Retired []RetiredEntry
	Key     ed25519.PrivateKey
	Now     time.Time
	// ValidFor is how long the list is trusted: 0 means DefaultNodesValidity, negative means no expiry.
	ValidFor time.Duration
}

// PackNodes writes nodes.json and nodes.sig into o.Dir and returns the list.
func PackNodes(o PackNodesOptions) (*Nodes, error) {
	if len(o.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("no signing key")
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.Serial == 0 {
		o.Serial = o.Now.Unix()
	}
	n := &Nodes{Schema: NodesSchema, Serial: o.Serial, Date: o.Now.UTC().Format(time.RFC3339), Nodes: o.Nodes, Retired: o.Retired}
	switch {
	case o.ValidFor == 0:
		n.ValidUntil = o.Now.Add(DefaultNodesValidity).UTC().Format(time.RFC3339)
	case o.ValidFor > 0:
		n.ValidUntil = o.Now.Add(o.ValidFor).UTC().Format(time.RFC3339)
	}
	body, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	if _, err := ParseNodes(body); err != nil { // never sign what no client would accept
		return nil, err
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(o.Dir, NodesName), body, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(o.Dir, NodesSigName), []byte(SignNodes(body, o.Key)+"\n"), 0o644); err != nil {
		return nil, err
	}
	return n, nil
}
