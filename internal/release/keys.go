package release

import (
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

// builtinKeys are the public keys (base64 ed25519) of the project's own releases, compiled in, one per
// machine that may sign a release. They are the root of trust of an official binary: it accepts updates
// any one of these keys signed and nothing else, unless its user deliberately adds a key (see
// UserKeyFile) or a fork builds in its own (see extraKey). Each private half stays on the machine named in
// Label and never leaves it, so no single machine is the only one that can sign a release.
//
// Since is the first release that carried the key. A client accepts a signature only from a key its own
// binary carries, so a key can sign release X only when Since is older than X: machines still on a
// release before Since would refuse it. `release pack` enforces that (see CheckSigner).
//
// Adding a signer: on that machine `aivoicebus release keygen` (prints only the public key and its
// fingerprint), then a line here with Since set to the next release, signed by a key that is already
// trusted. Revoking: delete the line, in a release signed by another key. docs/releasing.md has the steps.
var builtinKeys = []builtinKey{
	// 2026-10-15: the first public release key; it signed 0.1.0 to 0.15.0 and 0.16.0.
	{Key: "VZWMody5REXot6TMsPTVcgTgN8DBFmLmI3L+vd9MH4k=", Label: "pentests-macbook-air-m1-mba", Since: "0.1.0"},
	// 2026-11-01: one signing key per build machine (#126). The M4 Air's key signed 0.15.1 and 0.15.2.
	{Key: "ff/I+IEJIit1UyGaus2GwBBlft7q3di5+latmZnCzoM=", Label: "florins-macbook-air-m4-mba", Since: "0.15.0"},
	{Key: "vjyWCC0IqnXZ8zdmZ2+uBsnpRKJgol9hGMPjdnmUnyY=", Label: "imac-m1-imac", Since: "0.15.0"},
	{Key: "ifPkwhdAJPKrwwAQtxU8kZNRQhx/PXn8KWoS+EvzgVY=", Label: "mac14-m1pro-mbp", Since: "0.15.0"},
}

// builtinKey is one signer the project's binaries trust: its public key, the machine that holds the
// private half, and the first release that carried it.
type builtinKey struct {
	Key   string
	Label string
	Since string
}

// extraKey is one more public key a build can carry, for a fork or a company that signs its own
// releases. It is set when the binary is built, never at run time:
//
//	go build -ldflags "-X github.com/baditaflorin/aivoicebus/internal/release.extraKey=<base64 key>"
var extraKey string

// UserKeyFileName is the file in the config directory that holds the keys the user chose to trust.
const UserKeyFileName = "trusted-keys"

// UserKeyFile is where the keys a user added with `aivoicebus trust-key add` are kept: one per line,
// "<base64 key> [label]". It is read only when it is a regular file that nobody else can write or read:
// a file that can add a key lets whoever can write it redirect updates, so it is held to the same mode as the token.
func UserKeyFile() string { return filepath.Join(config.Dir(), UserKeyFileName) }

// Key is one trusted public key and why it is trusted.
type Key struct {
	Public ed25519.PublicKey
	Source string // "built in", "built in for this fork" or "trust-key"
	Label  string // for a built-in key, the machine that holds its private half
	Since  string // for a built-in key, the first release that carried it
}

// Fingerprint is the short name of a public key a person can compare: "SHA256:" and the unpadded
// base64 of the SHA-256 of the raw key bytes (the shape ssh-keygen -l prints).
func Fingerprint(k ed25519.PublicKey) string {
	h := sha256.Sum256(k)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])
}

// Keys returns every key this process trusts, with where each comes from, and a note for each
// source that was ignored (a trust-key file that others can read or write, an unreadable line).
func Keys() ([]Key, []string) {
	var out []Key
	var notes []string
	for _, b := range builtinKeys {
		if k, err := ParseKey(b.Key); err == nil {
			out = append(out, Key{Public: k, Source: "built in", Label: b.Label, Since: b.Since})
		}
	}
	if extraKey != "" {
		if k, err := ParseKey(extraKey); err == nil {
			out = append(out, Key{Public: k, Source: "built in for this fork"})
		} else {
			notes = append(notes, "the key this build was given for a fork is not a valid public key")
		}
	}
	user, usernotes := readUserKeys(UserKeyFile())
	return append(out, user...), append(notes, usernotes...)
}

// Builtin returns the built-in key that is k (the project's own signers, and a fork's key), and whether
// there is one. A key a user added does not count: other machines do not trust it.
func Builtin(k ed25519.PublicKey) (Key, bool) {
	ks, _ := Keys()
	for _, have := range ks {
		if have.Source != "trust-key" && have.Public.Equal(k) {
			return have, true
		}
	}
	return Key{}, false
}

// Signer returns which of keys signed manifest (sig is the contents of manifest.sig), so a person can
// see which machine signed a release. ErrBadSignature when none did.
func Signer(manifest, sig []byte, keys []Key) (Key, error) {
	pubs := make([]ed25519.PublicKey, 0, len(keys))
	for _, k := range keys {
		pubs = append(pubs, k.Public)
	}
	i, err := verifyIndex(manifest, sig, pubs)
	if err != nil {
		return Key{}, err
	}
	return keys[i], nil
}

// CheckSigner says why the key pub must not sign release version, or nil. A built-in key whose Since is
// not older than version shipped in this release or a later one, so every machine on an older release
// would refuse the update: sign with a key those machines already carry. A key that is not built in is
// allowed (a fork, a test, a team's own key), with the note returned for the person to see.
func CheckSigner(pub ed25519.PublicKey, version string) (note string, err error) {
	k, ok := Builtin(pub)
	if !ok {
		return fmt.Sprintf("the signing key %s is not built into this binary: official clients will refuse this release unless they trust it", Fingerprint(pub)), nil
	}
	if k.Since != "" && ValidVersion(version) && ValidVersion(k.Since) && !Newer(version, k.Since) {
		return "", fmt.Errorf("the key of %s (%s) first ships in release %s, so machines on older releases do not trust it yet: sign %s with a key an older release already carries (aivoicebus trust-key list shows each key's release)", k.Label, Fingerprint(pub), k.Since, version)
	}
	return "", nil
}

// TrustedKeys returns the public keys whose signatures this process accepts.
func TrustedKeys() []ed25519.PublicKey {
	ks, _ := Keys()
	out := make([]ed25519.PublicKey, 0, len(ks))
	for _, k := range ks {
		out = append(out, k.Public)
	}
	return out
}

func checkUserKeyFile(path string) (os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is accessible to other users (mode %04o); chmod 600 it", path, fi.Mode().Perm())
	}
	return fi, nil
}

func readUserKeys(path string) ([]Key, []string) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if _, err := checkUserKeyFile(path); err != nil {
		return nil, []string{"ignoring " + err.Error()}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, []string{"ignoring " + err.Error()}
	}
	defer f.Close()
	var keys []Key
	var notes []string
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		word, label, _ := strings.Cut(line, " ")
		k, err := ParseKey(word)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s line %d is not a public key: ignored", path, n))
			continue
		}
		keys = append(keys, Key{Public: k, Source: "trust-key", Label: strings.TrimSpace(label)})
	}
	return keys, notes
}

// AddUserKey records k (with a one-line label) in the user key file, creating it with mode 0600.
// It does nothing, and says so, when the key is already trusted.
func AddUserKey(k ed25519.PublicKey, label string) (added bool, err error) {
	for _, have := range TrustedKeys() {
		if have.Equal(k) {
			return false, nil
		}
	}
	path := UserKeyFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	if _, err := os.Lstat(path); err == nil {
		if _, err := checkUserKeyFile(path); err != nil {
			return false, err
		}
	}
	label = strings.Join(strings.Fields(label), " ")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return false, err
	}
	_, werr := fmt.Fprintf(f, "%s %s\n", EncodeKey(k), label)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr == nil, werr
}

// RemoveUserKey drops the user key with this fingerprint and reports whether there was one.
func RemoveUserKey(fingerprint string) (bool, error) {
	path := UserKeyFile()
	if _, err := checkUserKeyFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var keep []string
	removed := false
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		word, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if k, err := ParseKey(word); err == nil && Fingerprint(k) == fingerprint {
			removed = true
			continue
		}
		keep = append(keep, line)
	}
	if !removed {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(keep, "\n")+"\n"), 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}
