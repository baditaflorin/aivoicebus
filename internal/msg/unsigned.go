package msg

import "time"

// UnsignedReport is what a node holds of the messages sent without a valid signature (GET
// /v1/unsigned?days=N, FeatureUnsigned): per sender, how many arrived unsigned or with a signature
// that does not verify since Since. A node keeps every message it took in (an expired one too), so
// the answer covers the whole mesh's traffic in that window, not just what reached this node, and
// survives a restart. It is what `aivoicebus mesh --signed-check` reads before a node is switched to
// `serve --require-signed`, which refuses exactly these messages.
type UnsignedReport struct {
	Node          string           `json:"node"`
	Since         time.Time        `json:"since"`
	Scanned       int              `json:"scanned"`        // messages in the window
	RequireSigned bool             `json:"require_signed"` // this node already refuses them
	Senders       []UnsignedSender `json:"senders"`        // most recent first
}

// UnsignedSender is one sender's unsigned (or badly signed) messages in the window.
type UnsignedSender struct {
	From     string    `json:"from"`
	Unsigned int       `json:"unsigned"`
	Bad      int       `json:"bad_signature"`
	LastAt   time.Time `json:"last_at"`
	LastID   string    `json:"last_id"`
}
