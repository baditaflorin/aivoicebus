package msg

import (
	"fmt"
	"slices"
	"strings"
)

// What a session can do, read from its labels, so nobody has to declare the obvious: the
// repository it has checked out (repo:OWNER/NAME, from its origin remote), the branch, the tool,
// the model and the roles it claims, plus the words in its caps label (AIVOICEBUS_CAPS, and gpu
// where the machine has one). A sender asks for them with --needs and the session that has all
// of them gets the work. Like every label they are self-declared hints, never proof.

// NeedKeys are the capability kinds read from labels, as key:value.
var NeedKeys = []string{"repo", "branch", "tool", "model", "role"}

// MaxNeeds is how many capabilities one --needs may ask for.
const MaxNeeds = 8

// CapsOf is every capability of a session with identity me and labels l: the declared words
// first (sorted, as in the caps label), then key:value ones in NeedKeys order. repo is the
// origin's owner/name when the remote has one, else the repository's name.
func CapsOf(me string, l Labels) []string {
	var out []string
	for _, w := range strings.Split(l[CapsLabel], "+") {
		if w != "" {
			out = append(out, w)
		}
	}
	add := func(k, v string) {
		if v != "" {
			out = append(out, k+":"+v)
		}
	}
	repo := l["origin"]
	if repo == "" {
		repo = l["repo"]
	}
	add("repo", repo)
	add("branch", l["branch"])
	tool := l["tool"]
	if tool == "" {
		_, agent, _ := strings.Cut(me, "/")
		tool, _, _ = strings.Cut(agent, ".")
		tool = CleanValue(tool)
	}
	add("tool", tool)
	add("model", l[ModelLabel])
	for _, r := range RolesOf(l) {
		add("role", r)
	}
	return out
}

// ParseNeeds reads a --needs list: "repo:owner/name,gpu". Each is a capability word (as in
// AIVOICEBUS_CAPS) or key:value with a key from NeedKeys; a value may use * (branch:feature/*).
// Errors quote what was typed and say what is allowed.
func ParseNeeds(s string) ([]string, error) {
	var out []string
	for _, raw := range strings.Split(s, ",") {
		n := strings.ToLower(strings.TrimSpace(raw))
		if n == "" {
			continue
		}
		k, v, kv := strings.Cut(n, ":")
		switch {
		case !kv && !capWordRe.MatchString(n):
			return nil, fmt.Errorf("--needs %q: a capability is one word (letters, digits, . _ / -), such as gpu, or key:value with a key of %s", raw, strings.Join(NeedKeys, ", "))
		case kv && !slices.Contains(NeedKeys, k):
			return nil, fmt.Errorf("--needs %q: %q is not a capability kind; the kinds are %s (or a plain word such as gpu)", raw, k, strings.Join(NeedKeys, ", "))
		case kv && (v == "" || len(v) > MaxLabelValue || strings.Trim(v, "abcdefghijklmnopqrstuvwxyz0123456789._/+*-") != ""):
			return nil, fmt.Errorf("--needs %q: the value may only use letters, digits, . _ / + - and * (at most %d characters)", raw, MaxLabelValue)
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--needs names no capability: say which, such as --needs repo:owner/name,gpu")
	}
	if len(out) > MaxNeeds {
		return nil, fmt.Errorf("--needs asks for %d capabilities; at most %d", len(out), MaxNeeds)
	}
	return out, nil
}

// Lacks is the needs a session does not have, in the order asked (none: it has them all). A
// repo need without an owner (repo:go-common) matches the repository's name, so a checkout
// without an origin remote still counts.
func Lacks(me string, l Labels, needs []string) []string {
	caps := CapsOf(me, l)
	var out []string
	for _, n := range needs {
		if !hasNeed(caps, l, n) {
			out = append(out, n)
		}
	}
	return out
}

func hasNeed(caps []string, l Labels, need string) bool {
	k, v, kv := strings.Cut(need, ":")
	if !kv {
		return slices.Contains(caps, need)
	}
	if k == "repo" && !strings.Contains(v, "/") && l["repo"] != "" && glob(v, l["repo"]) {
		return true
	}
	for _, c := range caps {
		if ck, cv, ok := strings.Cut(c, ":"); ok && ck == k && glob(v, cv) {
			return true
		}
	}
	return false
}
