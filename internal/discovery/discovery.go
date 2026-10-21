// Package discovery teaches a machine the mesh's node list from the mesh itself,
// without trusting the mesh: the list is signed with the release key, so a node
// that is compromised cannot add an address that would receive the shared token.
package discovery

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/release"
)

// Every is how stale the saved list may get before a session start refreshes it.
// Retry is how long a failed attempt (a mesh with no list yet, nodes down) waits
// before the next one.
const (
	Every = 24 * time.Hour
	Retry = 6 * time.Hour
)

// Stale reports whether a session start should try to refresh the node list: it
// is missing or older than Every, and no attempt was made within Retry.
func Stale(now time.Time) bool {
	d := config.LoadDiscovered()
	if !d.TriedAt.IsZero() && now.Sub(d.TriedAt) < Retry {
		return false
	}
	return d.Serial == 0 || d.Expired(now) || now.Sub(d.FetchedAt) >= Every
}

// StartBudget is the most a session start may spend learning the node list: one deadline for
// the whole refresh (every node and both files), not a timeout per request.
const StartBudget = 300 * time.Millisecond

// RefreshIfDue is Refresh for a caller that must never be slowed down by it: it does nothing
// unless the saved list is stale, and the whole attempt, across all nodes, ends after budget.
// If the attempt cannot be remembered (an unwritable config directory) it is not made at all,
// or every start would pay the budget again. The session-start hook and the long-lived MCP
// server (which has no session start of its own to learn from) both use it.
func RefreshIfDue(keys []ed25519.PublicKey, now time.Time, budget time.Duration) {
	if !Stale(now) {
		return
	}
	c, err := client.FromEnv()
	if err != nil {
		return
	}
	if config.MarkDiscoveryTried(now) != nil {
		return
	}
	c.Timeout, c.Deadline = budget, time.Now().Add(budget)
	_, _ = Refresh(c, keys, now)
}

// Result says what a refresh found.
type Result struct {
	Serial  int64
	Nodes   []string
	Added   []string // new since the saved list
	Removed []string
	Changed bool
}

// Refresh fetches the signed node list from the mesh, verifies it with keys, and
// saves it. A list older than the saved one is refused (a replay of an old one),
// the same list only renews the fetch time, and anything that does not verify
// changes nothing.
func Refresh(c *client.Client, keys []ed25519.PublicKey, now time.Time) (Result, error) {
	config.MarkDiscoveryTried(now)
	body, err := c.FetchRelease(release.NodesName, release.MaxNodesBytes)
	if err != nil {
		return Result{}, fmt.Errorf("no node list from the mesh: %v", err)
	}
	sig, err := c.FetchRelease(release.NodesSigName, 1<<10)
	if err != nil {
		return Result{}, fmt.Errorf("no node list signature from the mesh: %v", err)
	}
	n, err := release.VerifyNodes(body, sig, keys)
	if err != nil {
		return Result{}, fmt.Errorf("the node list does not verify: %w", err)
	}
	if n.Expired(now) {
		return Result{}, fmt.Errorf("the node list expired on %s: whoever runs the mesh must sign a new one (aivoicebus release nodes)", n.Until().Local().Format("2006-01-02"))
	}
	saved := config.LoadDiscovered()
	switch {
	case n.Serial < saved.Serial:
		return Result{}, errors.New("the node list is older than the one already saved; refusing a replay")
	case n.Serial == saved.Serial:
		// The retired nodes too: a binary older than them rewrites the file without them.
		saved.FetchedAt, saved.ValidUntil, saved.Retired = now, n.Until(), retiredOf(n)
		_ = config.SaveDiscovered(saved) // saved carries the attempt time marked above
		return Result{Serial: n.Serial, Nodes: saved.Nodes}, nil
	}
	res := Result{Serial: n.Serial, Nodes: n.URLs(), Changed: true}
	for _, u := range res.Nodes {
		if !slices.Contains(saved.Nodes, u) {
			res.Added = append(res.Added, u)
		}
	}
	for _, u := range saved.Nodes {
		if !slices.Contains(res.Nodes, u) {
			res.Removed = append(res.Removed, u)
		}
	}
	if err := config.SaveDiscovered(config.Discovered{Serial: n.Serial, Nodes: res.Nodes, FetchedAt: now, TriedAt: now, ValidUntil: n.Until(), Retired: retiredOf(n)}); err != nil {
		return res, fmt.Errorf("learned the node list but could not save it: %v", err)
	}
	return res, nil
}

// retiredOf is the list's retired nodes as the machine keeps them.
func retiredOf(n *release.Nodes) []config.RetiredNode {
	var out []config.RetiredNode
	for _, r := range n.Retired {
		since, _ := time.Parse(time.RFC3339, r.Since) // ParseNodes checked it; "" is the zero time
		out = append(out, config.RetiredNode{Name: r.Name, URL: strings.TrimRight(r.URL, "/"), Since: since})
	}
	return out
}
