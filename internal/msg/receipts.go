package msg

import "time"

// Receipt is one session's first read of a message: when, and how it was shown (Via: at a
// session start, only counted, in an inbox listing, by the CLI or MCP).
type Receipt struct {
	By  string    `json:"by"`
	At  time.Time `json:"at"`
	Via string    `json:"via,omitempty"`
}

// Receipts is what a node knows about whether a message got through: who read it and when, who
// took it when it is a task, and how many replies its thread holds. GET /v1/receipts?id=.
type Receipts struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Kind    string    `json:"kind,omitempty"`
	At      time.Time `json:"at"`
	Reach   *int      `json:"reach,omitempty"`   // the sender's count of recipients when it sent it
	Reads   []Receipt `json:"reads"`             // the sender excluded, earliest first
	Owner   *Receipt  `json:"owner,omitempty"`   // a task: the session that took it, and when
	Replies int       `json:"replies"`           // messages that answer it (re = its id), not a decline or what told the person (offer.go)
	Expired bool      `json:"expired,omitempty"` // no longer delivered
	// Offers is, for a task the nodes offer one session at a time (Offered), who was offered it, when,
	// and what came of it, earliest first. Escalated is the message that told the person nobody took it.
	Offers    []OfferStep `json:"offers,omitempty"`
	Escalated *Receipt    `json:"escalated,omitempty"`
}

// ViaReply marks, in receipts only, a session that answered a message without acknowledging it:
// it got through all the same. Never an ack's Via.
const ViaReply = "reply"

// Node features added in 0.4.0.
const (
	FeatureReceipts  = "receipts"   // GET /v1/receipts?id=
	FeatureInboxWait = "inbox-wait" // GET /v1/inbox?wait=S&gen=G holds the read until something arrives
	FeatureLeases    = "leases"     // GET /v1/leases, and kind lease is held, not quarantined
)

// HeaderGen is the response header an inbox read carries: the node's change counter, which the
// next long-polled read sends back as gen= so it waits only for what is new since.
const HeaderGen = "X-Aivoicebus-Gen"

// MaxInboxWait bounds how long a node holds an inbox read open, inside every write timeout.
const MaxInboxWait = 25 * time.Second
