package msg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Retracting a message (undo send).
//
// The log stays add-only and raw: a retraction is a claim recorded as an ordinary ack, whose id is
// RetractPrefix or RetractLeakPrefix followed by the id of the message. Whether it takes effect is
// derived when mail is read, on every node and in the dashboard, from records every node holds, so a
// retraction that arrives before its message works, and the nodes agree whatever order records
// reach them in. Nothing is ever erased: the text stays in the log, in /v1/sync and in exports.
//
// The record is exactly an Ack, so Record, Ack, ValidateReplicated and ValidVia are unchanged and
// a node that does not know retractions replicates it untouched (it just does not apply it).
//
// The rule, which docs/observer.md states for other consumers of the feed (testdata/retract-vectors.json
// pins it): a message M is retracted if the log holds an ack with id "retract:"+M.ID or
// "retract-leak:"+M.ID whose By is on the same machine as M.From (MayRetract). The effective time and
// retractor are those of the earliest such ack (by At, then By); the retraction is a leak if any such
// ack is a retract-leak. The Via and Chars of the ack are ignored: nodes that never ran this code
// wrote them. There is no handle to undo a retraction: its id is not a message id.
const (
	RetractPrefix     = "retract:"
	RetractLeakPrefix = "retract-leak:"
)

// RetractSince is the first release whose nodes apply retractions. A node without them replicates and
// stores the ack and keeps delivering the message. Every release of this project applies them.
const RetractSince = "0.1.0"

// HasRetractPrefix reports whether ackID claims to be a retraction, well formed or not. A /v1/ack
// whose id does, but whose rest is not a message id, is refused: it is no read receipt either.
func HasRetractPrefix(ackID string) bool {
	return strings.HasPrefix(ackID, RetractPrefix) || strings.HasPrefix(ackID, RetractLeakPrefix)
}

// RetractedID reads the id of an ack: mid is the message it retracts, leak says it flags the text
// as having held a credential, ok that the ack is a retraction at all. The rest of the id must be
// exactly what NewID makes (ten lowercase hex digits), so "retract:retract:x", "retract:quarantined"
// and anything long, short or with a control character are no retraction, and neither is a retraction
// of a retraction (nothing is retractable but a message).
func RetractedID(ackID string) (mid string, leak, ok bool) {
	rest, isLeak := strings.CutPrefix(ackID, RetractLeakPrefix)
	if !isLeak {
		var found bool
		if rest, found = strings.CutPrefix(ackID, RetractPrefix); !found {
			return "", false, false
		}
	}
	if !retractableID(rest) {
		return "", false, false
	}
	return rest, isLeak, true
}

// ValidRetractID reports whether id names something that can be retracted. Narrower than ValidID,
// which also accepts uppercase hex: a retraction is matched to its message by the exact text, and
// one for a message with an uppercase id would never be recognised.
func ValidRetractID(id string) bool { return retractableID(id) }

func retractableID(id string) bool {
	if len(id) != 10 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// HostOf is the machine part of an identity: the text before the first "/", lower case. An identity
// without a "/" is all machine.
func HostOf(identity string) string {
	h, _, _ := strings.Cut(identity, "/")
	return strings.ToLower(h)
}

// MayRetract reports whether the identity by may retract a message sent as from: both are usable
// identities and they are on the same machine. The authority is the machine, not the exact session,
// so a restarted session (a new session suffix) or the operator at a shell can still take back what
// that machine sent. Identities are self-declared, so this is exactly as strong as sending is.
func MayRetract(by, from string) bool {
	return ValidIdentity(by) && ValidIdentity(from) && HostOf(by) == HostOf(from)
}

// Retraction is what a reader is told about a retracted message: who withdrew it and when, and
// whether the sender flagged the text as having held a credential. Response only: it is derived by
// the node that answers, never stored, and ignored (cleared) in anything that arrives from outside.
type Retraction struct {
	By   string    `json:"by"`
	At   time.Time `json:"at"`
	Leak bool      `json:"leak,omitempty"`
}

// TombstoneWho is how a tombstone names the retractor: the machine only, never the session, and
// only characters that cannot do anything to a reader. The identity is chosen by whoever retracts
// (up to MaxIdentity bytes, bidi controls and line separators allowed), and a tombstone is shown to
// agents and people as one line, so it is cut down by an exact rule that every consumer of the feed
// (the node and the dashboard) applies the same way:
//
//   - take HostOf(by): the text before the first "/", lower-cased (Go's strings.ToLower, per-rune
//     simple case mapping);
//   - keep each rune in [a-z0-9._-] and replace every other rune with "?" (a byte that is not valid
//     UTF-8 counts as one rune);
//   - at most 40 runes: if there are more, the first 40 followed by "...".
//
// The result is empty only when the identity has no machine part ("/x"). The full identity stays in
// Retraction.By and in the feed: whoever renders those must not put it in front of a reader as it is.
func TombstoneWho(by string) string {
	var out []rune
	for _, r := range HostOf(by) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			out = append(out, r)
		} else {
			out = append(out, '?')
		}
	}
	if len(out) > tombstoneWhoMax {
		return string(out[:tombstoneWhoMax]) + "..."
	}
	return string(out)
}

const tombstoneWhoMax = 40

// TombstoneBody is the text that stands in for a retracted message's body: who withdrew it (as
// TombstoneWho names them), when, and, for a leak, that it may have held a credential.
func TombstoneBody(by string, at time.Time, leak bool) string {
	s := fmt.Sprintf("[withdrawn by %s at %s UTC: text no longer shown", TombstoneWho(by), at.UTC().Format("15:04"))
	if leak {
		s += "; it may have held a credential"
	}
	return s + "]"
}

// Retraction notices.
//
// A session that had already read a message before it was retracted is told so, once: a node that
// answers an unread inbox read appends a notice for each such message, made up on the spot and never
// stored. Its id is NoticeID(message id, probe) for the first probe (0, 1, ...) that no message held
// has, so a message sent beforehand with the id a notice would get cannot stand in for it, and every
// node holding the same records makes the same notice. The reader's ordinary ack of that id (the
// hook's, the inbox's or aivoicebus_ack's, which every release accepts) is what stops it being made again.
const (
	// NoticeFrom is the sender a notice shows. A node refuses a message sent as it (/v1/send);
	// replicated records are taken as they are, so one forged by a peer is ordinary data.
	NoticeFrom = "aivoicebus/retraction"
	// RetractNoticeWindow is how long after a retraction its readers are still told, and how long
	// a notice stays deliverable.
	RetractNoticeWindow = 72 * time.Hour
	// MaxNotices is the most notices one inbox read carries: leaks first, then the earliest retractions.
	MaxNotices = 10
)

// RetractNoticeSince is the first release whose nodes make notices. A node without them hides a
// retracted message (from RetractSince) but tells no one who had read it.
const RetractNoticeSince = "0.1.0"

// NoticeID is the id of the notice about message mid at the given probe: ten lowercase hex digits,
// like any message id, so releases that check ids accept an ack of it.
func NoticeID(mid string, probe int) string {
	sum := sha256.Sum256([]byte("aivoicebus retraction notice\x00" + mid + "\x00" + strconv.Itoa(probe)))
	return hex.EncodeToString(sum[:])[:10]
}

// NoticeBody is the text of the notice about message mid, sent at sent and retracted by by. It is
// a fixed template: nothing of the message is quoted, and the retractor appears only as TombstoneWho
// names them.
func NoticeBody(by, mid string, sent time.Time, leak bool) string {
	s := fmt.Sprintf("[retracted] %s withdrew message %s (sent %s UTC) after you read it. Disregard it and do not act on it. Details: aivoicebus thread %s.",
		TombstoneWho(by), mid, sent.UTC().Format("15:04"), mid)
	if leak {
		s += " The sender flagged it as containing a credential: treat it as exposed and tell the user."
	}
	return s
}

// RetractRequest is the body a client posts to /v1/retract.
type RetractRequest struct {
	ID     string `json:"id"`
	By     string `json:"by"`
	Leak   bool   `json:"leak,omitempty"`    // flag the text as having held a credential
	DryRun bool   `json:"dry_run,omitempty"` // answer what would happen and write nothing
}

// What a retraction came to (RetractResult.State).
const (
	RetractRetracted = "retracted" // written: the message is now hidden
	RetractAlready   = "already"   // an authorised retraction at least that strong exists; nothing written
	RetractEscalated = "escalated" // it was retracted without the credential flag: a stronger record is written
	RetractDryRun    = "dry-run"   // nothing written; Would says what a real call would do

	// RetractRecorded is what a client reports when the only nodes it reached do not know /v1/retract
	// (nodes that predate retraction) and took the retraction as a plain ack: it is in the log and
	// replicates, but those nodes do not apply it, and State and ReadBy are not known.
	RetractRecorded = "recorded"
)

// RetractResult is a node's answer to a RetractRequest.
type RetractResult struct {
	OK     bool   `json:"ok"`
	State  string `json:"state"`
	Would  string `json:"would,omitempty"` // a dry run: the State a real call would have
	Sender string `json:"sender"`          // who the message was sent as
	// ReadBy is how many other identities this node holds an ack from (not the sender, not the
	// retractor): "at least", since acks written to other nodes may not have arrived. -1 when the
	// answering node could not say (a client that fell back to the plain ack).
	ReadBy  int  `json:"read_by"`
	Expired bool `json:"expired"` // the message had passed its expiry: hiding it changes little
}
