package secrets

import (
	"regexp"
	"strings"
)

// Authority shapes are text that is not a secret to read but a way to act, and messages are kept in
// clear text on every node and shown in the dashboard: an approver's signature (the one thing that
// lets a write-mode worker run a task, a token install go on) and a pipe link with its key (whoever
// reads the board can open the file while it lives). Find does not look for them: an approval reply
// legitimately carries a signature, and `send --file` legitimately carries a link. Authority looks
// for them where they do not belong.

// What Authority says it found, for Refusal.
const (
	KindApproverSignature = "approver signature"
	KindPipeLink          = "pipe link with its key"
)

// attachPrefix starts the one body line `send --file`, `token hand` and the log links write their
// link on (attach.linePrefix; a test in package outbox keeps the two equal). Such a link is the
// sanctioned carrier: its key is sealed for one machine, or the file burns on the first read, and
// readers are shown a summary of the line instead of the line.
const attachPrefix = "[attach]"

var (
	// msg.ApprovalBody: the word, then "[approver-sig v1 " and the base64 of an ed25519 signature.
	approverSig = regexp.MustCompile(`\[approver-sig v[0-9]+ [A-Za-z0-9+/_=-]{16,}\]`)
	// https://HOST/#ID~KEY, as the pipe (and the tail) issue them: a short id, then 64 hex characters.
	// A shell paste puts a backslash before # and ~ (token.cleanLink), so a link that still has them
	// is the same link.
	pipeLink = regexp.MustCompile(`https?://[^\s/#?]+/\\?#[A-Za-z0-9_-]{4,64}\\?~[0-9A-Fa-f]{64}`)
)

// Authority reports the first approver signature or pipe link with its key in body, outside the
// [attach] lines that carry a file's link. decision says the body is a reply in an approval thread
// that may carry a signature (see msg.DecisionProblem), so a signature is then not a finding.
func Authority(body string, decision bool) (Finding, bool) {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), attachPrefix) {
			continue
		}
		if !decision && approverSig.MatchString(line) {
			return Finding{Kind: KindApproverSignature, Strong: true}, true
		}
		if pipeLink.MatchString(line) {
			return Finding{Kind: KindPipeLink, Strong: true}, true
		}
	}
	return Finding{}, false
}
