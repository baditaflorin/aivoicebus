// Package secrets recognises text that looks like a credential, so a message that
// carries one can be stopped before it is stored. Messages are kept in clear text
// on every node and shown in the dashboard; a secret in one is a secret on five machines
// forever. Agents are told never to send secrets, and sometimes do: this is the
// backstop, at the CLI that sends and at the node that stores.
//
// The patterns are two kinds. Strong ones are almost never anything else (a private
// key block, a GitHub or AWS token, a bearer credential): a send that contains one is
// refused. Weak ones are shapes that real text sometimes has (password: something
// long, a long mixed-case token): a send that contains one only draws a warning.
//
// Canaries are the third kind: exact secrets (a node's own tokens, the files in a machine's
// config directory) that are refused wherever they appear, whatever they look like.
package secrets

import (
	"regexp"
	"strings"
)

// Finding is one thing that looks like a credential.
type Finding struct {
	Kind   string // "GitHub token", "private key", ...
	Strong bool
}

type rule struct {
	kind   string
	re     *regexp.Regexp
	strong bool
	// accept, when set, decides whether a match really is a credential; a match it rejects
	// is only a weak finding (a warning), so documentation and prose are not refused.
	accept func(match string) bool
	// genuine, when set, drops a match it rejects altogether, with no warning: it is ordinary
	// text that happens to have the shape (a kebab-case slug, a dummy password in a docs URL).
	genuine func(match string) bool
}

// Boundaries. A token must start at the beginning of the text or after a character that is not
// a letter or a digit (edge), unless its prefix is specific enough not to need one. Underscore
// counts as a start (x_ghp_..., TOKEN_AKIA...), which \b did not allow, and a longer word does
// not ("task-force-..." is not an "sk-" key). A token of one fixed length must also end there
// (end, endURL): ASIA plus 16 capitals followed by more capitals is a word, not an AWS key. A
// token of no fixed length takes the whole run of its characters, so it needs no end.
const (
	edge   = `(?:^|[^A-Za-z0-9])`
	end    = `(?:[^A-Za-z0-9]|$)`
	endURL = `(?:[^A-Za-z0-9_-]|$)` // for tokens whose own characters include _ and -
)

var rules = []rule{
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----`), true, nil, nil},
	// Prefixes this specific are a credential wherever they start, even glued to a letter or digit.
	{"GitHub token", regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`), true, nil, nil},
	{"GitHub token", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), true, nil, nil},
	{"GitLab token", regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`), true, nil, upperAfter("glpat-")},
	{"Tailscale key", regexp.MustCompile(`tskey-[a-z]{3,10}-[A-Za-z0-9_-]{16,}`), true, nil, tailscaleKey},
	// The rest are short or common as part of other text, so they need a clean start.
	{"AWS access key", regexp.MustCompile(edge + `(?:AKIA|ASIA)[0-9A-Z]{16}` + end), true, nil, nil},
	{"Slack token", regexp.MustCompile(edge + `xox[baprs]-[A-Za-z0-9-]{10,}`), true, nil, nil},
	{"Slack token", regexp.MustCompile(edge + `xapp-[0-9]-[A-Za-z0-9]{8,}-[0-9]{8,}-[A-Za-z0-9]{20,}`), true, nil, nil},
	{"Stripe key", regexp.MustCompile(edge + `(?:rk|sk)_(?:live|test)_[A-Za-z0-9]{24,}`), true, nil, upperAfter("_")},
	{"API key", regexp.MustCompile(edge + `sk[-_](?:live_|test_|ant-|proj-)?[A-Za-z0-9_-]{20,}`), true, nil, upperAfter("sk")},
	{"Google API key", regexp.MustCompile(edge + `AIza[A-Za-z0-9_-]{35}` + endURL), true, nil, upperAfter("AIza")},
	{"npm token", regexp.MustCompile(edge + `npm_[A-Za-z0-9]{36,}`), true, nil, upperAfter("npm_")},
	{"Hugging Face token", regexp.MustCompile(edge + `hf_[A-Za-z0-9]{30,}`), true, nil, upperAfter("hf_")},
	{"Shopify token", regexp.MustCompile(edge + `shp(?:at|ca|pa|ss)_[A-Fa-f0-9]{32}` + end), true, nil, nil},
	{"DigitalOcean token", regexp.MustCompile(edge + `do[por]_v1_[a-f0-9]{64}` + end), true, nil, nil},
	{"SendGrid key", regexp.MustCompile(edge + `SG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}` + endURL), true, nil, nil},
	{"JWT", regexp.MustCompile(edge + `eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), true, nil, nil},
	{"service API key", regexp.MustCompile(edge + `fb_[A-Za-z0-9]{16,}`), true, nil, nil},
	{"bootstrap link", regexp.MustCompile(`/bootstrap/[0-9A-Za-z]{24,}`), true, nil, nil},
	// scheme://user:password@host. The password may not hold ? # / or a space, so a port followed
	// by a query (http://localhost:3000?email=bob@example.com) is not read as a password.
	{"connection string with a password", regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s:/@?#]*:[^\s/@?#]{3,}@[A-Za-z0-9\[]`), true, nil, realPassword},
	{"bearer credential", regexp.MustCompile(`(?i)` + edge + `(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{16,}`), true, looksGenerated, nil},
	{"password or token value", regexp.MustCompile(`(?i)\b[a-z0-9_-]*(?:token|secret|passw(?:or)?d|api[_-]?key|apikey)\s*[:=]\s*["']?[^\s"',;]{8,}`), false, nil, nil},
}

// upperAfter is a genuine test for a token family whose real tokens are mixed case: the part of
// the match after prefix has a capital. A real token that long is never all lowercase, while a
// kebab-case or snake_case phrase that starts the same way (glpat-rotation-runbook-for-all-teams,
// my_sk_this_is_a_name) is prose.
func upperAfter(prefix string) func(string) bool {
	return func(match string) bool {
		_, body, _ := strings.Cut(match, prefix)
		return strings.ContainsAny(body, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	}
}

// tailscaleKey: a real key is mixed case and has digits (tskey-auth-k8s4zr3CNTRL-...).
func tailscaleKey(match string) bool {
	return upperAfter("tskey-")(match) && strings.ContainsAny(match, "0123456789")
}

// dummyPasswords are what documentation, tests and throwaway local services put in a connection
// string. A password that is exactly one of them (any case) is not a credential, so talking about
// postgres://user:password@localhost/db is not refused. A password that merely contains one is.
var dummyPasswords = map[string]bool{
	"password": true, "passwd": true, "pass": true, "pwd": true, "secret": true, "changeme": true,
	"example": true, "test": true, "testing": true, "admin": true, "root": true, "user": true,
	"username": true, "guest": true, "postgres": true, "postgresql": true, "mysql": true,
	"redis": true, "mongo": true, "mongodb": true, "rabbit": true, "rabbitmq": true, "db": true,
	"database": true, "dev": true, "demo": true, "default": true, "yourpassword": true,
	"your_password": true, "mypassword": true, "dbpassword": true, "dbpass": true,
	"1234": true, "12345": true, "123456": true, "1234567": true, "12345678": true, "123456789": true,
}

// placeholderPassword is a password that is visibly a blank to fill in: <password>, ${PW},
// $PASSWORD, {pw}, {{ pw }}, ****, xxxx, a printf verb (%s, %[1]s, %(pw)s). A password that only
// starts with % is not one: postgres://app:%21S3cr3t@db is a percent-encoded real password.
var placeholderPassword = regexp.MustCompile(`^(?:<[^<>]+>|\$\{[^{}]+\}|\$[A-Z_][A-Z0-9_]*|\{\{[^{}]+\}\}|\{[^{}]+\}|\*+|[xX]+|%(?:\(\w+\)|\[\d+\]|\d+\$)?[sdvq])$`)

// realPassword is true for a connection-string match whose password is neither a dummy nor a blank.
func realPassword(match string) bool {
	_, rest, _ := strings.Cut(match, "://")
	_, rest, _ = strings.Cut(rest, ":") // past the user name, which cannot hold ':'
	pw, _, _ := strings.Cut(rest, "@")  // the password cannot hold '@'
	return !dummyPasswords[strings.ToLower(pw)] && !placeholderPassword.MatchString(pw)
}

// looksGenerated is true for the credential part of "Bearer xyz" when it looks machine-made: a
// letter and a digit both. Placeholders ("Bearer YOUR_API_KEY_HERE") and prose ("basic
// internal/server/tokens.go") have no digit and are only warned about.
func looksGenerated(match string) bool {
	fields := strings.Fields(match)
	if len(fields) < 2 {
		return false
	}
	var letter, digit bool
	for _, r := range fields[len(fields)-1] {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letter = true
		}
	}
	return letter && digit
}

// opaque finds long mixed-case alphanumeric runs, which is what generated tokens
// look like. Lowercase hex (commit and file hashes) never matches.
var opaque = regexp.MustCompile(`[A-Za-z0-9_-]{32,}`)

func mixedCase(s string) bool {
	var upper, lower, digit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	return upper && lower && digit
}

// Find lists what looks like a credential in body, one finding per kind. Text a
// credential rule already matched is not looked at again by the weaker shapes, and
// text that has been masked ("[redacted:...]") is never a finding.
func Find(body string) []Finding {
	var out []Finding
	seen := map[string]bool{}
	add := func(kind string, strong bool) {
		if !seen[kind] {
			seen[kind] = true
			out = append(out, Finding{Kind: kind, Strong: strong})
		}
	}
	var rest []byte // body with what a rule recognised blanked out; made when the first one is found
	for _, r := range rules {
		for _, loc := range r.re.FindAllStringIndex(body, -1) {
			m := body[loc[0]:loc[1]]
			if strings.Contains(m, "redacted") || (r.genuine != nil && !r.genuine(m)) {
				continue
			}
			strong := r.strong && (r.accept == nil || r.accept(m))
			add(r.kind, strong)
			if strong { // do not look inside what was already recognised
				if rest == nil {
					rest = []byte(body)
				}
				for i := loc[0]; i < loc[1]; i++ {
					rest[i] = ' '
				}
			}
		}
	}
	text := body
	if rest != nil {
		text = string(rest)
	}
	for _, m := range opaque.FindAllString(text, -1) {
		if mixedCase(m) && !strings.Contains(m, "redacted") {
			add("long random token", false)
			break
		}
	}
	return out
}

// Strong returns the first finding that is nearly certainly a credential.
func Strong(body string) (Finding, bool) {
	for _, f := range Find(body) {
		if f.Strong {
			return f, true
		}
	}
	return Finding{}, false
}

// Refusal is the sentence a refused send is told.
func Refusal(f Finding) string {
	return "this message looks like it contains a " + f.Kind + ". Messages are kept in clear text on every node and shown in the dashboard: say where the secret lives instead of sending it"
}
