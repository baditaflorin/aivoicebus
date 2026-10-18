package msg

import "time"

// TokenSeen is what a node last saw of one machine's credentials (GET /v1/token/seen?host=H,
// FeatureTokenSeen): the kind of token ("machine" or "shared") and its name in the node's token
// file, of the last request made as that machine, and of the last message sent as it. A node
// knows only what reached it since it started; the zero At is "nothing yet".
type TokenSeen struct {
	Node     string    `json:"node"`
	Host     string    `json:"host"`
	Kind     string    `json:"kind,omitempty"`
	Name     string    `json:"name,omitempty"`
	At       time.Time `json:"at,omitzero"`
	SendID   string    `json:"send_id,omitempty"`
	SendKind string    `json:"send_kind,omitempty"`
	SendName string    `json:"send_name,omitempty"`
	SendAt   time.Time `json:"send_at,omitzero"`
}
