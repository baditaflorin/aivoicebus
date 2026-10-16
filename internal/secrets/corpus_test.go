package secrets

import (
	"crypto/ed25519"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// token returns n letters and digits in the mix a generated token has (upper, lower and digit;
// deploy/ops.sh gen_token makes 48 such characters).
func token(n int) string {
	const set = "aB3dE5gH7jK9mN1pQ2sT4vW6yZ8cF0hL"
	b := make([]byte, n)
	for i := range b {
		b[i] = set[(i*7)%len(set)]
	}
	return string(b)
}

// hexn returns n lowercase hex characters (n <= 64) derived from seed.
func hexn(seed string, n int) string {
	h := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(h[:])[:n]
}

// signingKey is a release signing key as `aivoicebus release keygen` writes it: the base64 of the
// 32-byte ed25519 seed. Different phrases give seeds whose base64 has + or / (which cut it into
// short runs) or not.
func signingKey(phrase string) string {
	seed := sha256.Sum256([]byte(phrase))
	return base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed[:]).Seed())
}

// builders make the credentials the corpus refers to as {{NAME}}. Each is joined from pieces here,
// at test time, so that no credential-shaped string is committed (GitHub push protection and other
// scanners would flag it, and the corpus would then be unpublishable).
var builders = map[string]func() string{
	// fake tokens of the corpus's own lines, split so the file holds no credential-shaped literal
	"CORPUS1": func() string { return "tske" + "y-auth-rotation-procedure-overview" },
	"CORPUS2": func() string { return "tske" + "y-auth-rotation-procedure-2026-overview" },
	"CORPUS3": func() string { return "ASIA" + "PACIFICREGIONSTA" },
	"CORPUS4": func() string { return "AKIA" + "IOSFODNN7EXAMPLE" },
	"CORPUS5": func() string { return "rk_l" + "ive_thisisalonglowercaseslugwithoutcaps" },
	"CORPUS6": func() string { return "AIza" + "thisisalonglowercaseslugwithnocaps1" },
	"PEM":     func() string { return "-----BEGIN " + "OPENSSH PRIVATE" + " KEY-----" },
	"GHP":     func() string { return "gh" + "p_" + token(36) },
	"GHS":     func() string { return "gh" + "s_" + token(36) },
	"GHPAT":   func() string { return "github" + "_pat_" + token(22) + "_" + token(30) },
	"GLPAT":   func() string { return "gl" + "pat-" + token(20) },
	"TSKEY":   func() string { return "ts" + "key-" + "auth-" + "k8s4zr3CNTRL-" + token(32) },
	"AKIA":    func() string { return "AK" + "IA" + "IOSFODNN7EXAMPLE" },
	"SLACK":   func() string { return "xo" + "xb-" + "1234567890-" + token(24) },
	"SK":      func() string { return "s" + "k-" + "proj-" + token(32) },
	"SKLIVE":  func() string { return "s" + "k_" + "live_" + token(32) },
	"AIZA":    func() string { return "AI" + "za" + "Sy" + token(33) },
	"NPM":     func() string { return "np" + "m_" + token(36) },
	"HF":      func() string { return "h" + "f_" + token(34) },
	"JWT": func() string {
		return "ey" + "J" + "hbGciOiJIUzI1NiJ9." + "ey" + "J" + "zdWIiOiIxMjM0NTY3ODkwIn0." + token(43)
	},
	"GLPAT2":   func() string { return "gl" + "pat-" + token(8) + "-" + token(5) + "_" + token(6) },
	"TSKEY2":   func() string { return "ts" + "key-" + "api-" + "kAbCdE12CNTRL-" + token(32) },
	"AIZA2":    func() string { return "AI" + "za" + "Sy" + token(10) + "-" + token(10) + "_" + token(11) },
	"SKANT":    func() string { return "s" + "k-ant-" + "api03-" + token(40) },
	"STRIPERK": func() string { return "r" + "k_live_" + token(26) },
	"STRIPESK": func() string { return "s" + "k_test_" + token(26) },
	"SHOPIFY":  func() string { return "shp" + "at_" + hexn("shopify", 32) },
	"SHOPIFYC": func() string { return "shp" + "ca_" + strings.ToUpper(hexn("shopify", 32)) },
	"DOKEY":    func() string { return "do" + "p_v1_" + hexn("digitalocean", 64) },
	"SENDGRID": func() string {
		return "S" + "G." + token(10) + "-" + token(5) + "_" + token(5) + "." + token(20) + "_" + token(10) + "-" + token(11)
	},
	"XAPP": func() string { return "xa" + "pp-1-" + "A01B2C3D4E5-" + "1234567890123-" + hexn("xapp", 64) },
	// each of these is one character short of a real one: not credentials
	"NSHOPIFY":  func() string { return "shp" + "at_" + hexn("shopify", 31) },
	"NDOKEY":    func() string { return "do" + "p_v1_" + hexn("digitalocean", 63) },
	"NSENDGRID": func() string { return "S" + "G." + token(21) + "." + token(43) },
	"NSTRIPE":   func() string { return "r" + "k_live_" + token(23) },
	"NXAPP":     func() string { return "xa" + "pp-1-" + "A01B2C3D4E5-" + "12345-" + hexn("xapp", 64) },
	"FB":        func() string { return "f" + "b_" + token(22) },
	"BOOTSTRAP": func() string { return "http://100.64.0.1:7777/boot" + "strap/" + token(40) + ".tgz" },
	"ALNUM24":   func() string { return token(24) },
	"DSN":       func() string { return "postgres" + "://app:" + "S3cr3t" + "Pass99" + "@db.internal:5432/app" },
	"DSNREDIS":  func() string { return "redis" + "://:" + "S3cr3t" + "Pass99" + "@cache.internal:6379" },
	"DSNPCT":    func() string { return "postgres" + "://app:" + "%21S3cr3t" + "Pass99" + "@db.internal/app" },
	"DSNMONGO":  func() string { return "mongodb+srv" + "://admin:" + "Tr0ub4dor" + "3xyz" + "@cluster0.example.net/db" },
	"DSNMYSQL":  func() string { return "mysql" + "://root:" + "xK9mQ2vL7p" + "@10.0.0.5:3306/app" },
	"DSNAMQP":   func() string { return "amqp" + "://guest:" + "Zq81vXw2Lm" + "@rabbit" },
	"GEN48":     func() string { return token(48) },
	"SIGNKEY":   func() string { return signingKey("aivoicebus corpus signing seed 3") },
	"SHA256HEX": func() string { h := sha256.Sum256([]byte("aivoicebus")); return hex.EncodeToString(h[:]) },
	"SHA1HEX":   func() string { h := sha1.Sum([]byte("aivoicebus")); return hex.EncodeToString(h[:]) },
	"SHA256B64": func() string {
		h := sha256.Sum256([]byte("aivoicebus"))
		return base64.StdEncoding.EncodeToString(h[:])
	},
}

var placeholder = regexp.MustCompile(`\{\{([A-Z0-9]+)\}\}`)

// expand turns {{NAME}} into its credential and \n into a newline.
func expand(t *testing.T, s string) string {
	t.Helper()
	s = strings.ReplaceAll(s, `\n`, "\n")
	return placeholder.ReplaceAllStringFunc(s, func(m string) string {
		b, ok := builders[placeholder.FindStringSubmatch(m)[1]]
		if !ok {
			t.Fatalf("the corpus uses %s, which corpus_test.go does not build", m)
		}
		return b()
	})
}

type corpusLine struct {
	n                   int
	verdict, want, body string // body as written: still holds {{NAME}} and \n
}

func readCorpus(t *testing.T) []corpusLine {
	t.Helper()
	b, err := os.ReadFile("testdata/corpus.txt")
	if err != nil {
		t.Fatal(err)
	}
	var out []corpusLine
	for i, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " | ", 3)
		if len(parts) != 3 {
			t.Fatalf("testdata/corpus.txt:%d: want \"verdict | expectation | body\"", i+1)
		}
		out = append(out, corpusLine{i + 1, parts[0], parts[1], parts[2]})
	}
	if len(out) < 50 {
		t.Fatalf("the corpus has %d lines; it should not shrink", len(out))
	}
	return out
}

// TestCorpus is the committed record of what the detector must refuse, only warn about, and let
// through. A new rule or a changed pattern that breaks a line here is a regression, whichever way.
func TestCorpus(t *testing.T) {
	for _, c := range readCorpus(t) {
		body := expand(t, c.body)
		fs := Find(body)
		var strong *Finding
		weakKinds := map[string]bool{}
		for i, f := range fs {
			if f.Strong {
				strong = &fs[i]
			} else {
				weakKinds[f.Kind] = true
			}
		}
		fail := func(format string, args ...any) {
			t.Errorf("testdata/corpus.txt:%d %q: "+format, append([]any{c.n, c.body}, args...)...)
		}
		switch c.verdict {
		case "strong":
			f, ok := Strong(body)
			if !ok || f.Kind != c.want {
				fail("got %+v refused=%v, want a refusal for %s", fs, ok, c.want)
			}
		case "weak":
			if strong != nil {
				fail("refused as %s, want only a warning", strong.Kind)
			}
			if !weakKinds[c.want] {
				fail("got %+v, want a warning for %s", fs, c.want)
			}
		case "pass":
			if strong != nil {
				fail("refused as %s, want it to pass", strong.Kind)
			}
		case "clean":
			if len(fs) != 0 {
				fail("flagged as %+v, want nothing", fs)
			}
		default:
			fail("unknown verdict %q", c.verdict)
		}
	}
}

// The corpus must be safe to commit: with the placeholders still in it, no line is a credential.
// If this fails, a literal credential-shaped string got into testdata/corpus.txt.
func TestCorpusFileHoldsNoCredential(t *testing.T) {
	for _, c := range readCorpus(t) {
		if f, ok := Strong(strings.ReplaceAll(c.body, `\n`, "\n")); ok {
			t.Errorf("testdata/corpus.txt:%d holds a literal %s; write it as a {{NAME}} built in corpus_test.go", c.n, f.Kind)
		}
	}
}

// A rule nobody wrote a corpus line for is a rule nobody checks: every strong kind needs one.
func TestEveryStrongRuleHasACorpusLine(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range readCorpus(t) {
		if c.verdict == "strong" {
			covered[c.want] = true
		}
	}
	for _, r := range rules {
		if r.strong && !covered[r.kind] {
			t.Errorf("no corpus line expects a refusal for %q", r.kind)
		}
	}
}

// The prefixes that are specific enough (GitHub, GitLab, Tailscale) are a credential even when a
// letter or digit comes right before them; the short ones need a clean start, or they would be
// found inside words and base64. Underscore, space and punctuation are a clean start for all.
// A token of one fixed length (an AWS key, a Shopify token) must also end where it ends: more
// letters or digits after it make it a longer word, not a credential.
func TestWhereATokenMayStartAndEnd(t *testing.T) {
	for _, tc := range []struct {
		name            string
		anywhere, fixed bool
	}{
		{"GHP", true, false}, {"GHPAT", true, false}, {"GLPAT", true, false}, {"GLPAT2", true, false},
		{"TSKEY", true, false}, {"TSKEY2", true, false},
		{"AKIA", false, true}, {"SLACK", false, false}, {"XAPP", false, false}, {"SK", false, false},
		{"SKANT", false, false}, {"STRIPERK", false, false}, {"AIZA", false, true}, {"AIZA2", false, true},
		{"NPM", false, false}, {"HF", false, false}, {"SHOPIFY", false, true}, {"DOKEY", false, true},
		{"SENDGRID", false, true}, {"JWT", false, false}, {"FB", false, false},
	} {
		cred := builders[tc.name]()
		for _, start := range []string{"", " ", "_", "x_", "-", "=", `"`, "(", ":", "\n", "/"} {
			if _, ok := Strong(start + cred); !ok {
				t.Errorf("%s after %q must be refused", tc.name, start)
			}
		}
		for _, start := range []string{"x", "X", "7"} {
			if _, ok := Strong(start + cred); ok != tc.anywhere {
				t.Errorf("%s glued to %q: refused=%v, want %v", tc.name, start, ok, tc.anywhere)
			}
		}
		for _, tail := range []string{" and more", `")`, ".\n"} {
			if _, ok := Strong(cred + tail); !ok {
				t.Errorf("%s followed by %q must be refused", tc.name, tail)
			}
		}
		if tc.fixed {
			if _, ok := Strong(cred + "X"); ok {
				t.Errorf("%s with a letter after it is a longer word, not a credential", tc.name)
			}
		} else if _, ok := Strong(cred + "_suffix"); !ok {
			t.Errorf("%s followed by _suffix must be refused", tc.name)
		}
	}
}

// A generated 48-character token is only a warning: any 48-character word could be one. What
// refuses the node's real tokens is the exact match (canary_test.go).
func TestAGeneratedTokenIsOnlyAWarning(t *testing.T) {
	body := "the shared token is " + token(48)
	if _, ok := Strong(body); ok {
		t.Error("a bare 48-character token must not be refused by shape")
	}
	if fs := Find(body); len(fs) != 1 || fs[0].Kind != "long random token" || fs[0].Strong {
		t.Errorf("%+v", fs)
	}
}

func TestADSNIsRefusedOnlyForARealisticPassword(t *testing.T) {
	dsn := func(pw string) string { return "postgres" + "://app:" + pw + "@host/db" }
	// dummies and blanks to fill in: ordinary talk about connection strings
	for _, pw := range []string{
		"password", "PASSWORD", "Pass", "postgres", "guest", "secret", "changeme", "admin", "root", "test", "1234", "123456",
		"<password>", "<your-password>", "$PASSWORD", "$DB_PASS", "${DB_PASSWORD}", "{pw}", "{{pw}}", "****", "xxxx", "XXXXXXXX",
		"%(pw)s", "%[1]s", "%s", "%v",
	} {
		if f, ok := Strong(dsn(pw)); ok {
			t.Errorf("password %q is a dummy or a blank, not a credential: %+v", pw, f)
		}
	}
	// a realistic password, however it is written
	for _, pw := range []string{"hunter2", "S3cr3t!x", "%21S3cr3tPass99", "$ecret99", "p4ssw0rd", "passwordX1", "Tr0ub4dor3", "<oops"} {
		if f, ok := Strong(dsn(pw)); !ok || f.Kind != "connection string with a password" {
			t.Errorf("password %q must be refused: %+v %v", pw, f, ok)
		}
	}
}

// A port followed by a query or fragment is not a user:password pair, whatever follows an @.
func TestAPortAndAnEmailInAURLAreNotAPassword(t *testing.T) {
	for _, body := range []string{
		"http://localhost:3000?email=bob@example.com",
		"smtp://h:587?from=ops@example.com",
		"http://localhost:8080#note@x.example",
		"http://x?y=1:abcdef@host",
		"see https://example.com:8443/path?next=/a@b and mailto:ops@example.com",
	} {
		if f, ok := Strong(body); ok {
			t.Errorf("%q: %+v", body, f)
		}
	}
}

// Many matches must not make Find quadratic: blanking what was recognised is one pass.
func TestFindStaysLinearWithManyCredentials(t *testing.T) {
	body := strings.Repeat("x://a:bcd@h ", 3000)
	start := time.Now()
	if _, ok := Strong(body); !ok {
		t.Fatal("a connection string with a password is refused")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("%d matches took %v", 3000, d)
	}
}
