// Package identity says who this process is on the board: host/agent, for
// example laptop-a/claude.a1b2.
package identity

import (
	"os"
	"strings"
)

var osGetenv = os.Getenv

// Host is this machine's name: lowercase, without ".local", safe inside an
// identity (no spaces or slashes). Servers use it as their default node id.
func Host() string {
	h, _ := os.Hostname()
	h = strings.ToLower(strings.TrimSuffix(h, ".local"))
	return strings.NewReplacer(" ", "-", "/", "-").Replace(h)
}

// Me is host/agent, e.g. laptop-a/claude-work. The host part is Label().
// AIVOICEBUS_ME overrides the whole identity; AIVOICEBUS_AGENT sets the agent
// part. Without either, the agent is the tool this process runs under, found
// from its parent processes and environment (see Detect): claude.a1b2,
// codex.q1w2... and "agent" when the tool is not known.
func Me() string {
	if v := os.Getenv("AIVOICEBUS_ME"); v != "" {
		return strings.ToLower(v)
	}
	agent := os.Getenv("AIVOICEBUS_AGENT")
	switch {
	case agent == "":
		agent = Explain().Agent() // the detection, or a registered AIVOICEBUS_NAME
	case os.Getenv("AIVOICEBUS_AGENT_SRC") == "hook":
		// exported by the session-start hook: it may belong to an outer tool (see Detect)
		agent = Explain().Agent()
	}
	return Label() + "/" + strings.ToLower(agent)
}
