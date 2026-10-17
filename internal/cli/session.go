package cli

import (
	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
)

// sessionClient is the client for a command that reads this session's mail: it declares the
// session's labels with every read, so what is addressed by selector (@repo=..., @can=...,
// @room=...) reaches it, in a wait as much as in an inbox listing.
func sessionClient() (*client.Client, error) {
	c, err := client.FromEnv()
	if err == nil {
		c.Labels = identity.Labels("")
	}
	return c, err
}
