package cli

import (
	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// laneFor makes c read the test lane (msg/testlane.go). A session that did not opt in would get none
// (the nodes send test messages only to a session that declared the opt-in), so it is told how.
func laneFor(c *client.Client) error {
	if !identity.TestOptIn() {
		return outbox.Usagef("this reads the test lane, and this session has not opted in: run it with AIVOICEBUS_TEST=1 in the environment (a worker with `worker --test-lane`)")
	}
	c.Lane = true
	return nil
}
