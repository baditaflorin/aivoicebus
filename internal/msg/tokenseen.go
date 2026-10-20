package msg

import (
	"regexp"
	"time"
)

// TokenHandTag starts the task that hands a machine its own machine token (aivoicebus token hand):
// a session with a person takes it, never a worker, since installing it changes a credential.
const TokenHandTag = "[token hand v1]"

// TokenSeen is what a node last saw of one machine's credentials (GET /v1/token/seen?host=H,
// FeatureTokenSeen): the kind of token ("machine" or "shared") and its name in the node's token
// file, of the last request made as that machine, and of the last message sent as it. A node
// knows only what reached it (kept across its restarts); the zero At is "nothing yet". Release is the
// client release the machine last said it runs (HeaderClient), at ReleaseAt.
type TokenSeen struct {
	Node      string    `json:"node"`
	Host      string    `json:"host"`
	Kind      string    `json:"kind,omitempty"`
	Name      string    `json:"name,omitempty"`
	At        time.Time `json:"at,omitzero"`
	SendID    string    `json:"send_id,omitempty"`
	SendKind  string    `json:"send_kind,omitempty"`
	SendName  string    `json:"send_name,omitempty"`
	SendAt    time.Time `json:"send_at,omitzero"`
	Release   string    `json:"release,omitempty"`
	ReleaseAt time.Time `json:"release_at,omitzero"`
}

// ValidRelease reports whether v looks like a release a client says it runs (1.2.3, 0.11.3-dev):
// anything else in HeaderClient is ignored.
func ValidRelease(v string) bool { return reRelease.MatchString(v) }

var reRelease = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}(-[a-z0-9.]{1,20})?$`)
