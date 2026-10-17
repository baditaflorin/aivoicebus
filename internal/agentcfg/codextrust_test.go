package agentcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Codex keeps trust per hook under [hooks.state."<hooks.json>:<event>:<group>:<handler>"] in
// config.toml; doctor and setup read it to say whether `trust-codex` is still needed.
func TestCodexHookTrustIsReadFromConfigToml(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	// a hook of the user's own comes first, so ours is group 1 of SessionStart
	os.WriteFile(filepath.Join(home, ".codex", "hooks.json"), []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"mine"}]}]}}`), 0o644)
	if _, err := Setup(Options{Home: home, ClaudeMCP: func(string, bool) (string, string) { return Skipped, "" }}); err != nil {
		t.Fatal(err)
	}
	hs, err := CodexHooks(home)
	if err != nil || len(hs) != 2 {
		t.Fatalf("%v %+v", err, hs)
	}
	hj := filepath.Join(home, ".codex", "hooks.json")
	if hs[0].Key != hj+":session_start:1:0" || hs[1].Key != hj+":user_prompt_submit:0:0" || hs[0].Trusted || hs[1].Trusted {
		t.Fatalf("%+v", hs)
	}
	c, ok := codexTrustCheck(home)
	if !ok || c.Level != LevelWarn || !strings.Contains(c.Detail, "aivoicebus trust-codex") {
		t.Errorf("untrusted: %+v", c)
	}
	// setup run again (nothing to change) still says what is missing
	steps, _ := Setup(Options{Home: home, ClaudeMCP: func(string, bool) (string, string) { return Skipped, "" }})
	if s := stepOf(steps, "codex hook"); !strings.Contains(s.Detail, "trust-codex") {
		t.Errorf("setup: %+v", s)
	}
	// what Codex writes once the user trusted one of them
	cfg := filepath.Join(home, ".codex", "config.toml")
	old, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, append(old, []byte("\n[hooks.state]\n\n[hooks.state.\""+hj+":session_start:1:0\"]\ntrusted_hash = \"sha256:aa\"\n")...), 0o644)
	if hs, _ = CodexHooks(home); !hs[0].Trusted || hs[1].Trusted {
		t.Fatalf("one trusted: %+v", hs)
	}
	f, _ := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("\n[hooks.state.\"" + hj + ":user_prompt_submit:0:0\"]\ntrusted_hash = \"sha256:bb\"\n")
	f.Close()
	if c, _ := codexTrustCheck(home); c.Level != LevelOK {
		t.Errorf("both trusted: %+v", c)
	}
	// no Codex at all: doctor says nothing about it
	if _, ok := codexTrustCheck(t.TempDir()); ok {
		t.Error("a machine without Codex got a codex line")
	}
}
