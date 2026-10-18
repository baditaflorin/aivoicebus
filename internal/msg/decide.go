package msg

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"regexp"
	"strings"
)

// Phone decisions on a write-mode worker's approval request. The worker makes a one-time code,
// puts only its SHA-256 in the request ("phone-check: sha256:<hex>") and sends the code itself only
// in the notification to the person's phone (ntfy action buttons). The button POSTs the code to a
// node's /v1/decide, which checks it against the request and records "phone approve|deny <code>"
// in the request's thread; the worker checks the code again. The approver's machine never sees the
// code, so an agent there cannot decide.

// FeatureDecide is the node feature for /v1/decide.
const FeatureDecide = "decide"

// PhoneIdentity is who a node records a phone decision as.
const PhoneIdentity = "phone/ntfy"

var (
	phoneCheck = regexp.MustCompile(`(?m)^phone-check: sha256:([0-9a-f]{64})$`)
	phoneCode  = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// PhoneCheckLine is the line a request carries for code.
func PhoneCheckLine(code string) string {
	sum := sha256.Sum256([]byte(code))
	return "phone-check: sha256:" + hex.EncodeToString(sum[:])
}

// PhoneCodeMatches reports whether code is the one whose hash request body carries.
func PhoneCodeMatches(body, code string) bool {
	if !phoneCode.MatchString(code) {
		return false
	}
	g := phoneCheck.FindStringSubmatch(body)
	if g == nil {
		return false
	}
	sum := sha256.Sum256([]byte(code))
	want, _ := hex.DecodeString(g[1])
	return subtle.ConstantTimeCompare(sum[:], want) == 1
}

// PhoneDecisionBody is what the node records.
func PhoneDecisionBody(say, code string) string { return "phone " + say + " " + code }

// ParsePhoneDecision reads a recorded phone decision: the word and the code.
func ParsePhoneDecision(body string) (say, code string, ok bool) {
	f := strings.Fields(body)
	if len(f) != 3 || f[0] != "phone" || !ValidChoice(f[1]) || !phoneCode.MatchString(f[2]) {
		return "", "", false
	}
	return f[1], f[2], true
}
