package msg

import (
	"regexp"
	"strings"
)

// HoldReason says why a message should not reach an agent, or "" when it may: a signature that
// does not verify or a machine key that changed, or text shaped like instructions to an agent (an
// injection: "ignore previous instructions", "run aivoicebus approve", a pipe into a shell...).
// The person still sees it, and can release it. False alarms are the price: the list is short and
// matches only what peer text has no business saying to an agent.
func HoldReason(m Message) string {
	switch m.Verified {
	case VerifiedBad:
		return "its signature does not verify (altered, or not from who it says)"
	case VerifiedChanged:
		return "it is signed by a key other than the one pinned for " + HostOf(m.From)
	}
	if m.Retracted != nil {
		return ""
	}
	body := strings.ToLower(m.Body)
	for _, p := range injections {
		if p.re.MatchString(body) {
			return "it reads like instructions to an agent (" + p.what + ")"
		}
	}
	return ""
}

var injections = []struct {
	what string
	re   *regexp.Regexp
}{
	{"ignore previous instructions", regexp.MustCompile(`\b(ignore|disregard|forget)\b[^.\n]{0,30}\b(previous|prior|above|earlier|all|your|system)\b[^.\n]{0,20}\b(instructions|prompts?|rules|guidelines)\b`)},
	{"a new role or system prompt", regexp.MustCompile(`\b(you are now|new instructions:|system prompt:|<\s*system\s*>)`)},
	{"the user's own aivoicebus commands", regexp.MustCompile(`\baivoicebus\s+(approve|deny|approver|resume|unhold|worker\s+allow|incident\b[^\n]*\s--apply)\b`)},
	{"a download piped into a shell", regexp.MustCompile(`\b(curl|wget)\b[^|\n]{0,200}\|\s*(sudo\s+)?(ba|z)?sh\b`)},
	{"deleting a home or root directory", regexp.MustCompile(`\brm\s+-[a-z]*r[a-z]*f?\s+(/|~|\$home)(\s|$)`)},
	{"skipping the agent's permissions", regexp.MustCompile(`dangerously-skip-permissions|dangerously-bypass|bypasspermissions`)},
}
