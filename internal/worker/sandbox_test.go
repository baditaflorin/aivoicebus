package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A sandboxed run cannot read a secret file the same command reads without the sandbox.
func TestASandboxedRunCannotReadSecrets(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_test"), []byte("SECRET-KEY-MATERIAL"), 0o600)
	cmd := []string{"cat", filepath.Join(home, ".ssh", "id_test")}
	plain := Exec(t.Context(), cmd, "", os.Environ(), Limits{Timeout: 10e9, MaxOutput: 1000})
	if !strings.Contains(plain.Stdout, "SECRET-KEY-MATERIAL") {
		t.Fatalf("control: %+v", plain)
	}
	argv, how, ok := Sandboxed(cmd, home)
	if !ok {
		t.Skipf("no sandbox on %s here (no sandbox-exec / bwrap)", runtime.GOOS)
	}
	r := Exec(t.Context(), argv, "", os.Environ(), Limits{Timeout: 10e9, MaxOutput: 1000})
	if strings.Contains(r.Stdout, "SECRET-KEY-MATERIAL") {
		t.Errorf("the sandbox (%s) let the run read ~/.ssh", how)
	}
}
