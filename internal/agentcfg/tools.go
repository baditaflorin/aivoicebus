package agentcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Besides Claude Code, two tools take session-start and per-prompt hooks whose output reaches the
// conversation (their official docs, 2026-10): Codex reads ~/.codex/hooks.json (plain stdout is
// added as context; new hooks run only once the user trusts them in /hooks), and Gemini CLI reads
// the "hooks" of ~/.gemini/settings.json (stdout must be JSON, which `hook --tool gemini` writes;
// timeouts are in milliseconds). Grok Build ignores what a session-start or prompt hook prints, so
// it gets no hook: it reads Claude Code's MCP servers and gets the tools that way.

func toolHookCommand(tool string, prompt bool) string {
	c := `[ -x "$HOME/.local/bin/aivoicebus" ] && "$HOME/.local/bin/aivoicebus" hook --tool ` + tool
	if prompt {
		c += " --event prompt"
	}
	return c + " || true"
}

// hookFile is one tool's hook configuration file and the entries aivoicebus keeps in it.
type hookFile struct {
	tool, dir, file string
	specs           []hookSpec
	trustNote       string // what the user must do once before the tool runs new hooks
}

var hookFiles = []hookFile{
	{tool: "codex", dir: ".codex", file: "hooks.json",
		specs: []hookSpec{
			{event: "SessionStart", command: toolHookCommand("codex", false), timeout: HookTimeout},
			{event: "UserPromptSubmit", command: toolHookCommand("codex", true), timeout: PromptHookTimeout},
		},
		trustNote: "Codex runs a new hook only after you trust it: run `aivoicebus trust-codex` once"},
	{tool: "gemini", dir: ".gemini", file: "settings.json",
		specs: []hookSpec{
			{event: "SessionStart", command: toolHookCommand("gemini", false), timeout: HookTimeout * 1000, name: "aivoicebus-start"},
			{event: "BeforeAgent", command: toolHookCommand("gemini", true), timeout: PromptHookTimeout * 1000, name: "aivoicebus-prompt"},
		}},
}

func (in *installer) jsonDoc(path, what string) (map[string]any, bool) {
	doc := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &doc); err != nil {
			in.add(what, Failed, filepath.Base(path)+" is not valid JSON, left alone: "+err.Error())
			return nil, false
		}
		if doc == nil {
			doc = map[string]any{}
		}
	case !os.IsNotExist(err):
		in.add(what, Failed, err.Error())
		return nil, false
	}
	return doc, true
}

func (in *installer) saveDoc(path, what string, doc map[string]any) bool {
	out, _ := json.MarshalIndent(doc, "", "  ")
	in.backup(path)
	if err := in.write(path, string(out)+"\n", 0o755); err != nil {
		in.add(what, Failed, err.Error())
		return false
	}
	return true
}

// toolHooks installs the session-start and per-prompt hooks of Codex and Gemini CLI.
func (in *installer) toolHooks() {
	for _, hf := range hookFiles {
		if !in.wants(hf.tool) {
			continue
		}
		what := hf.tool + " hook"
		if _, err := os.Stat(in.p.join(hf.dir)); err != nil {
			in.add(what, Skipped, "no ~/"+hf.dir+" (tool not installed); setup adds it once it exists")
			continue
		}
		path := in.p.join(hf.dir, hf.file)
		doc, ok := in.jsonDoc(path, what)
		if !ok {
			continue
		}
		changed, err := ensureHooks(doc, hf.specs)
		if err != nil {
			in.add(what, Failed, filepath.Base(path)+" left alone: "+err.Error())
			continue
		}
		if !changed {
			if c, ok := codexTrustCheck(in.p.Home); hf.tool == "codex" && ok && c.Level != LevelOK {
				in.add(what, OK, path+" ("+c.Detail+")")
				continue
			}
			in.add(what, OK, path)
			continue
		}
		if in.saveDoc(path, what, doc) {
			detail := path
			if hf.trustNote != "" {
				detail += " (" + hf.trustNote + ")"
			}
			in.add(what, Changed, detail)
		}
	}
}

func (in *installer) removeToolHooks() {
	for _, hf := range hookFiles {
		if !in.wants(hf.tool) {
			continue
		}
		what := hf.tool + " hook"
		path := in.p.join(hf.dir, hf.file)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		doc, ok := in.jsonDoc(path, what)
		if !ok {
			continue
		}
		changed, _ := removeHook(doc)
		if !changed {
			in.add(what, OK, "none in "+path)
			continue
		}
		if in.saveDoc(path, what, doc) {
			in.add(what, Changed, path)
		}
	}
}

// The MCP server is registered with every tool that has a place for it, so an agent of any make
// has aivoicebus_task, _claim, _wait, _status... as native tools.

const (
	tomlBegin = "# aivoicebus:begin (added by aivoicebus setup; aivoicebus uninstall removes it)"
	tomlEnd   = "# aivoicebus:end"
)

func codexMCPBlock(bin string) string {
	b, _ := json.Marshal(bin) // a JSON string is a valid TOML basic string
	return tomlBegin + "\n[mcp_servers.aivoicebus]\ncommand = " + string(b) + "\nargs = [\"mcp\"]\n" + tomlEnd + "\n"
}

// cutBlock returns s without the marked block, and whether there was one.
func cutBlock(s string) (string, bool) {
	i := strings.Index(s, tomlBegin)
	if i < 0 {
		return s, false
	}
	j := strings.Index(s[i:], tomlEnd)
	if j < 0 {
		return s, false
	}
	end := i + j + len(tomlEnd)
	if end < len(s) && s[end] == '\n' {
		end++
	}
	return s[:i] + s[end:], true
}

func (in *installer) mcpServers() {
	bin := in.p.Binary()
	// Codex: a table in ~/.codex/config.toml, kept between markers (no TOML parser in a
	// standard-library-only build, so the block is appended and replaced whole).
	if in.wants("codex") {
		path := in.p.join(".codex", "config.toml")
		if _, err := os.Stat(in.p.join(".codex")); err != nil {
			in.add("codex mcp", Skipped, "no ~/.codex (tool not installed)")
		} else {
			cur, _ := readFile(path)
			rest, had := cutBlock(cur)
			switch {
			case !had && strings.Contains(cur, "[mcp_servers.aivoicebus]"):
				in.add("codex mcp", OK, "registered by hand in "+path+", left alone")
			default:
				want := strings.TrimRight(rest, "\n")
				if want != "" {
					want += "\n\n"
				}
				want += codexMCPBlock(bin)
				if want == cur {
					in.add("codex mcp", OK, path)
					break
				}
				in.backup(path)
				if err := in.write(path, want, 0o755); err != nil {
					in.add("codex mcp", Failed, err.Error())
					break
				}
				in.add("codex mcp", Changed, path)
			}
		}
	}
	// Gemini CLI: mcpServers in ~/.gemini/settings.json.
	if in.wants("gemini") {
		path := in.p.join(".gemini", "settings.json")
		if _, err := os.Stat(in.p.join(".gemini")); err != nil {
			in.add("gemini mcp", Skipped, "no ~/.gemini (tool not installed)")
		} else if doc, ok := in.jsonDoc(path, "gemini mcp"); ok {
			servers, _ := doc["mcpServers"].(map[string]any)
			if servers == nil {
				servers = map[string]any{}
			}
			cur, _ := servers["aivoicebus"].(map[string]any)
			args, _ := cur["args"].([]any)
			if cur != nil && cur["command"] == bin && len(args) == 1 && args[0] == "mcp" {
				in.add("gemini mcp", OK, path)
			} else {
				servers["aivoicebus"] = map[string]any{"command": bin, "args": []any{"mcp"}}
				doc["mcpServers"] = servers
				if in.saveDoc(path, "gemini mcp", doc) {
					in.add("gemini mcp", Changed, path)
				}
			}
		}
	}
	// Claude Code (and Grok Build, which reads Claude Code's MCP servers): through `claude mcp`,
	// the tool's own way to change its user settings, when the caller provides it.
	if (in.wants("claude") || in.wants("grok")) && in.o.ClaudeMCP != nil {
		status, detail := in.o.ClaudeMCP(bin, in.o.DryRun)
		in.add("claude mcp", status, detail)
	}
}

func (in *installer) removeMCPServers() {
	if in.wants("codex") {
		path := in.p.join(".codex", "config.toml")
		if cur, ok := readFile(path); ok {
			if rest, had := cutBlock(cur); had {
				in.backup(path)
				if err := in.write(path, strings.TrimRight(rest, "\n")+"\n", 0o755); err != nil {
					in.add("codex mcp", Failed, err.Error())
				} else {
					in.add("codex mcp", Changed, path)
				}
			}
		}
	}
	if in.wants("gemini") {
		path := in.p.join(".gemini", "settings.json")
		if _, err := os.Stat(path); err == nil {
			if doc, ok := in.jsonDoc(path, "gemini mcp"); ok {
				if servers, _ := doc["mcpServers"].(map[string]any); servers != nil {
					if cur, _ := servers["aivoicebus"].(map[string]any); cur != nil && strings.Contains(commandOf(cur), "aivoicebus") {
						delete(servers, "aivoicebus")
						if len(servers) == 0 {
							delete(doc, "mcpServers")
						}
						if in.saveDoc(path, "gemini mcp", doc) {
							in.add("gemini mcp", Changed, path)
						}
					}
				}
			}
		}
	}
	if in.wants("claude") && in.o.ClaudeMCPRemove != nil {
		status, detail := in.o.ClaudeMCPRemove(in.o.DryRun)
		in.add("claude mcp", status, detail)
	}
}
