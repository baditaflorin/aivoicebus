// Package config discovers client settings: node URLs and the shared token.
// It reads the environment and ~/.config/aivoicebus; it makes no network calls.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// envFileKeys are the settings ~/.config/aivoicebus/env may provide.
// The file and log services are here too, so a hook or an MCP server that does not inherit the
// shell's environment still sends files the way the user set up.
var envFileKeys = []string{"AIVOICEBUS_URL", "AIVOICEBUS_HOST", "AIVOICEBUS_CAPS", "AIVOICEBUS_PIPE_URL", "AIVOICEBUS_TAIL_URL", "AIVOICEBUS_FILE_VIA"}

// Dir returns the per-user config directory, or "" when there is no home.
func Dir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h + "/.config/aivoicebus"
}

// LoadEnvFile fills the envFileKeys settings from
// ~/.config/aivoicebus/env when they are unset, so hooks and non-interactive
// shells work without sourcing it.
// SafeNodeURLs refuses a comma-separated node list that holds anything but the characters a node
// address needs (letters, digits and : / . _ @ - [ ] % , ): the list is written to a file the
// shell sources, so `$(id)` or `;` in the user-info part must never get that far.
func SafeNodeURLs(list string) error {
	for _, r := range list {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(":/._@-[],%", r)) {
			return fmt.Errorf("a node URL may hold only letters, digits, the percent sign and : / . _ @ - [ ] (not %q)", r)
		}
	}
	return nil
}

func LoadEnvFile() {
	dir := Dir()
	if dir == "" {
		return
	}
	b, _ := os.ReadFile(dir + "/env")
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		for _, k := range envFileKeys {
			if v, ok := strings.CutPrefix(l, "export "+k+"="); ok && os.Getenv(k) == "" {
				os.Setenv(k, strings.Trim(v, `"'`))
			}
		}
	}
}

// URLs lists the nodes to try, in order: Resolve's URLs.
func URLs() []string { return Resolve().URLs }

// NodeSet is the nodes this machine uses, and the configured ones the signed node list retired.
type NodeSet struct {
	URLs []string
	// Retired are nodes AIVOICEBUS_URL names that the signed node list took out of the mesh.
	// They are not in URLs, unless Kept.
	Retired []RetiredNode
	// Kept is set when AIVOICEBUS_URL names retired nodes but no node of the list: the list does
	// not apply to this machine, so they are still tried.
	Kept bool
}

// Resolve works out the nodes to try, in order. AIVOICEBUS_URL is comma-separated and always
// comes first. If this machine has learned the mesh's node list (see Discovered) and
// AIVOICEBUS_URL names at least one of its nodes, the list applies: the other nodes it knows
// follow, so a node added later reaches machines set up before it existed, and a configured
// node it retires is dropped, so a machine stops trying a node taken out of the mesh without
// anyone editing its configuration. A client pointed at some other mesh (a dev node, a test)
// shares no node with the list and is left alone. A list never takes away every node a machine
// has: it applies only through a configured node it keeps.
func Resolve() NodeSet {
	var configured []string
	seen := map[string]bool{}
	for _, u := range strings.Split(os.Getenv("AIVOICEBUS_URL"), ",") {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" && !seen[u] {
			configured = append(configured, u)
			seen[u] = true
		}
	}
	d := LoadDiscovered()
	if d.Expired(time.Now()) {
		d = Discovered{}
	}
	retired := map[string]RetiredNode{}
	for _, r := range d.Retired {
		retired[r.URL] = r
	}
	shared := false
	for _, u := range d.Nodes {
		if _, out := retired[u]; seen[u] && !out {
			shared = true
		}
	}
	var ns NodeSet
	for _, u := range configured {
		r, out := retired[u]
		if out {
			ns.Retired = append(ns.Retired, r)
		}
		if !out || !shared {
			ns.URLs = append(ns.URLs, u)
		}
	}
	ns.Kept = len(ns.Retired) > 0 && !shared
	if shared {
		for _, u := range d.Nodes {
			if _, out := retired[u]; !seen[u] && !out {
				ns.URLs = append(ns.URLs, u)
				seen[u] = true
			}
		}
	}
	return ns
}

// Discovered is the mesh's node list as this machine last learned it from a
// signed list (see release.Nodes). The URLs in it were verified when it was saved.
type Discovered struct {
	Serial    int64     `json:"serial"`
	Nodes     []string  `json:"nodes"`
	FetchedAt time.Time `json:"fetched_at"`         // when the list was last confirmed by the mesh
	TriedAt   time.Time `json:"tried_at,omitempty"` // when a refresh was last attempted, whether or not it worked
	// ValidUntil is the signed list's own expiry (zero: none). Past it the learned nodes are
	// not used, so a list nobody renewed cannot keep steering the token to stale addresses.
	ValidUntil time.Time `json:"valid_until,omitempty"`
	// Retired are nodes the list took out of the mesh (see Resolve).
	Retired []RetiredNode `json:"retired,omitempty"`
}

// RetiredNode is a node the signed node list took out of the mesh.
type RetiredNode struct {
	Name  string    `json:"name"`
	URL   string    `json:"url"`
	Since time.Time `json:"since,omitzero"` // when it was taken out (zero: the list does not say)
}

// Expired reports whether the saved list is past the validity its signer gave it.
func (d Discovered) Expired(now time.Time) bool {
	return !d.ValidUntil.IsZero() && now.After(d.ValidUntil)
}

const discoveredFile = "nodes.json"

// LoadDiscovered reads the saved node list; a machine that never learned one gets the zero value.
func LoadDiscovered() Discovered {
	var d Discovered
	dir := Dir()
	if dir == "" {
		return d
	}
	b, err := os.ReadFile(dir + "/" + discoveredFile)
	if err != nil || json.Unmarshal(b, &d) != nil {
		return Discovered{}
	}
	return d
}

// SaveDiscovered writes the node list atomically (mode 0600).
func SaveDiscovered(d Discovered) error {
	dir := Dir()
	if dir == "" {
		return errors.New("no home directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "nodes-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dir+"/"+discoveredFile)
}

// Token resolves the shared bearer token: AIVOICEBUS_TOKEN, else the given
// file, else AIVOICEBUS_TOKEN_FILE, else ~/.config/aivoicebus/token.
func Token(file string) string {
	if v := os.Getenv("AIVOICEBUS_TOKEN"); v != "" {
		return v
	}
	b, _ := os.ReadFile(TokenPath(file))
	return strings.TrimSpace(string(b))
}

// TokenPath is the file Token reads (and `join` writes): file, else AIVOICEBUS_TOKEN_FILE,
// else ~/.config/aivoicebus/token.
func TokenPath(file string) string {
	if file == "" {
		file = os.Getenv("AIVOICEBUS_TOKEN_FILE")
	}
	if file == "" {
		if dir := Dir(); dir != "" {
			file = dir + "/token"
		}
	}
	return file
}

// MarkDiscoveryTried records that a refresh was just attempted, so a mesh that
// has no list yet is not asked again at every session start.
func MarkDiscoveryTried(now time.Time) error {
	d := LoadDiscovered()
	d.TriedAt = now
	return SaveDiscovered(d)
}
