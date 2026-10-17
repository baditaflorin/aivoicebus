package msg

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
)

// An approval of a write-mode worker task is a reply whose body is the word (approve or deny) and
// a line with the approver key's signature over what is decided: the request, the word, and the
// worker it is for. A signature by the machine key alone is not one (see config.CreateApprover).

const approvalTag = "[approver-sig v1 "

// ApprovalStatement is what an approver key signs.
func ApprovalStatement(request, word, worker string) []byte {
	return []byte("aivoicebus-approval v1\n" + request + "\n" + word + "\n" + worker)
}

// ApprovalBody is the reply that carries a signed decision.
func ApprovalBody(word string, sig []byte) string {
	return word + "\n" + approvalTag + base64.StdEncoding.EncodeToString(sig) + "]"
}

// CheckApproval reads a reply as a signed decision on request for worker by key: the word, and
// whether the signature holds.
func CheckApproval(body, request, worker string, key ed25519.PublicKey) (string, bool) {
	word, rest, _ := strings.Cut(strings.TrimSpace(body), "\n")
	word = strings.ToLower(strings.TrimSpace(word))
	b64, ok := strings.CutPrefix(strings.TrimSpace(rest), approvalTag)
	if !ok || (word != "approve" && word != "deny") || len(key) != ed25519.PublicKeySize {
		return word, false
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(b64, "]"))
	if err != nil {
		return word, false
	}
	return word, ed25519.Verify(key, ApprovalStatement(request, word, worker), sig)
}
