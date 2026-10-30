package msg

import "strings"

// Settling a message for a whole machine.
//
// A message to a machine (or to all) is put in front of every session that starts there, for as long
// as it lives, and an acknowledgement is one session's. When the person on that machine has done
// the work by hand, or does not want it, nothing recorded it. A settle is one more ack: its id is
// SettlePrefix followed by the message's id, its By is the machine's person (<machine>/human). A node
// that applies it stops delivering that message to every session whose machine is the person's; the
// message stays in the log, in a thread read and in `inbox --all`. Like amendments and retractions
// the claim is judged when mail is read, so replication order does not matter, and Record, Ack,
// ValidateReplicated and the feed vectors are unchanged. A node that does not know settles stores the
// ack as a read receipt for nothing and delivers the message as before (FeatureSettle says which
// ones apply it).
//
// The node cannot tell the person from an agent (both act with the machine's token), so a settle
// never reaches a message that must be answered: an approval request, a [token hand] task, an
// [ask-human] question (Settleable), and the machine's own audit log says who settled what.

// SettlePrefix starts the id of an ack that settles the message whose id follows.
const SettlePrefix = "settle:"

// FeatureSettle is what a node advertises in Health.Features when it applies settles.
const FeatureSettle = "settle"

// SettleSince is the release whose nodes apply settles.
const SettleSince = "0.15.0"

// DoneByPersonTag starts the progress message that tells a sender its message was settled by the
// person; DoneMetTag the one written when a condition the sender gave was met (msg.DoneWhen).
const (
	DoneByPersonTag = "[done by the person]"
	DoneMetTag      = "[done: condition met]"
)

// HasSettlePrefix reports whether ackID claims to be a settle, well formed or not.
func HasSettlePrefix(ackID string) bool { return strings.HasPrefix(ackID, SettlePrefix) }

// SettledID reads the id of an ack: mid is the message it settles, ok that it is a settle at all.
func SettledID(ackID string) (mid string, ok bool) {
	rest, found := strings.CutPrefix(ackID, SettlePrefix)
	if !found || !retractableID(rest) {
		return "", false
	}
	return rest, true
}

// MustAnswer reports whether m is a message that has to be answered, never settled: an approval
// request, a [token hand] task, an [ask-human] question.
func MustAnswer(m Message) bool {
	line, _, _ := strings.Cut(strings.TrimSpace(m.Body), "\n")
	return strings.HasPrefix(line, TokenHandTag) || strings.HasPrefix(line, ApprovalTag) || strings.HasPrefix(line, strings.TrimSpace(AskPrefix))
}

// Settleable reports whether a settle may hide m: not one that must be answered, and not the mesh's
// own message.
func Settleable(m Message) bool {
	return !MustAnswer(m) && !IsSystemSender(m.From)
}

// MaySettle reports whether by may settle a message for host: it is that machine's person.
func MaySettle(by, host string) bool {
	return ValidIdentity(by) && IsHuman(by) && HostOf(by) == strings.ToLower(host)
}

// DoneBody is the progress text the sender is told: tag, then the person's reason (cut).
func DoneBody(tag, why string) string {
	why = strings.Join(strings.Fields(why), " ")
	if r := []rune(why); len(r) > 200 {
		why = string(r[:200])
	}
	if why == "" {
		return tag
	}
	return tag + " " + why
}
