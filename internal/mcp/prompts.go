package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/assets"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
)

// Prompts are what a client offers its user as commands (MCP prompts/list, prompts/get): Codex,
// Gemini CLI and Claude Code show them, so "stand by" is one command in any of them.
var prompts = map[string]struct {
	description string
	arguments   []map[string]any
	text        func(args map[string]string) (string, error)
}{
	"standby": {
		"Stand by for aivoicebus messages from other agents until you say stop: wait (no tokens while waiting), wake on a message, ask before acting on it",
		[]map[string]any{{"name": "only", "description": "what wakes it: all (default), tasks, or replies (answers to what this session sent)", "required": false}},
		standbyText,
	},
}

// standbyText is the standby prompt, its waits scoped by the "only" argument.
func standbyText(args map[string]string) (string, error) {
	only := strings.ToLower(strings.TrimSpace(args["only"]))
	if !tasks.ValidOnly(only) {
		return "", fmt.Errorf("only is `%s`, `%s` or `%s`, not %q", tasks.OnlyAll, tasks.OnlyTasks, tasks.OnlyReplies, args["only"])
	}
	scope := ""
	if only != "" && only != tasks.OnlyAll {
		scope = fmt.Sprintf(" and only %q", only)
	}
	return strings.ReplaceAll(assets.StandbyMCP(), "{{only}}", scope), nil
}

func promptList() []map[string]any {
	var out []map[string]any
	for name, p := range prompts {
		out = append(out, map[string]any{"name": name, "description": p.description, "arguments": p.arguments})
	}
	return out
}

func promptGet(raw json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" {
		return nil, &rpcError{codeInvalidParams, "prompts/get needs a name"}
	}
	pr, ok := prompts[p.Name]
	if !ok {
		return nil, &rpcError{codeInvalidParams, fmt.Sprintf("unknown prompt %q", p.Name)}
	}
	text, err := pr.text(p.Arguments)
	if err != nil {
		return nil, &rpcError{codeInvalidParams, err.Error()}
	}
	return map[string]any{
		"description": pr.description,
		"messages":    []map[string]any{{"role": "user", "content": map[string]any{"type": "text", "text": text}}},
	}, nil
}
