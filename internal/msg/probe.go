package msg

import "strings"

// The two throwaway identities `aivoicebus join` probes the mesh with (see internal/cli/join.go).
// A node leaves them out of /v1/roster and /v1/who unless ?all=1 asks for them (gossip keeps
// them), and the client filters them from the display views again (a node that does not still lists them),
// so a probe never shows up as a live session. Decisions (who a message reaches, who holds a role)
// use the unfiltered roster: any session can name itself like a probe. The node-local "seen" entry
// the reader leaves behind expires with the rest of the roster (7 days).
const (
	ProbeSenderAgent = "join-probe"
	ProbeReaderAgent = "join-reader"
)

// IsProbe reports whether identity (host/agent) is one of the join probe identities.
func IsProbe(identity string) bool {
	_, agent, ok := strings.Cut(identity, "/")
	return ok && (agent == ProbeSenderAgent || agent == ProbeReaderAgent)
}
