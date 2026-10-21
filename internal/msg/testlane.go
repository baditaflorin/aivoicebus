package msg

import (
	"encoding/hex"
	"strings"
	"time"
)

// The test lane. A message sent as a test (send --test, selftest) reaches only sessions that opted
// in and asked for it, never reaches a hook or a replay, is left out of stats, view and export, and
// is purged about ten minutes after it was sent. What marks it is the message id, which no field
// of the wire format had to give up for:
//
//	7e57 + five random hex digits + one check digit  (the sum of the nine digits before it, mod 16)
//
// It is still ten lowercase hex digits, so ValidID, the replication check, the signature (the id is
// signed) and the id-prefix resolver are unchanged, and an ack or a retraction (RetractPrefix+id)
// names the lane by the id it carries, with no lookup of the message. A message is a test when its
// id has that shape AND it expires within TestLife: a random id of an older client that happens to
// match (1 in 2^20, and only with a short TTL) stays an ordinary message that is delivered. NewID
// never makes one of these ids from this release on.
const (
	TestIDPrefix = "7e57"
	// TestLife is the longest a test message lives: the node stamps its expiry at most this far on,
	// and purges the body after it.
	TestLife = 10 * time.Minute

	// TestLabel is the session label a session opts in with (AIVOICEBUS_TEST=1), TestOn its value.
	TestLabel = "test"
	TestOn    = "on"

	// ParamLane is the query parameter of /v1/inbox and /v1/thread that asks for the test lane:
	// without it a read never contains a test message, whoever reads.
	ParamLane = "lane"
	LaneTest  = "test"

	// FeatureTestLane is what a node advertises when it hides, counts apart and purges test
	// messages. A client sends one only to a mesh whose nodes all have it.
	FeatureTestLane = "testlane"
)

// nibble is the value of a lowercase hex digit, or -1.
func nibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}

// testCheck is the check digit of the first nine digits of a test id.
func testCheck(nine string) byte {
	sum := 0
	for i := 0; i < len(nine); i++ {
		sum += nibble(nine[i])
	}
	return "0123456789abcdef"[sum%16]
}

// NewTestID returns a fresh test id (see the shape above).
func NewTestID() string {
	b := make([]byte, 3)
	randRead(b)
	nine := TestIDPrefix + hex.EncodeToString(b)[:5]
	return nine + string(testCheck(nine))
}

// IsTestID reports whether id has the shape of a test id: the prefix, ten lowercase hex digits,
// and the right check digit. Alone it does not make a message a test (see Message.IsTest).
func IsTestID(id string) bool {
	if len(id) != 10 || !strings.HasPrefix(id, TestIDPrefix) {
		return false
	}
	for i := 0; i < len(id); i++ {
		if nibble(id[i]) < 0 {
			return false
		}
	}
	return testCheck(id[:9]) == id[9]
}

// IsTest reports whether m is a test message: a test id and a life of at most TestLife (TTL before
// a node stamps the expiry, Exp - At after). Never a lease, never one for the person on a machine
// (an approval request, a token hand, a decision signed by the approver key): those cannot hide in
// the lane, so such a message with a test id is an ordinary one.
func (m Message) IsTest() bool {
	if !IsTestID(m.ID) || m.Kind == KindLease || PersonOnly(m) || looksLikeApproval(m.Body) {
		return false
	}
	if m.Exp != nil {
		return !m.At.IsZero() && m.Exp.Sub(m.At) <= TestLife
	}
	return m.TTL >= 1 && time.Duration(m.TTL)*time.Second <= TestLife
}

// looksLikeApproval reports whether body is shaped like a decision signed by the approver key.
func looksLikeApproval(body string) bool {
	_, rest, _ := strings.Cut(strings.TrimSpace(body), "\n")
	return strings.HasPrefix(strings.TrimSpace(rest), approvalTag)
}

// TestRefusal says why a message with a test id cannot be sent as a test, or "" when it can. The
// client asks before it sends; a node that is shown such a message treats it as an ordinary one.
func TestRefusal(m Message) string {
	switch {
	case m.Kind == KindLease:
		return "a lease is never a test: it would hold nothing"
	case PersonOnly(m) || looksLikeApproval(m.Body):
		return "a message for the person on a machine (an approval request, a token hand, a decision) cannot be a test: it could not be hidden from the person"
	}
	return ""
}

// IsTestAck reports whether an ack is about a test message, judged by the id it carries: a read
// receipt, or a retraction of one.
func IsTestAck(a Ack) bool {
	if mid, _, ok := RetractedID(a.ID); ok {
		return IsTestID(mid)
	}
	return IsTestID(a.ID)
}

// IsTest reports whether the record holds a test message or an ack of one.
func (r Record) IsTest() bool {
	switch {
	case r.Msg != nil:
		return r.Msg.IsTest()
	case r.Ack != nil:
		return IsTestAck(*r.Ack)
	}
	return false
}

// TestStub is what stays of a test message once it is purged: the record with its body, signature
// and the sender's session names removed, keeping who sent it, to whom, when and in which thread.
// It is a valid replicated record on every release, so the numbering of the log stays gap-free,
// and it is still a test (same id, same expiry), so every filter keeps leaving it out.
func TestStub(m Message) Message {
	m.Body = ""
	m.Key, m.Box, m.Sig = "", "", ""
	m.FromTitle, m.FromModel = "", ""
	m.Verified, m.Private = "", false
	return m
}

// TestPurged reports whether m is a test message already reduced to its stub.
func TestPurged(m Message) bool { return m.Body == "" && m.Sig == "" && m.IsTest() }

// OptedIn reports whether the labels a session declared opt it in to the test lane.
func OptedIn(l Labels) bool { return l[TestLabel] == TestOn }
