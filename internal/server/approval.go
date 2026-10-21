package server

import (
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// approvalRequestOf is the approval request that a message answering re belongs to: the root of
// re's thread, or re itself, when it is an [approval needed] message. A reply to a reply in the
// thread counts: the worker takes a decision from anywhere in it. A request this node does not hold
// (not replicated here yet) is none: the CLI looks on every node, and the worker checks the
// signature itself.
func approvalRequestOf(s *store.Store, re string) (msg.Message, bool) {
	if re == "" {
		return msg.Message{}, false
	}
	var target, root msg.Message
	for _, m := range s.Thread(re) {
		if m.ID == re {
			target = m
		}
		if m.ID == m.Thread {
			root = m
		}
	}
	switch {
	case msg.IsApprovalRequest(root.Body):
		return root, true
	case msg.IsApprovalRequest(target.Body):
		return target, true
	}
	return msg.Message{}, false
}
