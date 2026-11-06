//go:build linux || darwin

package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/term"
)

func TestLeaseTerminalInputRefusedBeforeAnyRequest(t *testing.T) {
	pty := openLeaseTestPTY(t)
	if !term.IsTerminal(pty) {
		t.Fatal("fixture is not a terminal")
	}
	n := newLeaseFailureNode(t, func(*leaseFailureNode) bool { return false }, false)
	marker := filepath.Join(t.TempDir(), "ran")
	err := leaseRun(n.client, nil, "fixture/runner", []string{leaseKey, "--settle", "0s", "--", "sh", "-c", `: > "$1"`, "fixture", marker}, Streams{In: pty, Out: io.Discard, Err: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "terminal input") || n.checks.Load() != 0 || n.sends.Load() != 0 || exists(marker) {
		t.Fatalf("terminal was not refused before mutation: %v checks=%d sends=%d", err, n.checks.Load(), n.sends.Load())
	}
}

func TestLeaseDevNullInputAllowed(t *testing.T) {
	n := newLeaseFailureNode(t, func(*leaseFailureNode) bool { return false }, false)
	in, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	err = leaseRun(n.client, nil, "fixture/runner", []string{leaseKey, "--settle", "0s", "--", "sh", "-c", "exit 0"}, Streams{In: in, Out: io.Discard, Err: io.Discard})
	if code, ok := err.(exitError); !ok || code.code != 0 {
		t.Fatalf("/dev/null rejected: %v", err)
	}
}
