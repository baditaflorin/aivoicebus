package msg

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Offers. A task sent to an audience (a machine, a tool on it, a selector, all) used to be shown to
// every session there at once, and each one took it that another would do it: a week of the hub's
// messages had a rollout task shown to 31 sessions and claimed after 19 hours. The nodes now offer
// such a task to one session at a time (store/offer.go): a session that is listening first, then an
// interactive one (titled, seen lately), then any live one. That session takes it with a claim, as
// before, or passes it on at once with a decline; one that does neither within OfferWindow is passed
// over, and the next is offered it. A task nobody took in EscalateAfter is told to the person.
//
// Nothing here is a field of the wire format. A decline is a progress message whose body starts with
// DeclineTag (every node since ProgressSince stores and replicates one unchanged). What a reader is
// told about its offer is Message.Offer, response only like Retracted. The message that tells the
// person has an id derived from the task's (EscalationID), so the many sessions that may notice at
// once make one message: a node refuses a second message under an id it holds.
const (
	// OfferWindow is how long one session is offered a task before the next one is.
	OfferWindow = 10 * time.Minute
	// OfferActiveWithin is how recently a titled session must have been seen to be offered a task
	// before the untitled ones (hourly and headless runs declare no title).
	OfferActiveWithin = 15 * time.Minute
	// OfferLiveWithin is how recently any session must have been seen to be offered a task at all:
	// the limit a send to one session also uses for "live".
	OfferLiveWithin = 30 * time.Minute
	// EscalateAfter is how long a task may go untaken before the person is told.
	EscalateAfter = 30 * time.Minute

	// FeatureOffers is what a node advertises when it offers a task one session at a time.
	FeatureOffers = "offers"

	// DeclineTag starts the body of a decline: "not this session", sent to the task's sender.
	DeclineTag = "[decline]"
)

// What came of one session's offer (OfferStep.Outcome).
const (
	OfferOpen     = "open"     // it is offered to that session now
	OfferPassed   = "passed"   // its window ran out, or another took it
	OfferDeclined = "declined" // it said not this session
	OfferClaimed  = "claimed"  // it took it
)

// Offered reports whether the nodes offer task m one session at a time: a task to an audience,
// not to one session by name, not one for the person, not a test, and not one with a done-when: the
// condition is checked by whichever session starts on the machine (msg/wants.go), so every session has to
// be shown it, and once the condition holds it costs none of them anything.
func Offered(m Message) bool {
	return m.Kind == KindTask && IsBulletin(m.To) && !OneSession(m.To) && !PersonOnly(m) && !m.IsTest() && WantsOf(m).DoneWhen == "" // PersonOnly: <machine>/human too
}

// Offer is what a node tells the session it offers a task to. Response only, like Retracted: never
// stored, replicated or signed, and cleared on a message that arrives with it set.
type Offer struct {
	Since    time.Time `json:"since"`              // when the offer to this session began
	Until    time.Time `json:"until"`              // when it goes to the next session unless taken or declined
	Before   int       `json:"before"`             // sessions offered it before this one
	Left     int       `json:"left"`               // live sessions it can still go to after this one
	Escalate bool      `json:"escalate,omitempty"` // untaken for EscalateAfter, and the person not told yet
}

// OfferStep is one session's offer of a task, as receipts report it to the sender.
type OfferStep struct {
	By      string    `json:"by"`
	At      time.Time `json:"at"`             // when it was offered it (or read it)
	Until   time.Time `json:"until"`          // when that offer ended or ends
	Outcome string    `json:"outcome"`        // OfferOpen, OfferPassed, OfferDeclined or OfferClaimed
	Note    string    `json:"note,omitempty"` // a decline's reason, as its sender wrote it
}

// OfferTier ranks session e for being offered task m at now, best first: 0 listening (it reads the
// task within a second), 1 titled and seen within OfferActiveWithin (a person is working in it), 2
// seen within OfferLiveWithin. -1: it is not offered the task.
func OfferTier(e RosterEntry, m Message, now time.Time) int {
	switch {
	case e.Accepts(Message{ID: m.ID, Kind: KindTask, From: m.From, Thread: m.Thread}):
		return 0
	case now.Sub(e.Seen) > OfferLiveWithin:
		return -1
	case e.Title != "" && now.Sub(e.Seen) <= OfferActiveWithin:
		return 1
	}
	return 2
}

// DeclineBody is the body of a decline with an optional reason.
func DeclineBody(note string) string {
	if note = strings.TrimSpace(note); note == "" {
		return DeclineTag
	}
	return DeclineTag + " " + note
}

// IsDecline reports whether m declines the task it answers (Re): a progress message whose first line
// starts with DeclineTag.
func IsDecline(m Message) bool {
	_, ok := declineNote(m)
	return ok
}

// DeclineNote is the reason a decline gives ("" for none, or for a message that is no decline).
func DeclineNote(m Message) string {
	n, _ := declineNote(m)
	return n
}

func declineNote(m Message) (string, bool) {
	if m.Kind != KindProgress || m.Re == "" {
		return "", false
	}
	rest, ok := strings.CutPrefix(strings.TrimSpace(m.Body), DeclineTag)
	if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\n') {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// EscalationID is the id of the message that tells the person nobody took task: the same for every
// client, so two sessions that notice at once send one message (a node refuses a second under an id it
// holds). Never the shape of a test id or a successor mark.
func EscalationID(task string) string {
	for i := 0; ; i++ {
		h := sha256.Sum256([]byte("aivoicebus escalation " + task + " " + strconv.Itoa(i)))
		if id := hex.EncodeToString(h[:5]); !IsTestID(id) && !IsSuccessorID(id) {
			return id
		}
	}
}

// IsEscalation reports whether m is the message that told the person nobody took the task it answers.
func IsEscalation(m Message) bool {
	return m.Re != "" && IsHuman(m.To) && m.ID == EscalationID(m.Re)
}

// EscalationTo is the person told about task m: the one on the machine it was sent to, or, for an
// audience across machines (all, */tool, a selector), the one on the sender's machine.
func EscalationTo(m Message) string {
	h := HostOf(m.To)
	if m.To == "all" || IsSelector(m.To) || h == "*" || h == "" {
		h = HostOf(m.From)
	}
	return h + "/human"
}
