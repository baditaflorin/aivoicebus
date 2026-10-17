// Package assets holds the texts aivoicebus ships inside the binary, so what an
// agent is told always matches the binary it runs and there is no file to drift.
package assets

import _ "embed"

//go:embed usage.md
var usage string

//go:embed snippet.md
var snippet string

//go:embed standby-claude.md
var standbyClaude string

//go:embed standby-mcp.md
var standbyMCP string

// StandbyClaude is Claude Code's /aivoicebus-standby command (~/.claude/commands/aivoicebus-standby.md).
func StandbyClaude() string { return standbyClaude }

// StandbyMCP is the MCP server's "standby" prompt, for any client that lists prompts (Codex, Gemini, ...).
func StandbyMCP() string { return standbyMCP }

// Usage is the full how-to, printed by `aivoicebus usage`.
func Usage() string { return usage }

// Snippet is the short block placed in each agent tool's instruction file.
func Snippet() string { return snippet }
