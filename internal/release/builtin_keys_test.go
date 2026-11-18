package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every built-in key is a whole ed25519 public key, and none is listed twice: a typo here would make a
// machine's signing key silently not count. Each names the machine that holds it and the release that
// first carried it, and no machine holds two.
func TestBuiltinKeysAreWholeAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	machines := map[string]bool{}
	cur := repoVersion(t)
	for _, k := range builtinKeys {
		b, err := base64.StdEncoding.DecodeString(k.Key)
		if err != nil || len(b) != 32 {
			t.Errorf("builtin key %q is not a 32-byte base64 ed25519 key (%v)", k.Key, err)
		}
		if seen[k.Key] {
			t.Errorf("builtin key %q is listed twice", k.Key)
		}
		seen[k.Key] = true
		if k.Label == "" || strings.ContainsAny(k.Label, " \t\n") {
			t.Errorf("builtin key %q names no machine (Label %q): say which machine holds its private half", k.Key, k.Label)
		}
		if machines[k.Label] {
			t.Errorf("machine %s holds two built-in keys: one signer per machine, revoke the old one", k.Label)
		}
		machines[k.Label] = true
		if !ValidVersion(k.Since) {
			t.Errorf("builtin key of %s: Since %q is not a release number", k.Label, k.Since)
		} else if Compare(k.Since, nextMinor(cur)) > 0 {
			t.Errorf("builtin key of %s: Since %s is past the next release after %s", k.Label, k.Since, cur)
		}
	}
	if len(builtinKeys) < 2 {
		t.Errorf("only %d builtin key: a single signing machine is a single point of failure", len(builtinKeys))
	}
	// Signers that every release since 0.15.0 carries, so any of them can sign today.
	ready := 0
	for _, k := range builtinKeys {
		if Compare(k.Since, cur) < 0 {
			ready++
		}
	}
	if ready < 2 {
		t.Errorf("only %d key can sign release %s+: at least two signers must already ship", ready, cur)
	}
}

func repoVersion(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// nextMinor is the release after v that a new key would first ship in, at most: X.(Y+1).0.
func nextMinor(v string) string {
	p := strings.Split(v, ".")
	if len(p) != 3 {
		return v
	}
	n, err := strconv.Atoi(p[1])
	if err != nil {
		return v
	}
	return p[0] + "." + strconv.Itoa(n+1) + ".0"
}

// The release that adds a signer cannot be signed by that signer: machines on older releases do not
// carry its key yet. Signing a later release with it is fine, and a key that is not built in passes with
// a note.
func TestCheckSigner(t *testing.T) {
	old := builtinKeys
	defer func() { builtinKeys = old }()
	pubOld, _, _ := ed25519.GenerateKey(nil)
	pubNew, _, _ := ed25519.GenerateKey(nil)
	pubOther, _, _ := ed25519.GenerateKey(nil)
	builtinKeys = []builtinKey{
		{Key: EncodeKey(pubOld), Label: "old-mac", Since: "0.1.0"},
		{Key: EncodeKey(pubNew), Label: "new-mac", Since: "0.17.0"},
	}
	home(t)
	if _, err := CheckSigner(pubOld, "0.17.0"); err != nil {
		t.Errorf("an old key must sign the release that adds a new one: %v", err)
	}
	if _, err := CheckSigner(pubNew, "0.17.0"); err == nil || !strings.Contains(err.Error(), "new-mac") || !strings.Contains(err.Error(), "0.17.0") {
		t.Errorf("the new key signed the release that first carries it: %v", err)
	}
	if _, err := CheckSigner(pubNew, "0.16.9"); err == nil {
		t.Error("the new key signed a release older than the one that carries it")
	}
	if _, err := CheckSigner(pubNew, "0.17.1"); err != nil {
		t.Errorf("the new key must sign the release after the one that carries it: %v", err)
	}
	note, err := CheckSigner(pubOther, "0.17.0")
	if err != nil || !strings.Contains(note, "not built into this binary") {
		t.Errorf("a key that is not built in: note %q, err %v", note, err)
	}
}

// Signer names which trusted key signed, so `release verify` can say which machine signed a release.
func TestSignerNamesTheKey(t *testing.T) {
	pubA, privA, _ := ed25519.GenerateKey(nil)
	pubB, privB, _ := ed25519.GenerateKey(nil)
	keys := []Key{{Public: pubA, Label: "a"}, {Public: pubB, Label: "b"}}
	m := []byte(`{"version":"0.17.0"}`)
	for _, c := range []struct {
		priv ed25519.PrivateKey
		want string
	}{{privA, "a"}, {privB, "b"}} {
		k, err := Signer(m, []byte(Sign(m, c.priv)), keys)
		if err != nil || k.Label != c.want {
			t.Errorf("Signer = %q, %v; want %q", k.Label, err, c.want)
		}
	}
	_, privC, _ := ed25519.GenerateKey(nil)
	if _, err := Signer(m, []byte(Sign(m, privC)), keys); err != ErrBadSignature {
		t.Errorf("an untrusted key: %v, want ErrBadSignature", err)
	}
	if err := Verify(m, []byte(Sign(m, privB)), []ed25519.PublicKey{pubA, pubB}); err != nil {
		t.Errorf("Verify with any one of several keys: %v", err)
	}
}

func TestPublicKeyPath(t *testing.T) {
	for in, want := range map[string]string{
		"/h/.config/aivoicebus/release-signing.key": "/h/.config/aivoicebus/release-signing.pub",
		"k": "k.pub",
	} {
		if got := PublicKeyPath(in); got != want {
			t.Errorf("PublicKeyPath(%q) = %q, want %q", in, got, want)
		}
	}
}
