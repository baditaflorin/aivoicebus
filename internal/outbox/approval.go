package outbox

import (
	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// approvalRequestOf is the approval request a reply to orig belongs to: orig itself, or the root of
// its thread. The thread is read only when orig is not the request, and only for a body that reads as
// a decision, so an ordinary reply costs nothing more.
func approvalRequestOf(c *client.Client, orig msg.Message) (msg.Message, bool) {
	if msg.IsApprovalRequest(orig.Body) {
		return orig, true
	}
	if orig.Thread == "" || orig.Thread == orig.ID {
		return msg.Message{}, false
	}
	ms, err := c.Thread(orig.ID)
	if err != nil {
		return msg.Message{}, false // the node checks it too
	}
	for _, m := range ms {
		if m.ID == orig.Thread && msg.IsApprovalRequest(m.Body) {
			return m, true
		}
	}
	return msg.Message{}, false
}
