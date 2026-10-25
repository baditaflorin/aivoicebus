package msg

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Facts are what a machine has that agents used to ask each other about by task: does this Mac
// hold the release signing key, the deploy token, an ssh alias for a node, which fleet tools at
// which version. Each client works them out on its own machine (identity.Labels) and advertises
// them in two labels, so `who --has signing-key` answers at once. Booleans, alias names and
// versions only: never a file's contents, never a path. Like every label they are self-declared
// hints, not proof.
//
//	has  signing-key+deploy-token+fleet-guard-0.3.13+fleet-ctx-0.2.0, or none
//	ssh  the node ssh aliases that resolve on the machine, NAME_bastion advertised as NAME
//
// has is the marker: a session that advertises it has said what it has, so a fact it does not
// list (an ssh alias missing from its ssh label, or no ssh label at all) is a no. A session
// without it said nothing: an older client, facts turned off, or labels already full (facts are
// left out rather than push out a label routing depends on).
const (
	HasLabel = "has"
	SSHLabel = "ssh"
	FactNone = "none"
)

// FactLabels are the labels facts travel in.
var FactLabels = []string{HasLabel, SSHLabel}

// The facts a --has query names.
const (
	FactSigningKey  = "signing-key"
	FactDeployToken = "deploy-token"
	FactFleetGuard  = "fleet-guard"
	FactFleetCtx    = "fleet-ctx"
	FactSSH         = "ssh"
)

// FactTools are the tools whose installed version is a fact, in the order the has label lists them.
var FactTools = []string{FactFleetGuard, FactFleetCtx}

// FactNames is every fact a --has query may name, for errors and usage.
var FactNames = []string{FactSigningKey, FactDeployToken, FactFleetGuard + "[:VERSION]", FactFleetCtx + "[:VERSION]", FactSSH + ":ALIAS"}

var versionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,3}(-[0-9a-z.]{1,16})?$`)

// CleanVersion is v as a fact's version ("v0.3.13" and "fleet-ctx 0.2.0" give 0.3.13 and 0.2.0),
// or "" when it is not shaped like one: whatever a version file or command printed is advertised
// only when it can be nothing but a version.
func CleanVersion(v string) string {
	f := strings.Fields(strings.ToLower(v))
	if len(f) == 0 {
		return ""
	}
	s := strings.TrimPrefix(f[len(f)-1], "v")
	if len(s) > 24 || !versionRe.MatchString(s) {
		return ""
	}
	return s
}

var aliasRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// SSHWord is how the ssh alias a is written in the ssh label: lower case, with a trailing
// "_bastion" (the deploy scripts' convention for a node's alias) left off so four aliases fit in
// one label. "" for a name that cannot be an alias.
func SSHWord(a string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	if !aliasRe.MatchString(a) {
		return ""
	}
	if w := strings.TrimSuffix(a, "_bastion"); w != "" {
		return w
	}
	return a
}

// FactValue joins words into one label value in the order given, cut at a whole word within
// MaxLabelValue, so the first words (the ones asked about most) are the ones that survive.
// "none" for no words.
func FactValue(words []string) string {
	out := ""
	for _, w := range words {
		if w == "" || !capWordRe.MatchString(w) {
			continue
		}
		next := w
		if out != "" {
			next = out + "+" + w
		}
		if len(next) > MaxLabelValue {
			break
		}
		out = next
	}
	if out == "" {
		return FactNone
	}
	return out
}

// Facts is what one session's labels say its machine has. Known is false when it said nothing.
type Facts struct {
	Known       bool              `json:"known"`
	SigningKey  bool              `json:"signing_key"`
	DeployToken bool              `json:"deploy_token"`
	Tools       map[string]string `json:"tools,omitempty"` // fleet-guard -> 0.3.13
	SSH         []string          `json:"ssh,omitempty"`   // as advertised: 0own for 0own_bastion
}

// FactsOf reads the facts in a session's labels.
func FactsOf(l Labels) Facts {
	has, ok := l[HasLabel]
	if !ok || has == "" {
		return Facts{}
	}
	f := Facts{Known: true}
	for _, w := range strings.Split(has, "+") {
		switch t, v := hasWord(w); t {
		case FactSigningKey:
			f.SigningKey = true
		case FactDeployToken:
			f.DeployToken = true
		case "":
		default:
			if f.Tools == nil {
				f.Tools = map[string]string{}
			}
			f.Tools[t] = v
		}
	}
	for _, w := range strings.Split(l[SSHLabel], "+") {
		if w != "" && w != FactNone && SSHWord(w) == w && !slices.Contains(f.SSH, w) {
			f.SSH = append(f.SSH, w)
		}
	}
	slices.Sort(f.SSH)
	return f
}

// hasWord reads one word of the has label: the fact it names and, for a tool, its version.
// "" for a word that is no fact.
func hasWord(w string) (fact, version string) {
	if w == FactSigningKey || w == FactDeployToken {
		return w, ""
	}
	for _, t := range FactTools {
		if v, ok := strings.CutPrefix(w, t+"-"); ok && v != "" && CleanVersion(v) == v {
			return t, v
		}
	}
	return "", ""
}

// ValidFacts reports whether has and ssh read as facts, word by word: what a cache file says goes
// out as labels only if it could have been worked out.
func ValidFacts(has, ssh string) bool {
	if has == "" || len(has) > MaxLabelValue || len(ssh) > MaxLabelValue {
		return false
	}
	if has != FactNone {
		for _, w := range strings.Split(has, "+") {
			if t, _ := hasWord(w); t == "" {
				return false
			}
		}
	}
	if ssh != "" {
		for _, w := range strings.Split(ssh, "+") {
			if w == "" || SSHWord(w) != w {
				return false
			}
		}
	}
	return true
}

// Words is the facts as short readable words: signing-key deploy-token fleet-guard:0.3.13
// ssh:0own. Empty for a session that has nothing, or said nothing (see Known).
func (f Facts) Words() []string {
	var out []string
	if f.SigningKey {
		out = append(out, FactSigningKey)
	}
	if f.DeployToken {
		out = append(out, FactDeployToken)
	}
	for _, t := range FactTools {
		if v := f.Tools[t]; v != "" {
			out = append(out, t+":"+v)
		}
	}
	if len(f.SSH) > 0 {
		out = append(out, FactSSH+":"+strings.Join(f.SSH, ","))
	}
	return out
}

// FactQuery is one --has: a fact, and for a tool or an ssh alias a pattern (* allowed).
type FactQuery struct {
	Name    string
	Pattern string // a tool's version, an ssh alias (as SSHWord writes it); "" is any
	Text    string // as typed, lower-cased: what the answer is shown under
}

// ParseFactQuery reads a --has: signing-key, deploy-token, fleet-guard, fleet-guard:0.3.*,
// fleet-ctx:0.2.0, ssh:0own_bastion. Errors quote what was typed and list the facts.
func ParseFactQuery(s string) (FactQuery, error) {
	raw := s
	s = strings.ToLower(strings.TrimSpace(s))
	name, pat, hasPat := strings.Cut(s, ":")
	bad := func(why string) (FactQuery, error) {
		return FactQuery{}, fmt.Errorf("--has %q: %s; the facts are %s", raw, why, strings.Join(FactNames, ", "))
	}
	if name == "release-signing-key" || name == "release-signing.key" {
		name = FactSigningKey
	}
	switch name {
	case FactSigningKey, FactDeployToken:
		if hasPat {
			return bad("this fact is yes or no and takes no value")
		}
	case FactFleetGuard, FactFleetCtx:
		if hasPat && (pat == "" || len(pat) > 24 || strings.Trim(pat, "0123456789abcdefghijklmnopqrstuvwxyz.-*") != "") {
			return bad("a version may only use letters, digits, . - and *")
		}
	case FactSSH:
		if pat == "" {
			return bad("say which alias, such as ssh:node-a_bastion (or ssh:* for any)")
		}
		if w := SSHWord(strings.ReplaceAll(pat, "*", "x")); w == "" {
			return bad("an alias may only use letters, digits, . _ - and *")
		}
		if p := strings.TrimSuffix(pat, "_bastion"); p != "" {
			pat = p
		}
	default:
		return bad("not a fact this client knows")
	}
	return FactQuery{Name: name, Pattern: pat, Text: s}, nil
}

func (q FactQuery) String() string {
	switch {
	case q.Text != "":
		return q.Text
	case q.Pattern == "":
		return q.Name
	}
	return q.Name + ":" + q.Pattern
}

// Match is whether facts f answer q with yes, and the value that did (a version, an alias).
// Meaningless when !f.Known: the caller says "unknown" then, never "no".
func (q FactQuery) Match(f Facts) (bool, string) {
	switch q.Name {
	case FactSigningKey:
		return f.SigningKey, ""
	case FactDeployToken:
		return f.DeployToken, ""
	case FactSSH:
		for _, a := range f.SSH {
			if glob(q.Pattern, a) {
				return true, a
			}
		}
		return false, ""
	}
	v := f.Tools[q.Name]
	if v == "" || (q.Pattern != "" && !glob(q.Pattern, v)) {
		return false, v
	}
	return true, v
}
