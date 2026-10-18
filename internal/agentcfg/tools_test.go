package agentcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stepOf(steps []Step, name string) Step {
	for _, s := range steps {
		if s.Name == name {
			return s
		}
	}
	return Step{}
}

func TestSetupHooksMCPAndInstructionsForEveryTool(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{".codex", ".gemini", ".gemini/antigravity", ".grok"} {
		os.MkdirAll(filepath.Join(home, d), 0o755)
	}
	// What the user already has stays.
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("model = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"x\"\n"), 0o644)
	os.WriteFile(filepath.Join(home, ".gemini", "settings.json"), []byte(`{"theme":"dark","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"mine"}]}]}}`), 0o644)
	steps, err := Setup(Options{Home: home, ClaudeMCP: func(bin string, dry bool) (string, string) { return Changed, "fake " + bin }})
	if err != nil {
		t.Fatalf("%v %+v", err, steps)
	}
	for _, n := range []string{"codex hook", "gemini hook", "codex mcp", "gemini mcp", "antigravity mcp", "grok instructions", "claude mcp"} {
		if s := stepOf(steps, n); s.Status != Changed {
			t.Errorf("%s: %+v", n, s)
		}
	}
	if !strings.Contains(stepOf(steps, "codex hook").Detail, "aivoicebus trust-codex") {
		t.Error("the codex step does not say the hooks must be trusted once")
	}

	var codex map[string]any
	b, _ := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	json.Unmarshal(b, &codex)
	h := codex["hooks"].(map[string]any)
	if !strings.Contains(string(b), "hook --tool codex --event prompt") || h["SessionStart"] == nil || h["UserPromptSubmit"] == nil {
		t.Errorf("codex hooks.json: %s", b)
	}
	toml, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(string(toml), "model = \"gpt-5\"") || !strings.Contains(string(toml), "[mcp_servers.other]") ||
		!strings.Contains(string(toml), "[mcp_servers.aivoicebus]") || !strings.Contains(string(toml), `args = ["mcp"]`) {
		t.Errorf("config.toml:\n%s", toml)
	}
	gem, _ := os.ReadFile(filepath.Join(home, ".gemini", "settings.json"))
	var g map[string]any
	json.Unmarshal(gem, &g)
	if g["theme"] != "dark" || !strings.Contains(string(gem), `"command": "mine"`) || !strings.Contains(string(gem), "BeforeAgent") ||
		!strings.Contains(string(gem), `"timeout": 2000`) || !strings.Contains(string(gem), `"name": "aivoicebus-start"`) || g["mcpServers"].(map[string]any)["aivoicebus"] == nil {
		t.Errorf("gemini settings.json: %s", gem)
	}
	agyMCP, _ := os.ReadFile(filepath.Join(home, ".gemini", "config", "mcp_config.json"))
	if !strings.Contains(string(agyMCP), `"aivoicebus"`) {
		t.Errorf("antigravity mcp_config.json: %s", agyMCP)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".grok", "AGENTS.md")); !strings.Contains(string(b), "aivoicebus") {
		t.Error("no grok instructions")
	}

	// A second run changes nothing.
	steps, _ = Setup(Options{Home: home})
	for _, n := range []string{"codex hook", "gemini hook", "codex mcp", "gemini mcp", "antigravity mcp", "grok instructions"} {
		if s := stepOf(steps, n); s.Status != OK {
			t.Errorf("second run %s: %+v", n, s)
		}
	}
	// Uninstall takes out exactly what setup put in.
	if _, err := Uninstall(RemoveOptions{Home: home}); err != nil {
		t.Fatal(err)
	}
	toml, _ = os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	gem, _ = os.ReadFile(filepath.Join(home, ".gemini", "settings.json"))
	agyMCP, _ = os.ReadFile(filepath.Join(home, ".gemini", "config", "mcp_config.json"))
	codexHooks, _ := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	if strings.Contains(string(toml), "aivoicebus") || !strings.Contains(string(toml), "[mcp_servers.other]") ||
		strings.Contains(string(gem), "aivoicebus") || !strings.Contains(string(gem), `"mine"`) ||
		strings.Contains(string(agyMCP), "aivoicebus") || strings.Contains(string(codexHooks), "aivoicebus") {
		t.Errorf("after uninstall:\n%s\n%s\n%s\n%s", toml, gem, agyMCP, codexHooks)
	}
}

func TestAServerRegisteredByHandIsLeftAlone(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	mine := "[mcp_servers.aivoicebus]\ncommand = \"/opt/aivoicebus\"\nargs = [\"mcp\"]\n"
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(mine), 0o644)
	steps, _ := Setup(Options{Home: home, Tools: []string{"codex"}})
	if s := stepOf(steps, "codex mcp"); s.Status != OK || !strings.Contains(s.Detail, "by hand") {
		t.Errorf("%+v", s)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml")); string(b) != mine {
		t.Errorf("rewritten: %s", b)
	}
}
