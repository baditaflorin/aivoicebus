package msg

import (
	"fmt"
	"strings"
	"time"
)

// Amending a message (replace it with a corrected one).
//
// An amendment is two ordinary records: the new text as a message A, sent to exactly the audience of
// the original O (same `to`, `re` O, same thread, same priority), and a claim, an ack whose id is
// AmendPrefix followed by A's id, written by the sender. The target is not in the claim: it is A's
// own `re`. Like a retraction the claim is derived when mail is read, on every node, from records
// every node holds, so the nodes agree whatever order the records reach them in, nothing is ever
// erased, and Record, Ack, ValidateReplicated and the feed vectors are unchanged. A node that does
// not know amendments stores the ack as a read receipt for nothing and delivers O and A as two
// ordinary messages: what a correction sent as a reply always did.
//
// The rule (docs/observer.md states it for other consumers of the feed): A is an amendment of O if
//   - O is A's `re`, both are held, and AmendmentPair says A may replace O (same machine, same
//     target, same thread, both plain notes);
//   - the log holds an ack with id "amend:"+A.ID whose By is on the same machine as A.From (MayAmend);
//   - A is not itself retracted (retract A and O is delivered again).
//
// The effective amendment of O is the latest such A by (At, ID); a sibling that lost that race is
// superseded too. A reader that has not read O is then given only A (O is not delivered to it);
// one that has is given A, told which version it had read.
const AmendPrefix = "amend:"

// FeatureAmend is what a node advertises in Health.Features when it applies amendments.
const FeatureAmend = "amend"

// HasAmendPrefix reports whether ackID claims to be an amendment, well formed or not. A /v1/ack
// whose id does, but whose rest is not a message id, is refused: it is no read receipt either.
func HasAmendPrefix(ackID string) bool { return strings.HasPrefix(ackID, AmendPrefix) }

// AmendedID reads the id of an ack: aid is the amendment it claims (the message that replaces
// another), ok that the ack is an amendment claim at all. The rest of the id must be exactly what
// NewID makes (ten lowercase hex digits), as for a retraction.
func AmendedID(ackID string) (aid string, ok bool) {
	rest, found := strings.CutPrefix(ackID, AmendPrefix)
	if !found || !retractableID(rest) {
		return "", false
	}
	return rest, true
}

// MayAmend reports whether the identity by may claim that a message sent as from amends another: it is
// on the machine that sent it, the same authority as MayRetract (a restarted session, or the operator
// at a shell, can still correct what that machine sent).
func MayAmend(by, from string) bool { return MayRetract(by, from) }

// AmendmentPair reports whether message a can replace message o: a replies to o, both were sent from
// the same machine, to the same target, in the same thread, both are plain notes (a task would be
// taken twice, a question answered twice, a claim or a state is not a text), neither is a node's own
// notice, and a is not o. It says nothing of who claimed it: that is MayAmend.
func AmendmentPair(a, o Message) bool {
	return a.Re != "" && a.Re == o.ID && a.ID != o.ID &&
		a.Kind == "" && o.Kind == "" &&
		a.To == o.To && a.Thread == o.Thread &&
		ValidIdentity(a.From) && ValidIdentity(o.From) && HostOf(a.From) == HostOf(o.From) &&
		!IsSystemSender(a.From) && !IsSystemSender(o.From)
}

// Amends is what a reader is told about a message that replaces an earlier one. Response only: it is
// derived by the node that answers, never stored, and ignored (cleared) in anything that arrives from
// outside.
type Amends struct {
	Of   string `json:"of"`             // the message this one replaces
	Read string `json:"read,omitempty"` // the nearest earlier version of it that this reader had already read; empty for a thread read, which has no reader
}

// Amended is what a reader is told about a message that was replaced: which message replaced it and
// when. Response only, like Amends.
type Amended struct {
	By string    `json:"by"` // the id of the amendment
	At time.Time `json:"at"`
}

// AmendTag is the marker after the header of a message that replaces an earlier one.
func AmendTag(at time.Time) string { return "[amended " + at.UTC().Format("15:04") + "Z]" }

// AmendedTag is the marker after the header of a message that was replaced.
func AmendedTag(by string, at time.Time) string {
	return fmt.Sprintf("[amended: replaced by %s at %sZ]", by, at.UTC().Format("15:04"))
}

// AmendNotice is the line put before the text of an amendment shown to a reader who had read the
// version it replaces. It is a fixed template: nothing of either message is quoted.
func AmendNotice(at time.Time, read string) string {
	return fmt.Sprintf("[amended %sZ: this replaces message %s, which you read; use this text, not that one]", at.UTC().Format("15:04"), read)
}
