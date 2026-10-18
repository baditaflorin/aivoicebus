package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/agentcfg"
	"github.com/baditaflorin/aivoicebus/internal/config"
)

// doctor says whether a person can approve anything on this machine: with none of the approver
// key, a notify URL or a terminal, a token install from an agent's session waits for nobody.
func TestDoctorWarnsWhenNobodyCanApprove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_NOTIFY_URL", "")
	t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:1")
	t.Setenv("AIVOICEBUS_TOKEN", "tok")

	_, out, _ := run(t, "", "doctor", "--offline")
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, " approval ") {
			line = l
		}
	}
	if !strings.HasPrefix(line, "WARN") || !strings.Contains(line, "aivoicebus approver init") || !strings.Contains(line, "aivoicebus watch notify set URL") {
		t.Fatalf("no approver key, no notify URL, no terminal: want a WARN naming both fixes, got %q\n%s", line, out)
	}

	if c := approvalCheck(true); c.Level != agentcfg.LevelOK || !strings.Contains(c.Detail, "only by y/N at a terminal") {
		t.Errorf("at a terminal: %+v", c)
	}

	// A file that is not an approver key is not a way to approve, and doctor says so.
	os.MkdirAll(config.Dir(), 0o700)
	os.WriteFile(config.ApproverPath(), []byte("junk"), 0o600)
	if c := approvalCheck(false); c.Level != agentcfg.LevelWarn || !strings.Contains(c.Detail, "unreadable") {
		t.Errorf("a damaged approver key: %+v", c)
	}
	os.Remove(config.ApproverPath())

	os.WriteFile(filepath.Join(config.Dir(), notifyFile), []byte("https://ntfy.example.test/x\n"), 0o600)
	if c := approvalCheck(false); c.Level != agentcfg.LevelOK || !strings.Contains(c.Detail, "phone") {
		t.Errorf("a notify URL: %+v", c)
	}
	os.Remove(filepath.Join(config.Dir(), notifyFile))

	if _, err := config.CreateApprover("a long enough passphrase"); err != nil {
		t.Fatal(err)
	}
	if c := approvalCheck(false); c.Level != agentcfg.LevelOK || !strings.Contains(c.Detail, "approver key") {
		t.Errorf("an approver key: %+v", c)
	}
}

// An agent's tool call often runs with /dev/null as stdin, a character device: not a terminal.
func TestDevNullIsNoTerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("/dev/null counted as a terminal")
	}
}
