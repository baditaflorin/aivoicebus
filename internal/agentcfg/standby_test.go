package agentcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setup writes /aivoicebus-standby for Claude Code, leaves a file of the user's own by that name
// alone, and uninstall removes only its own.
func TestTheStandbyCommandIsOursToWriteAndRemove(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	opts := Options{Home: home, ClaudeMCP: func(string, bool) (string, string) { return Skipped, "" }}
	steps, _ := Setup(opts)
	path := filepath.Join(home, ".claude", "commands", "aivoicebus-standby.md")
	if s := stepOf(steps, "claude standby"); s.Status != Changed {
		t.Fatalf("%+v", s)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "aivoicebus wait --for 30m") || !strings.Contains(string(b), "run_in_background") {
		t.Errorf("%s", b)
	}
	if steps, _ = Setup(opts); stepOf(steps, "claude standby").Status != OK {
		t.Errorf("second run: %+v", stepOf(steps, "claude standby"))
	}
	if _, err := Uninstall(RemoveOptions{Home: home}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("uninstall left the command")
	}
	os.WriteFile(path, []byte("my own standby\n"), 0o644)
	steps, _ = Setup(opts)
	if s := stepOf(steps, "claude standby"); s.Status != Skipped {
		t.Errorf("own file: %+v", s)
	}
	Uninstall(RemoveOptions{Home: home})
	if b, _ := os.ReadFile(path); string(b) != "my own standby\n" {
		t.Errorf("a file of the user's own was changed: %q", b)
	}
}
