//go:build unix

package config

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// A token kept in a password store's directory is a symlink; it is a secret all the same, and
// skipping it would let that token through.
func TestSecretValuesFollowSymlinksToRegularFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_TOKEN_FILE", "")
	store := t.TempDir() // somewhere else entirely
	dir := filepath.Join(home, ".config/aivoicebus")
	os.MkdirAll(dir+"/machine-tokens", 0o700)
	write := func(name, v string) string {
		p := filepath.Join(store, name)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	link := func(target, at string) {
		if err := os.Symlink(target, at); err != nil {
			t.Fatal(err)
		}
	}
	link(write("machine-air", "air-token-via-symlink-0123456789"), dir+"/machine-tokens/air")
	link(write("observer", "observer-token-via-symlink-0123456789"), dir+"/observer-token")
	link(write("sharedlink", "shared-token-via-symlink-0123456789"), dir+"/token")
	link(filepath.Join(store, "missing"), dir+"/token.next") // dangling: skipped, no error
	link(store, dir+"/bootstrap-key")                        // a directory: skipped
	got := SecretValues()
	for _, want := range []string{"air-token-via-symlink-0123456789", "observer-token-via-symlink-0123456789", "shared-token-via-symlink-0123456789"} {
		if !slices.Contains(got, want) {
			t.Errorf("a symlink to a regular file must be read: %q missing from %d values", want, len(got))
		}
	}
}

// open(2) on a FIFO with no writer blocks, and every send reads these files: a FIFO (or a
// symlink to one) in the config directory must be skipped, not wait for ever.
func TestSecretValuesSkipAFIFOWithoutBlocking(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_TOKEN_FILE", "")
	dir := filepath.Join(home, ".config/aivoicebus")
	os.MkdirAll(dir+"/machine-tokens", 0o700)
	fifo := func(path string) {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("no FIFOs here: %v", err)
		}
	}
	fifo(dir + "/token")
	fifo(dir + "/observer-token")
	fifo(dir + "/machine-tokens/pipe")
	if err := os.Symlink(dir+"/token", dir+"/token.next"); err != nil { // a symlink to a FIFO
		t.Fatal(err)
	}
	os.WriteFile(dir+"/machine-tokens/air", []byte("air-token-0123456789abcdef\n"), 0o600)

	done := make(chan []string, 1)
	go func() { done <- SecretValues() }()
	select {
	case got := <-done:
		if !slices.Contains(got, "air-token-0123456789abcdef") || len(got) != 1 {
			t.Errorf("the regular file is read and the FIFOs are not: %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SecretValues blocked on a FIFO")
	}
}
