package config

import (
	"bufio"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// This machine's keys: an ed25519 key every session on it signs messages with, and an X25519 key
// messages sealed for this machine are opened with. Both live in the config directory, mode 0600,
// made on first use. Losing them only means other machines see a new key (KEY CHANGED) once.

const (
	signKeyFile  = "machine.key" // base64 ed25519 seed
	boxKeyFile   = "machine.box" // base64 X25519 private key
	knownMachine = "known-machines"
)

func readOrMake(name string, size int, make func() ([]byte, error)) ([]byte, error) {
	dir := Dir()
	if dir == "" {
		return nil, errors.New("no home directory")
	}
	path := filepath.Join(dir, name)
	if b, err := os.ReadFile(path); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != size {
			return nil, errors.New(path + " is not a key this release can read")
		}
		return raw, nil
	}
	raw, err := make()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) { // another process made it at the same moment: use theirs
			return readOrMake(name, size, make)
		}
		return nil, err
	}
	_, err = f.WriteString(base64.StdEncoding.EncodeToString(raw) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return raw, err
}

// MachineKeys returns this machine's signing key and the private half of its box key.
func MachineKeys() (ed25519.PrivateKey, []byte, error) {
	seed, err := readOrMake(signKeyFile, ed25519.SeedSize, func() ([]byte, error) {
		b := make([]byte, ed25519.SeedSize)
		_, err := rand.Read(b)
		return b, err
	})
	if err != nil {
		return nil, nil, err
	}
	box, err := readOrMake(boxKeyFile, 32, func() ([]byte, error) {
		k, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		return k.Bytes(), nil
	})
	if err != nil {
		return nil, nil, err
	}
	return ed25519.NewKeyFromSeed(seed), box, nil
}

// KnownMachine is the signing and box keys pinned for host (base64), and whether there are any.
func KnownMachine(host string) (key, box string, ok bool) {
	dir := Dir()
	if dir == "" {
		return "", "", false
	}
	f, err := os.Open(filepath.Join(dir, knownMachine))
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) >= 2 && fs[0] == host {
			if len(fs) >= 3 {
				box = fs[2]
			}
			return fs[1], box, true
		}
	}
	return "", "", false
}

// PinMachine records host's keys the first time they are seen (and its box key when it was not
// known yet). It never replaces a pinned signing key: a changed one is for the user to accept,
// with `aivoicebus keys forget HOST`.
func PinMachine(host, key, box string) error {
	if strings.ContainsAny(host+key+box, " \t\n") || host == "" || key == "" {
		return errors.New("not a host and key")
	}
	dir := Dir()
	if dir == "" {
		return errors.New("no home directory")
	}
	path := filepath.Join(dir, knownMachine)
	cur, _ := os.ReadFile(path)
	var out []string
	found := false
	for _, l := range strings.Split(string(cur), "\n") {
		fs := strings.Fields(l)
		if len(fs) == 0 {
			continue
		}
		if fs[0] == host {
			found = true
			if fs[1] == key && len(fs) < 3 && box != "" {
				l = host + " " + key + " " + box
			}
		}
		out = append(out, l)
	}
	if !found {
		line := host + " " + key
		if box != "" {
			line += " " + box
		}
		out = append(out, line)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// KnownMachinesText is the pinned keys file as it is ("" when there is none).
func KnownMachinesText() string {
	dir := Dir()
	if dir == "" {
		return ""
	}
	b, _ := os.ReadFile(filepath.Join(dir, knownMachine))
	return string(b)
}

// ForgetMachine removes host's pinned keys, so the next key seen for it is pinned again.
func ForgetMachine(host string) (bool, error) {
	dir := Dir()
	if dir == "" {
		return false, errors.New("no home directory")
	}
	path := filepath.Join(dir, knownMachine)
	cur, err := os.ReadFile(path)
	if err != nil {
		return false, nil
	}
	var out []string
	found := false
	for _, l := range strings.Split(string(cur), "\n") {
		if fs := strings.Fields(l); len(fs) > 0 && fs[0] == host {
			found = true
			continue
		}
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	if !found {
		return false, nil
	}
	return true, os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}
