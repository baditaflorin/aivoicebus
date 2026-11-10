package agentcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The forwarder's start-up is asked for by name: a plain setup does not touch the shell's rc file.
func TestTheForwarderStartUpIsOptInAndRemovable(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".bashrc"), []byte("export A=1\n"), 0o644)
	rc := filepath.Join(home, ".bashrc")
	script := filepath.Join(home, ".local", "bin", "ensure-aivoicebus-forward")
	skip := func(string, bool) (string, string) { return Skipped, "" }

	steps, _ := Setup(Options{Home: home, ClaudeMCP: skip})
	if s := stepOf(steps, "forwarder script"); s.Name != "" {
		t.Fatalf("a plain setup set the forwarder up: %+v", s)
	}
	if _, err := os.Stat(script); err == nil {
		t.Fatal("a plain setup wrote the script")
	}

	dry, _ := Setup(Options{Home: home, Tools: []string{"grokbot"}, DryRun: true, ClaudeMCP: skip})
	if stepOf(dry, "forwarder script").Status != Changed {
		t.Errorf("dry run: %+v", dry)
	}
	if _, err := os.Stat(script); err == nil {
		t.Fatal("a dry run wrote the script")
	}

	opts := Options{Home: home, Tools: []string{"grokbot"}, ClaudeMCP: skip}
	steps, _ = Setup(opts)
	if stepOf(steps, "forwarder script").Status != Changed || stepOf(steps, "forwarder start").Status != Changed {
		t.Fatalf("%+v", steps)
	}
	b, _ := os.ReadFile(script)
	fi, _ := os.Stat(script)
	if !strings.Contains(string(b), "forward ensure") || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("script %q mode %v", b, fi.Mode())
	}
	if rcText, _ := os.ReadFile(rc); !strings.Contains(string(rcText), "ensure-aivoicebus-forward") || !strings.HasPrefix(string(rcText), "export A=1\n") {
		t.Errorf("rc: %q", rcText)
	}
	steps, _ = Setup(opts)
	if stepOf(steps, "forwarder script").Status != OK || stepOf(steps, "forwarder start").Status != OK {
		t.Errorf("second run: %+v", steps)
	}
	if rcText, _ := os.ReadFile(rc); strings.Count(string(rcText), "ensure-aivoicebus-forward") != 1 {
		t.Errorf("the line was added twice: %q", rcText)
	}

	if _, err := Uninstall(RemoveOptions{Home: home}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err == nil {
		t.Error("uninstall left the script")
	}
	if rcText, _ := os.ReadFile(rc); string(rcText) != "export A=1\n" {
		t.Errorf("uninstall left %q", rcText)
	}
}
