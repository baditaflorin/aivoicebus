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

// builtinKeys are the public keys (base64 ed25519) of the project's own releases, compiled in.
// They are the root of trust of an official binary: it accepts updates the project signed and
// nothing else, unless its user deliberately adds a key (see UserKeyFile) or a fork builds in its own
// (see extraKey).
//
// Rotating: release a build that lists the new key as well, signed with the old
// one; once every machine runs it, drop the old key in a later release.
var builtinKeys = []string{
	// 2026-10-15: the first public release key. The private half is kept offline by the maintainer.
	"VZWMody5REXot6TMsPTVcgTgN8DBFmLmI3L+vd9MH4k=",
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
	Label  string
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
	for _, s := range builtinKeys {
		if k, err := ParseKey(s); err == nil {
			out = append(out, Key{Public: k, Source: "built in"})
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
