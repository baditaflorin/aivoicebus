package outbox

import (
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// LastUnread is the message `aivoicebus reply` answers when no id is given: the ONE unread message
// in this session's inbox that a person could answer. Anyone who can write to this session can
// make theirs the newest, so "the newest" is no safe guess: with more than one candidate (or
// none) nothing is inferred, and the error names them so the sender can pick with --re. A node's
// notice, a withdrawn message and an expired one are not candidates. Nothing found is ErrNotFound;
// several is ErrUsage, since the command needs an id from the caller.
func LastUnread(c *client.Client, me string) (msg.Message, error) {
	if len(c.Labels) == 0 {
		c.Labels = identity.Labels("") // this read declares the session's labels: do not blank them
	}
	ms, err := c.Inbox(me, false)
	if err != nil {
		return msg.Message{}, err
	}
	return pickReply(ms, time.Now())
}

// pickReply chooses among an inbox listing (the part LastUnread tests without a node).
func pickReply(ms []msg.Message, now time.Time) (msg.Message, error) {
	var cands []idCand
	var only msg.Message
	for _, m := range ms {
		if m.Retracted != nil || m.From == msg.NoticeFrom || m.Expired(now) {
			continue
		}
		cands = append(cands, idCand{id: m.ID, from: m.From})
		only = m
	}
	switch len(cands) {
	case 0:
		return msg.Message{}, NotFoundf("no unread message to reply to (`aivoicebus inbox` lists them; `--re ID` replies to a specific one)")
	case 1:
		return only, nil
	}
	return msg.Message{}, Usagef("%d unread messages could be answered, so none is guessed (a reply goes to whoever wrote the one it answers): %s; run it again with --re ID to pick one", len(cands), listCandidates(cands))
}
