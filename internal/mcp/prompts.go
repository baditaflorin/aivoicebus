package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/baditaflorin/aivoicebus/internal/assets"
)

// Prompts are what a client offers its user as commands (MCP prompts/list, prompts/get): Codex,
// Gemini CLI and Claude Code show them, so "stand by" is one command in any of them.
var prompts = map[string]struct{ description, text string }{
	"standby": {"Stand by for aivoicebus messages from other agents until you say stop: wait (no tokens while waiting), wake on a message, ask before acting on it", assets.StandbyMCP()},
}

func promptList() []map[string]any {
	var out []map[string]any
	for name, p := range prompts {
		out = append(out, map[string]any{"name": name, "description": p.description})
	}
	return out
}

func promptGet(raw json.RawMessage) (any, *rpcError) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" {
		return nil, &rpcError{codeInvalidParams, "prompts/get needs a name"}
	}
	pr, ok := prompts[p.Name]
	if !ok {
		return nil, &rpcError{codeInvalidParams, fmt.Sprintf("unknown prompt %q", p.Name)}
	}
	return map[string]any{
		"description": pr.description,
		"messages":    []map[string]any{{"role": "user", "content": map[string]any{"type": "text", "text": pr.text}}},
	}, nil
}
