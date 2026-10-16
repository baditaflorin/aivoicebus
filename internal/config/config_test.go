package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestURLsTrimsAndSkipsEmpties(t *testing.T) {
	t.Setenv("AIVOICEBUS_URL", " http://a:7777/ ,,http://b:7777 ,")
	want := []string{"http://a:7777", "http://b:7777"}
	if got := URLs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("URLs() = %v, want %v", got, want)
	}
	t.Setenv("AIVOICEBUS_URL", "")
	if got := URLs(); len(got) != 0 {
		t.Fatalf("empty env should give no URLs, got %v", got)
	}
}

func TestTokenPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	write := func(path, v string) {
		os.MkdirAll(filepath.Dir(path), 0o700)
		os.WriteFile(path, []byte(v+"\n"), 0o600)
	}
	write(filepath.Join(home, ".config/aivoicebus/token"), "from-default-file")
	explicit := filepath.Join(t.TempDir(), "explicit")
	write(explicit, "from-explicit-file")
	viaEnvFile := filepath.Join(t.TempDir(), "viaenv")
	write(viaEnvFile, "from-token-file-env")

	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_TOKEN_FILE", "")
	if got := Token(""); got != "from-default-file" {
		t.Errorf("default file: %q", got)
	}
	t.Setenv("AIVOICEBUS_TOKEN_FILE", viaEnvFile)
	if got := Token(""); got != "from-token-file-env" {
		t.Errorf("AIVOICEBUS_TOKEN_FILE: %q", got)
	}
	if got := Token(explicit); got != "from-explicit-file" {
		t.Errorf("explicit file should beat AIVOICEBUS_TOKEN_FILE: %q", got)
	}
	t.Setenv("AIVOICEBUS_TOKEN", "from-env")
	if got := Token(explicit); got != "from-env" {
		t.Errorf("AIVOICEBUS_TOKEN should win: %q", got)
	}
}

func TestLoadEnvFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".config/aivoicebus"), 0o700)
	envFile := filepath.Join(home, ".config/aivoicebus/env")
	os.WriteFile(envFile, []byte("export PATH=\"$HOME/.local/bin:$PATH\"\nexport AIVOICEBUS_URL=\"http://a:7777,http://b:7777\"\n"), 0o644)

	t.Setenv("AIVOICEBUS_URL", "")
	LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_URL"); got != "http://a:7777,http://b:7777" {
		t.Errorf("not loaded from the env file: %q", got)
	}
	t.Setenv("AIVOICEBUS_URL", "http://explicit:1")
	LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_URL"); got != "http://explicit:1" {
		t.Errorf("an explicit AIVOICEBUS_URL must not be overridden: %q", got)
	}
}

func TestLoadEnvFileAlsoLoadsTheHostOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".config/aivoicebus"), 0o700)
	os.WriteFile(filepath.Join(home, ".config/aivoicebus/env"),
		[]byte("export AIVOICEBUS_URL=http://a:7777\nexport AIVOICEBUS_HOST=laptop-a16\n"), 0o644)
	t.Setenv("AIVOICEBUS_URL", "")
	t.Setenv("AIVOICEBUS_HOST", "")
	LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_HOST"); got != "laptop-a16" {
		t.Errorf("AIVOICEBUS_HOST = %q", got)
	}
	t.Setenv("AIVOICEBUS_HOST", "from-shell")
	LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_HOST"); got != "from-shell" {
		t.Errorf("an explicit AIVOICEBUS_HOST must win: %q", got)
	}
}

func TestLoadEnvFileAlsoLoadsTheCapabilities(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".config/aivoicebus"), 0o700)
	os.WriteFile(filepath.Join(home, ".config/aivoicebus/env"), []byte("export AIVOICEBUS_CAPS=\"ask,run\"\n"), 0o644)
	t.Setenv("AIVOICEBUS_CAPS", "")
	LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_CAPS"); got != "ask,run" {
		t.Errorf("AIVOICEBUS_CAPS = %q", got)
	}
	t.Setenv("AIVOICEBUS_CAPS", "read-only")
	LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_CAPS"); got != "read-only" {
		t.Errorf("an explicit value must win: %q", got)
	}
}
