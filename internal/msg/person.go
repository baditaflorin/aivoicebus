package msg

import "strings"

// Some messages are for the person on a machine, never for an agent there: a [token hand] task
// (it replaces the machine's credential), an [approval needed] request, an [ask-human] question,
// and any task the sender marks [for the person] (task --person). An agent may see one, and tell its
// user what to run, but never takes it: a claim of it from an agent's session is refused (see
// tasks.Claim), and standby skips it.

// PersonTag starts a task that its sender meant for the person on the machine (task --person).
const PersonTag = "[for the person]"

// ApprovalTag starts a request for the person's approval (a write-mode worker's task, a token install).
const ApprovalTag = "[approval needed]"

// personTags are the first-line tags that mark a message as the person's.
var personTags = []string{TokenHandTag, ApprovalTag, AskPrefix, PersonTag}

// PersonOnly reports whether m is for the person on its machine, not an agent: it is addressed to
// <machine>/human (HumanOf), or its first line starts with one of the person's tags and it was not
// sent to one session by name (a sender who names one session chose who takes it).
func PersonOnly(m Message) bool {
	if IsHuman(m.To) {
		return true
	}
	return PersonTagged(m) && !OneSession(m.To)
}

// PersonTagged reports whether m's first line starts with one of the person's tags, whoever it is
// addressed to: the tag says whom it is meant for even when the sender chose one session to carry it.
func PersonTagged(m Message) bool {
	line, _, _ := strings.Cut(strings.TrimSpace(m.Body), "\n")
	for _, t := range personTags {
		if strings.HasPrefix(line, strings.TrimSpace(t)) {
			return true
		}
	}
	return false
}

// IsHuman reports whether to is a machine's person (<machine>/human), whom no agent reads as.
func IsHuman(to string) bool {
	h, a, ok := strings.Cut(strings.ToLower(to), "/")
	return ok && h != "" && h != "*" && a == "human"
}

// OneSession reports whether to names a single session: host/tool.session, or a long-lived name
// (host/build1) that is no tool's. A machine, a tool (host/claude), a wildcard, a selector or all
// reaches whoever is there.
func OneSession(to string) bool {
	h, a, ok := strings.Cut(to, "/")
	if !ok || h == "*" || h == "" || a == "" || a == "*" || IsSelector(to) || IsHuman(to) {
		return false
	}
	if strings.Contains(a, ".") {
		return true
	}
	return !KnownTool(a)
}

// knownTools are the agent tools whose name alone addresses every session of that tool on a
// machine (host/claude). It mirrors the identity package's table, which msg cannot import.
var knownTools = []string{"claude", "codex", "gemini", "grok", "cursor-agent", "opencode", "aider", "goose", "agent", "mcp", "worker"}

// KnownTool reports whether name is a tool, not one session's name.
func KnownTool(name string) bool {
	for _, t := range knownTools {
		if t == name {
			return true
		}
	}
	return false
}

// PersonStep is what the person runs for m, which is PersonOnly, in their own terminal.
func PersonStep(m Message) string {
	line, _, _ := strings.Cut(strings.TrimSpace(m.Body), "\n")
	switch {
	case strings.HasPrefix(line, TokenHandTag):
		return "aivoicebus token install " + m.ID
	case strings.HasPrefix(line, ApprovalTag):
		return "aivoicebus approve " + m.ID + "  (or: aivoicebus deny " + m.ID + ")"
	case strings.HasPrefix(line, strings.TrimSpace(AskPrefix)):
		return "aivoicebus answer " + m.ID
	case m.Kind == KindTask:
		return "aivoicebus claim " + m.ID + "  (then do it, and answer with: aivoicebus reply --re " + m.ID + " ...)"
	}
	return "aivoicebus thread " + m.ID
}
