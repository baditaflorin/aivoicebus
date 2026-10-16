package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSecretValuesReadEveryFileThatHoldsACredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_TOKEN_FILE", "")
	dir := filepath.Join(home, ".config/aivoicebus")
	write := func(name, text string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("token", "shared-token-0123456789abcdef\n")
	write("token.next", "next-token-0123456789abcdef\n")
	write("token.nodes", "# per node\nshared-nodes-0123456789abcdef\nlaptop-a:machine-token-0123456789abcdef\n\n~backup:backup-token-0123456789abcdef\n")
	write("observer-token", "observer-token-0123456789abcdef\n")
	write("bootstrap-key", "bootstrap-key-0123456789abcdef\n")
	write("release-signing.key", "c2lnbmluZy1zZWVkLTAxMjM0NTY3ODlhYmNkZWY=\n")
	write("machine-tokens/air", "air-token-0123456789abcdef\n")
	write("machine-tokens/pi", "pi-token-0123456789abcdef\n")
	write("credentials/deploykey/token", "handed-over-deploykey-0123456789abcdef")
	write("credentials/deploykey/hosts", "ci.example.com\n")       // the host list is not a credential
	write("env", "export AIVOICEBUS_URL=http://100.64.0.1:7777\n") // settings, not a secret
	write("hostlabel.json", `{"label":"mbp-0123456789abcdef"}`)    // neither

	got := SecretValues()
	for _, want := range []string{
		"shared-token-0123456789abcdef", "next-token-0123456789abcdef", "shared-nodes-0123456789abcdef",
		"laptop-a:machine-token-0123456789abcdef", "machine-token-0123456789abcdef",
		"backup-token-0123456789abcdef", "observer-token-0123456789abcdef", "bootstrap-key-0123456789abcdef",
		"c2lnbmluZy1zZWVkLTAxMjM0NTY3ODlhYmNkZWY=", "air-token-0123456789abcdef", "pi-token-0123456789abcdef",
		"handed-over-deploykey-0123456789abcdef", // a credential saved with `aivoicebus secret save` must not be sendable either
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing a value from the files (%d values read)", len(got))
		}
	}
	for _, v := range got {
		if v == "ci.example.com" || strings.Contains(v, "AIVOICEBUS_URL") || strings.Contains(v, "mbp-0123456789abcdef") || strings.HasPrefix(v, "#") || v == "" {
			t.Errorf("not a credential: %q", v)
		}
	}
}

func TestSecretValuesIncludeTheTokenInUseAndSurviveMissingFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no config directory at all
	t.Setenv("AIVOICEBUS_TOKEN_FILE", "")
	t.Setenv("AIVOICEBUS_TOKEN", "")
	if got := SecretValues(); len(got) != 0 {
		t.Errorf("nothing to read: %v", got)
	}
	t.Setenv("AIVOICEBUS_TOKEN", "from-the-environment-0123456789")
	if got := SecretValues(); !slices.Contains(got, "from-the-environment-0123456789") {
		t.Errorf("the token the client uses is a secret too: %v", got)
	}
	viaFile := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(viaFile, []byte("from-the-token-file-0123456789\n"), 0o600)
	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_TOKEN_FILE", viaFile)
	if got := SecretValues(); !slices.Contains(got, "from-the-token-file-0123456789") {
		t.Errorf("AIVOICEBUS_TOKEN_FILE: %v", got)
	}
}

func TestSecretValuesBoundWhatTheyReadAndIgnoreDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_TOKEN_FILE", "")
	dir := filepath.Join(home, ".config/aivoicebus")
	os.MkdirAll(filepath.Join(dir, "machine-tokens/nested"), 0o700)
	os.WriteFile(filepath.Join(dir, "observer-token"), []byte(strings.Repeat("x", 3<<20)), 0o600)
	got := SecretValues()
	if len(got) != 1 || len(got[0]) > maxSecretFile {
		t.Errorf("a file is read only up to %d bytes: %d values", maxSecretFile, len(got))
	}
}

func TestSecretValuesStripAByteOrderMarkAndAnyKindOfLineEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_TOKEN_FILE", "")
	dir := filepath.Join(home, ".config/aivoicebus")
	os.MkdirAll(dir, 0o700)
	// a file saved by a Windows editor: byte order mark, CRLF; and one with old Mac line ends
	os.WriteFile(filepath.Join(dir, "token"), []byte("\xef\xbb\xbfwindows-token-0123456789abcdef\r\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "token.nodes"), []byte("\xef\xbb\xbf# nodes\rmac:first-token-0123456789abcdef\rsecond-token-0123456789abcdef\r"), 0o600)
	got := SecretValues()
	for _, want := range []string{"windows-token-0123456789abcdef", "first-token-0123456789abcdef", "second-token-0123456789abcdef"} {
		if !slices.Contains(got, want) {
			t.Errorf("%q is missing from %d values", want, len(got))
		}
	}
	for _, v := range got {
		if strings.ContainsAny(v, "\r\n") || strings.HasPrefix(v, "\xef\xbb\xbf") || strings.HasPrefix(v, "#") {
			t.Errorf("a value keeps a byte order mark, line end or comment: %q", v)
		}
	}
}
