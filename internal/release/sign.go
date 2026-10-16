package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrNoTrustedKeys means the binary carries no public key, so no release can
// be verified.
var ErrNoTrustedKeys = errors.New("this build trusts no release signing key")

// ErrBadSignature means no trusted key signed these manifest bytes.
var ErrBadSignature = errors.New("manifest signature does not match any trusted key")

// GenerateKey makes a new signing key pair.
func GenerateKey() (ed25519.PrivateKey, ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, pub, err
}

// EncodeKey is the text form of a public key.
func EncodeKey(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

// ParseKey decodes the text form of a public key.
func ParseKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not a base64 ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}

// SavePrivateKey writes the key (its 32-byte seed, base64) to path with mode
// 0600, refusing to overwrite: losing a signing key by accident is worse than
// an error.
func SavePrivateKey(path string, priv ed25519.PrivateKey) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// LoadPrivateKey reads a key written by SavePrivateKey. It insists the file is
// not readable by others.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by other users (mode %04o); chmod 600 it", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("not a aivoicebus signing key file")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// Sign returns the base64 signature of manifest.
func Sign(manifest []byte, priv ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest))
}

// Verify checks that one of keys signed manifest. sig is the contents of
// manifest.sig. Several keys are accepted so a key can be rotated: ship a
// release, signed by the old key, whose binary also trusts the new one.
func Verify(manifest, sig []byte, keys []ed25519.PublicKey) error {
	if len(keys) == 0 {
		return ErrNoTrustedKeys
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return ErrBadSignature
	}
	for _, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, manifest, raw) {
			return nil
		}
	}
	return ErrBadSignature
}
