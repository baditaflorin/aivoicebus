package secrets

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAPGPPrivateKeyBlockIsRefused(t *testing.T) {
	body := "backup:\n-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQdGBF..."
	if f, ok := Strong(body); !ok || f.Kind != "private key" {
		t.Errorf("%+v %v", f, ok)
	}
}

func TestPlaceholdersAndProseAfterBearerOrBasicOnlyDrawAWarning(t *testing.T) {
	for _, body := range []string{
		"send the header Authorization: Bearer YOUR_API_KEY_HERE and it works",
		"the basic internal/server/tokens.go handles that",
		"use Bearer <your-token-goes-here> as the value",
	} {
		if f, ok := Strong(body); ok {
			t.Errorf("%q must not be refused: %+v", body, f)
		}
	}
	// a generated-looking credential after the same words is still refused
	if _, ok := Strong("Authorization: Bearer 9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c"); !ok {
		t.Error("a real bearer token must still be refused")
	}
	if _, ok := Strong("Authorization: Basic dXNlcjpwYXNzd29yZDEyMzQ1Njc4"); !ok {
		t.Error("a real basic credential (base64 with digits) must still be refused")
	}
}

func TestStrongPatternsAreRefusedWithTheirKind(t *testing.T) {
	for kind, body := range map[string]string{
		"GitHub token":      "push with ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789 now",
		"AWS access key":    "AKIA" + "IOSFODNN7EXAMPLE is the key",
		"Slack token":       "xoxb" + "-1234567890-abcdefghijkl",
		"API key":           "use sk-p" + "roj-AbcdefghijklmnopqrstuV1234", // real keys are mixed case
		"JWT":               "eyJh" + "bGciOiJIUzI1NiJ9.eyJz" + "dWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		"service API key":   "key fb_0123456789abcdefABCDEF",
		"bootstrap link":    "curl http://100.64.0.14:7777/bootstrap/faf3bd7e1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f.tgz",
		"bearer credential": "Authorization: Bearer abcdefghijklmnop1234567890",
		"private key":       "key:\n----" + "-BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA",
	} {
		f, ok := Strong(body)
		if !ok || f.Kind != kind || !f.Strong {
			t.Errorf("%q: got %+v %v, want a strong %s", body, f, ok, kind)
		}
		if !strings.Contains(Refusal(f), kind) {
			t.Errorf("the refusal names what it found: %q", Refusal(f))
		}
	}
}

func TestWeakShapesAreFoundButNeverRefused(t *testing.T) {
	for _, body := range []string{
		"AIVOICEBUS_TOKEN=Zm9vYmFyYmF6cXV4MTIzNDU2",
		`password: "hunter2hunter2"`,
		"the generated one is Qm9ycm93ZWRUaW1lQW5kU3BhY2UxMjM0NTY3ODkw ok",
	} {
		fs := Find(body)
		if len(fs) == 0 {
			t.Errorf("%q: nothing found", body)
		}
		for _, f := range fs {
			if f.Strong {
				t.Errorf("%q: %+v must be a warning, not a refusal", body, f)
			}
		}
		if _, ok := Strong(body); ok {
			t.Errorf("%q must not be refused", body)
		}
	}
}

func TestOrdinaryAgentTrafficIsNeverFlagged(t *testing.T) {
	for _, body := range []string{
		"aivoicebus 0.4.1 installed (checksum and signature verified), doctor all green, mesh reachable.",
		"[aivoicebus] You are laptop-a/claude.9058. 1 unread message(s) from other agents.",
		"merged a745853 and 3c5f45c5c, release v0.4.1; main is 3c5f45c5c0a1b2c3d4e5f60718293a4b5c6d7e8f",
		"sha256 afa197c40004bd1b1f37444716586c4de79cabe209c4d03e56ca7f9dd8606ed8 for the darwin binary",
		"never print AIVOICEBUS_TOKEN or put a token in a message",
		"token: short",
		"The password is rotated monthly.",
		"see https://github.com/baditaflorin/aivoicebus/pull/8 and /home/dev/src/aivoicebus/internal/store/store.go",
		"the bootstrap link on node-a is still on and expires about 16:07",
		"ssh node-c_bastion 'systemctl is-active aivoicebus'",
		"nodes 100.64.0.14:7777, 100.64.0.13:7777 and 100.64.0.12:7777 replicate",
		"Bearer tokens are discussed in docs/observer.md",
		"api key rotation is due, the key id is the first 8 hex chars: a1b2c3d4",
		strings.Repeat("a long lowercase run without digits or capitals ", 20),
	} {
		if fs := Find(body); len(fs) != 0 {
			t.Errorf("%q flagged as %+v", body, fs)
		}
	}
}

func TestEachKindIsReportedOnceAndAlreadyMaskedTextIsFine(t *testing.T) {
	body := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789 and ghp_" + "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ and gith" + "ub_pat_11ABCDEFG0abcdefghijklmnop_qrstuvwxyz"
	fs := Find(body)
	if len(fs) != 1 || fs[0].Kind != "GitHub token" {
		t.Errorf("%+v", fs)
	}
	if fs := Find("token is [redacted:github-token] and password: [redacted:value]"); len(fs) != 0 {
		t.Errorf("text the dashboard already masked is not a secret: %+v", fs)
	}
}

// TestRealTrafficCorpus runs the detector over a real feed (AIVOICEBUS_CORPUS=file, JSON
// lines of replication records) to measure false positives and see what it would have
// stopped. It prints only the kind and message id, never the text. Skipped when unset.
func TestRealTrafficCorpus(t *testing.T) {
	path := os.Getenv("AIVOICEBUS_CORPUS")
	if path == "" {
		t.Skip("AIVOICEBUS_CORPUS not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	total, strong, weak := 0, 0, 0
	for sc.Scan() {
		var r struct {
			Msg *struct {
				ID, From, Body string
			} `json:"msg"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Msg == nil {
			continue
		}
		total++
		for _, fd := range Find(r.Msg.Body) {
			if fd.Strong {
				strong++
			} else {
				weak++
			}
			t.Logf("%s %-7s %-24s %s", r.Msg.ID, map[bool]string{true: "STRONG", false: "weak"}[fd.Strong], fd.Kind, r.Msg.From)
		}
	}
	t.Logf("%d messages: %d strong findings, %d weak", total, strong, weak)
}
