package msg

import (
	"slices"
	"time"
)

// Health is what a node reports about itself at /v1/health.
type Health struct {
	Node     string            `json:"node"`
	Version  string            `json:"version"`
	Now      time.Time         `json:"now"`
	Log      map[string]uint64 `json:"log"` // origin -> highest contiguous sequence held
	Messages int               `json:"messages"`
	Acks     int               `json:"acks"`
	Sessions int               `json:"sessions"` // sessions this node has heard from

	// Quarantined is how many records this node holds only as an inert placeholder, because the
	// original failed the replication check (Record.ValidateReplicated). 0 on a healthy mesh.
	Quarantined int `json:"quarantined"`

	// Retracted is how many messages this node hides because their sender's machine retracted
	// them (see retract.go). Absent when none is.
	Retracted int `json:"retracted,omitempty"`

	// Features names what this node can do beyond the oldest protocol (the Feature constants), so
	// a client can tell a node that lacks something from one that failed. Absent from a node that
	// advertises none: absent means "unknown", not "none", and a client then falls back to its own probing
	// (see client.Retract). Nodes ignore it from each other; it is not replicated.
	Features []string `json:"features,omitempty"`

	Peers []PeerHealth `json:"peers"`
}

// Features a node advertises in Health.Features.
const (
	FeatureFiles         = "files"           // /v1/files: holds an encrypted file for a few minutes
	FeatureQuarantine    = "quarantine"      // replaces a record that fails replication checks by a placeholder
	FeatureRetract       = "retract"         // /v1/retract: a sender withdraws a message
	FeatureRetractDryRun = "retract-dry-run" // /v1/retract answers a dry run without writing
	FeatureTasks         = "tasks"           // accepts task, claim and end, and delivers a task to its owner only
	FeatureTokenSeen     = "token-seen"      // /v1/token/seen: which kind of token last acted as a machine
	FeatureUnsigned      = "unsigned-report" // /v1/unsigned: who sent messages without a valid signature lately
	FeatureInboxPeek     = "inbox-peek"      // GET /v1/inbox?peek=1 reads what waits for an identity without making it a session
)

// NodeFeatures is what this release of the node can do, sorted.
func NodeFeatures() []string {
	return []string{FeatureDecide, FeatureFiles, FeatureInboxPeek, FeatureInboxWait, FeatureLeases, FeatureQuarantine, FeatureReceipts, FeatureRetract, FeatureRetractDryRun, FeatureShare, FeatureStates, FeatureTasks, FeatureTokenSeen, FeatureTurns, FeatureUnsigned}
}

// Has reports whether the node advertised feature f. False also for a node that advertises
// nothing, which is how a node that advertises none looks: use it to choose a path, never to refuse one.
func (h Health) Has(f string) bool { return slices.Contains(h.Features, f) }

// PeerHealth is how a node's replication from one peer is going.
type PeerHealth struct {
	URL      string    `json:"url"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
	LastOK   time.Time `json:"last_ok,omitempty"`
	Lag      int64     `json:"lag"`      // records the peer held that this node lacked at the last look
	SkewMs   int64     `json:"skew_ms"`  // the peer's clock minus this node's, in milliseconds
	Version  string    `json:"version"`  // the release the peer runs
	Pulled   int64     `json:"pulled"`   // records taken from it since this node started
	Failures int64     `json:"failures"` // pulls that failed since this node started
}
