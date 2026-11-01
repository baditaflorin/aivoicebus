package release

import (
	"encoding/base64"
	"testing"
)

// Every built-in key is a whole ed25519 public key, and none is listed twice: a typo here would make a
// machine's signing key silently not count.
func TestBuiltinKeysAreWholeAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range builtinKeys {
		b, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(b) != 32 {
			t.Errorf("builtin key %q is not a 32-byte base64 ed25519 key (%v)", k, err)
		}
		if seen[k] {
			t.Errorf("builtin key %q is listed twice", k)
		}
		seen[k] = true
	}
	if len(builtinKeys) < 2 {
		t.Errorf("only %d builtin key: a single signing machine is a single point of failure", len(builtinKeys))
	}
}
