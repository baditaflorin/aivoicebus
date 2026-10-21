package msg

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
)

// A reply in an approval thread is stored only when it can be a person's decision. Only a signature
// by the approver key decides (CheckApproval, which the worker and the token install run themselves);
// the word alone never does, and an agent that writes it still believes it approved. So the CLI and
// the nodes refuse the replies that cannot count, and say who can: the person, in a terminal. This is
// a guard on top of the decision, never the decision: a request names the approver's PUBLIC key on a
// line of its own so a node can tell a signature that verifies from one that does not, and a request
// without the line (a worker older than this release) is judged on its shape alone.

// ApproverKeyPrefix starts the request line that names the approver key's public half.
const ApproverKeyPrefix = "approver-key: "

// ApproverKeyLine is the request line for key.
func ApproverKeyLine(key ed25519.PublicKey) string {
	return ApproverKeyPrefix + base64.StdEncoding.EncodeToString(key)
}

// approverKeyIn reads the key a request names ("" key and false when it names none).
func approverKeyIn(body string) (ed25519.PublicKey, bool) {
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(line, ApproverKeyPrefix)
		if !ok {
			continue
		}
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, false
		}
		return k, true
	}
	return nil, false
}

// IsApprovalRequest reports whether body is a request for the person's approval.
func IsApprovalRequest(body string) bool { return strings.HasPrefix(body, ApprovalTag) }

// DecisionShaped reports whether a reply reads as a decision: its first word is approve or deny
// (or approved, denied), or it carries an approver signature line.
func DecisionShaped(body string) bool {
	body = strings.TrimSpace(body)
	switch firstWord(body) {
	case "approve", "approved", "deny", "denied":
		return true
	}
	return strings.Contains(body, approvalSigOpen)
}

// firstWord is the first word of body's first line, lowercased, without trailing punctuation.
func firstWord(body string) string {
	line, _, _ := strings.Cut(body, "\n")
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimRight(f[0], ".,:;!)"))
}

// ApprovalRefusal is what a refused reply to an approval request says: why, and who decides.
func ApprovalRefusal(requestID, why string) string {
	return fmt.Sprintf("refused: %s. Only the person's signed decision answers an approval request, and the word in a message never counts. Nothing was sent: tell your user it is waiting; they run, in their own terminal: `aivoicebus approve %s` (or `aivoicebus deny %s`)", why, requestID, requestID)
}

// DecisionProblem says why reply, sent into the thread of the approval request request, may not be
// stored ("" when it may): it is not decision-shaped, or it is one that can hold: signed, and, when
// the request names the approver key, signed with it for this request and word.
func DecisionProblem(request Message, reply string) string {
	if !IsApprovalRequest(request.Body) || !DecisionShaped(reply) {
		return ""
	}
	reply = strings.TrimSpace(reply)
	word, rest, _ := strings.Cut(reply, "\n")
	word = strings.ToLower(strings.TrimSpace(word))
	switch {
	case !strings.HasPrefix(strings.TrimSpace(rest), approvalTag):
		return "this reply says " + oneWord(word) + " without an approver signature"
	case word != "approve" && word != "deny":
		return "this reply carries an approver signature, but its first word is " + oneWord(word) + ", not approve or deny"
	}
	key, ok := approverKeyIn(request.Body)
	if !ok {
		return "" // the request names no key (an older worker): nothing here can say it does not verify
	}
	if _, ok := CheckApproval(reply, request.ID, request.From, key); !ok {
		return "its approver signature does not verify for this request with the key the request names"
	}
	return ""
}

// oneWord is the first word of s, shortened, for a refusal's text (never more of a peer's words).
func oneWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return "something"
	}
	w := []rune(f[0])
	if len(w) > 12 {
		w = w[:12]
	}
	return `"` + strings.TrimRight(string(w), ".,:;!)") + `"`
}
