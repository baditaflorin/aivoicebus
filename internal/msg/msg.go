// Package msg holds the wire and domain types of the board plus its
// addressing rules. It does no I/O and imports nothing from this module.
package msg

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// MaxBody is the largest accepted message body, in bytes.
const MaxBody = 32 << 10

// HeaderLatest is the response header a node adds when it holds a release: the
// newest release number. It is only a hint; clients verify the signed
// manifest before acting on it.
const HeaderLatest = "X-Aivoicebus-Latest"

// HeaderVersion is the response header a node adds to every authenticated answer:
// the release it runs, so peers and `aivoicebus mesh` can spot version skew.
const HeaderVersion = "X-Aivoicebus-Version"

// Message is one entry on the board. Bodies are untrusted data.
type Message struct {
	ID     string    `json:"id"`
	From   string    `json:"from"` // host/agent
	To     string    `json:"to"`   // all | host | host/agent | */agent | host/* | @selector
	Thread string    `json:"thread"`
	Re     string    `json:"re,omitempty"`
	Body   string    `json:"body"`
	At     time.Time `json:"at"`

	// Prio is "fyi", "ask" or "urgent"; empty means "ask". A session start shows
	// urgent and ask messages and only counts fyi ones.
	Prio string `json:"prio,omitempty"`
	// TTL is how long, in seconds, the sender wants the message to stay relevant.
	// It only travels from the client to a node, which turns it into Exp.
	TTL int64 `json:"ttl,omitempty"`
	// Exp is when the message stops being delivered, set by the node as At + TTL.
	Exp *time.Time `json:"exp,omitempty"`

	// Kind is "question" (the sender wants an answer), "answer" (a reply that closes
	// the question named by Re) or empty (a plain note). A question that can reach
	// several sessions stops being delivered to the ones that have not read it once
	// someone else has answered it; the thread still shows everything.
	Kind string `json:"kind,omitempty"`
	// Reach is how many sessions the sender's client counted as recipients when it
	// sent the message. A display hint from the sender, never used for delivery;
	// nil when the client could not tell.
	Reach *int `json:"reach,omitempty"`
	// FromTitle is the title of the sender's session when it sent the message (what its tool called
	// the session then), so an old conversation keeps its names after the roster has forgotten the
	// session. Self-declared and cleaned by CleanTitle; empty when the session had none or opted out.
	FromTitle string `json:"from_title,omitempty"`

	// Retracted is set by a node on a copy of a message its sender withdrew (see retract.go),
	// whose Body is then a tombstone. Response only: it is never stored or replicated, and a
	// message that arrives with it set has it cleared.
	Retracted *Retraction `json:"retracted,omitempty"`
}

// Message kinds.
const (
	KindQuestion = "question"
	KindAnswer   = "answer"
)

// MaxReach bounds the Reach a sender may claim.
const MaxReach = 100000

// Message priorities.
const (
	PrioFYI    = "fyi"
	PrioAsk    = "ask"
	PrioUrgent = "urgent"
)

// errTTLRange is what a time to live outside the bounds is told, in words a person reads.
var errTTLRange = errors.New("ttl must be between 1 minute and 30 days")

// Bounds for a message's time to live.
const (
	MinTTL = time.Minute
	MaxTTL = 30 * 24 * time.Hour
)

// Priority is the message's priority with the default filled in.
func (m Message) Priority() string {
	if m.Prio == "" {
		return PrioAsk
	}
	return m.Prio
}

// How long a bulletin stays deliverable when its sender set no TTL.
const (
	LifeFYI   = 24 * time.Hour // news the reader can ignore
	LifeOther = 72 * time.Hour // anything that asks or alarms
)

// IsBulletin reports whether address to can reach more than one session over
// time: everyone, a selector, a whole host, a whole tool, or every session of a
// tool. Every new session reads what is addressed to it and has not read yet, so
// a bulletin without a lifetime would be handed to each session that ever starts,
// for ever. A message to one exact session (host/tool.session) has one reader.
func IsBulletin(to string) bool {
	if to == "all" || IsSelector(to) {
		return true
	}
	host, agent, hasAgent := strings.Cut(to, "/")
	if !hasAgent || host == "*" || agent == "*" {
		return true // a whole host, */tool or host/*
	}
	return !strings.Contains(agent, ".") // host/tool, not host/tool.session
}

// DefaultLife is how long a bulletin of priority prio stays deliverable when its
// sender gave no TTL: a day for fyi, three days otherwise.
func DefaultLife(prio string) time.Duration {
	if prio == PrioFYI {
		return LifeFYI
	}
	return LifeOther
}

// ExpiresAt is when the message stops being delivered, and false if it never
// does: the expiry its sender chose, else the default lifetime of a bulletin,
// else never. Evaluated when read, so messages already in the log are covered too.
func (m Message) ExpiresAt() (time.Time, bool) {
	if m.Exp != nil {
		return *m.Exp, true
	}
	if m.At.IsZero() || !IsBulletin(m.To) {
		return time.Time{}, false
	}
	return m.At.Add(DefaultLife(m.Priority())), true
}

// Expired reports whether the message has passed its expiry at time now.
func (m Message) Expired(now time.Time) bool {
	exp, ok := m.ExpiresAt()
	return ok && now.After(exp)
}

// ValidateOptions checks the optional fields a sender controls.
func (m Message) ValidateOptions() error {
	if m.Prio != "" && !has(Priorities, m.Prio) {
		return fmt.Errorf("priority %q is not one of %s", m.Prio, strings.Join(PrioNames(), ", "))
	}
	if m.TTL != 0 {
		if d := time.Duration(m.TTL) * time.Second; d < MinTTL || d > MaxTTL {
			return errTTLRange
		}
	}
	if m.Kind != "" && !has(Kinds, m.Kind) {
		return fmt.Errorf("kind %q is not one of %s", m.Kind, strings.Join(KindNames(), ", "))
	}
	if m.Kind == KindAnswer && m.Re == "" {
		return errors.New("an answer must say which message it answers (re)")
	}
	if m.Reach != nil && (*m.Reach < 0 || *m.Reach > MaxReach) {
		return errors.New("reach is out of range")
	}
	return nil
}

// Bounds on the fields readers are shown verbatim in a header line.
const (
	MaxIdentity = 200 // From, To, and the `me` of a read
	MaxRef      = 64  // Thread and Re
)

// ValidIdentity reports whether s is a sane identity or target: not empty, at most MaxIdentity
// characters, and free of control characters (a newline in a sender would forge extra lines in
// what an agent reads).
func ValidIdentity(s string) bool { return s != "" && len(s) <= MaxIdentity && !hasControl(s) }

// ValidRef reports whether s can name a message (an id, as an ack carries it): present, short,
// and free of control characters.
func ValidRef(s string) bool { return s != "" && len(s) <= MaxRef && !hasControl(s) }

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// ValidateShape checks the fields that are put in front of readers verbatim: bounded, and without
// control characters, so a hostile sender cannot fill a reader's context through a header or
// break its layout. The body has its own cap (MaxBody).
func (m Message) ValidateShape() error {
	switch {
	case !ValidIdentity(m.From):
		return errors.New("from is empty, too long or contains control characters")
	case !ValidIdentity(m.To):
		return errors.New("to is empty, too long or contains control characters")
	case len(m.Thread) > MaxRef || hasControl(m.Thread):
		return errors.New("thread is too long or contains control characters")
	case len(m.Re) > MaxRef || hasControl(m.Re):
		return errors.New("re is too long or contains control characters")
	}
	return nil
}

// ParseSpan reads a positive span of time: a Go duration (90m, 2h) or whole days (3d).
func ParseSpan(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	bad := fmt.Errorf("%q is not a duration like 90m, 2h or 3d", s)
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 || n > math.MaxInt64/int(24*time.Hour) { // beyond that the span wraps around
			return 0, bad
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil || d <= 0 {
			return 0, bad
		}
	}
	return d, nil
}

// ParseTTL reads a time to live (see ParseSpan), within MinTTL and MaxTTL.
func ParseTTL(s string) (time.Duration, error) {
	d, err := ParseSpan(s)
	if err != nil {
		return 0, fmt.Errorf("ttl %v", err)
	}
	if d < MinTTL || d > MaxTTL {
		return 0, errTTLRange
	}
	return d, nil
}

// Ack records that identity By has read message ID. Via and Chars are a
// delivery receipt: how the message reached By and how many characters it put
// into By's context, so waste can be measured. Both are optional.
type Ack struct {
	ID    string    `json:"id"`
	By    string    `json:"by"`
	At    time.Time `json:"at"`
	Via   string    `json:"via,omitempty"`
	Chars int       `json:"chars,omitempty"`
}

// AckRequest is the body a client posts to acknowledge a message.
type AckRequest struct {
	ID    string `json:"id"`
	By    string `json:"by"`
	Via   string `json:"via,omitempty"`
	Chars int    `json:"chars,omitempty"`
}

// How a message reached the reader, as recorded in an Ack.
const (
	ViaHook      = "hook"       // shown in full at session start
	ViaHookCount = "hook-count" // only counted at session start (fyi)
	ViaInbox     = "inbox"      // listed by `aivoicebus inbox`
	ViaCLI       = "cli"        // acknowledged by hand with `aivoicebus ack`
)

// ValidVia reports whether via is a known way of delivery ("" means unknown).
func ValidVia(via string) bool {
	switch via {
	case "", ViaHook, ViaHookCount, ViaInbox, ViaCLI:
		return true
	}
	return false
}

// Record is the replication unit: one immutable log entry per (Origin, Seq).
// Nodes keep a version vector (origin -> highest contiguous seq applied) and
// pull whatever a peer has beyond it. Messages and acks are add-only, so
// merging is a union and never conflicts.
type Record struct {
	Origin string   `json:"origin"`
	Seq    uint64   `json:"seq"`
	Msg    *Message `json:"msg,omitempty"`
	Ack    *Ack     `json:"ack,omitempty"`
}

// NewID returns a random 10-hex-char message id.
func NewID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ValidID reports whether id looks like one produced by NewID.
func ValidID(id string) bool {
	if len(id) != 10 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// Matches reports whether address `to` targets identity `me`, for a session
// that declares no labels (so a selector target never matches). See MatchesLabeled.
func Matches(to, me string) bool { return MatchesLabeled(to, me, nil) }

// MatchesLabeled reports whether address `to` targets identity `me`, which
// declares labels l.
//
// Identity is host/agent, and agent is tool or tool.session (several sessions
// of one tool can share a machine: imac/claude.a1b2, imac/claude.c3d4).
// Targets: all | host | host/agent | */agent | host/* | @selector, where an
// agent without a session part (claude) addresses every session of that tool
// (claude.*), one with it (claude.a1b2) addresses exactly that session, and
// "@repo=x,tool=claude" addresses every session whose labels satisfy it.
func MatchesLabeled(to, me string, l Labels) bool {
	if to == "all" || to == me {
		return true
	}
	if IsSelector(to) {
		sel, err := ParseSelector(to)
		return err == nil && sel.Match(l)
	}
	th, ta, tHasAgent := strings.Cut(to, "/")
	mh, ma, _ := strings.Cut(me, "/")
	if !tHasAgent {
		return th == mh // whole host
	}
	if th != "*" && th != mh {
		return false
	}
	if ta == "*" || ta == ma {
		return true
	}
	mtool, _, _ := strings.Cut(ma, ".")
	return !strings.Contains(ta, ".") && ta == mtool
}
