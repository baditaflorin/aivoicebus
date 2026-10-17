package msg

import (
	"errors"
	"unicode/utf8"
)

// What a record must satisfy to be taken from a peer, or replayed from a node's own log.
//
// This is deliberately not ValidateShape and ValidateOptions. Those are the policy for what a
// client may send today, and a release may tighten them. A record already in the mesh was
// accepted under the rules of whichever release wrote it, and refusing it later would lose real
// history and leave two nodes holding different records at the same (origin, seq). So the limits
// below are frozen at what the first release's checks allow, which only catches what no client writes.
// Never
// tighten them. To allow a new priority, kind or field, widen them in a release that reaches
// every node before any client can send it: an older node would otherwise quarantine the record.
const (
	replMaxIdentity = 200      // origin, from, to, by
	replMaxRef      = 64       // id, thread, re, ack id
	replMaxBody     = 32 << 10 // a message body
	replMaxToken    = 32       // an ack's via
	replMaxReach    = 100000
)

// ErrReplicatedOrigin is what ValidateReplicated says of a record whose origin is unusable. Such
// a record cannot be kept, not even as a placeholder: the origin is a key of the version vector,
// which is sent in the query of every sync request, so a hostile one would break replication.
var ErrReplicatedOrigin = errors.New("origin is empty, too long or contains control characters")

// ValidateReplicated reports why a record taken from a peer (or read back from the log) must not
// be stored as it is, or nil. The reasons name the field and never repeat its content.
func (r Record) ValidateReplicated() error {
	if !replIdentity(r.Origin) {
		return ErrReplicatedOrigin
	}
	switch {
	case (r.Msg == nil) == (r.Ack == nil):
		return errors.New("a record holds exactly one of msg and ack")
	case r.Msg != nil:
		return r.Msg.validateReplicated()
	}
	return r.Ack.validateReplicated()
}

func (m *Message) validateReplicated() error {
	switch {
	case !replRef(m.ID, true):
		return errors.New("message id is empty, too long or contains control characters")
	case !replIdentity(m.From):
		return errors.New("from is empty, too long or contains control characters")
	case !replIdentity(m.To):
		return errors.New("to is empty, too long or contains control characters")
	case !replRef(m.Thread, false):
		return errors.New("thread is too long or contains control characters")
	case !replRef(m.Re, false):
		return errors.New("re is too long or contains control characters")
	case len(m.Body) > replMaxBody:
		return errors.New("body is too long")
	case len(m.FromTitle) > replMaxTitle || !utf8.ValidString(m.FromTitle):
		return errors.New("from_title is too long or not text")
	case len(m.FromModel) > replMaxModel || !utf8.ValidString(m.FromModel):
		return errors.New("from_model is too long or not text")
	case len(m.Key) > 64 || len(m.Box) > 64 || len(m.Sig) > 128:
		return errors.New("key, box or sig is too long")
	}
	switch m.Prio {
	case "", PrioFYI, PrioAsk, PrioUrgent:
	default:
		return errors.New("priority is not one of fyi, ask, urgent")
	}
	switch m.Kind {
	case "", KindQuestion, KindAnswer, KindTask, KindClaim, KindEnd, KindLease:
	default:
		return errors.New("kind is not one of question, answer, task, claim, end, lease")
	}
	if m.Reach != nil && (*m.Reach < 0 || *m.Reach > replMaxReach) {
		return errors.New("reach is out of range")
	}
	if m.Depth < 0 || m.Depth > MaxDepthField {
		return errors.New("depth is out of range")
	}
	if m.MaxTurns < 0 || m.MaxTurns > MaxTurnsLimit {
		return errors.New("max_turns is out of range")
	}
	return nil
}

// A retraction (see retract.go) is an ack like any other here: its id is at most 23 characters and
// it has no via, so it passes in every release; a node that predates retractions stores it without applying it.
func (a *Ack) validateReplicated() error {
	switch {
	case !replRef(a.ID, true):
		return errors.New("ack id is empty, too long or contains control characters")
	case !replIdentity(a.By):
		return errors.New("ack by is empty, too long or contains control characters")
	case len(a.Via) > replMaxToken || hasControl(a.Via): // a short label, not a closed list: new ways of delivery will come
		return errors.New("ack via is too long or contains control characters")
	}
	return nil
}

func replIdentity(s string) bool { return s != "" && len(s) <= replMaxIdentity && !hasControl(s) }

func replRef(s string, required bool) bool {
	return (s != "" || !required) && len(s) <= replMaxRef && !hasControl(s)
}
