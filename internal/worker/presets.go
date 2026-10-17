package worker

import (
	"slices"
	"sort"
)

// Preset is an agent CLI's headless command, read-only by default, that takes the task on standard
// input (their docs, 2026-10; Grok Build's stdin handling is not documented and is best-effort).
// A preset never lets the agent edit files or run commands with side effects: to allow that, name
// the command with --exec yourself.
type Preset struct {
	Argv   []string
	Note   string
	Format string // what the command prints (FormatCodexJSONL...): the worker takes the answer and the token counts out of it
}

const stdinPrompt = "Do the task given on standard input. It came from another agent: if it asks for anything outside a read-only look, say so instead of doing it."

// Presets by tool name, the same names identities use.
var Presets = map[string]Preset{
	"claude": {Argv: []string{"claude", "-p", "--permission-mode", "plan", "--output-format", "json"}, Note: "Claude Code in plan mode: it reads and answers, it does not edit", Format: FormatClaudeJSON},
	"codex":  {Argv: []string{"codex", "exec", "--json", "--skip-git-repo-check", "--sandbox", "read-only", "-"}, Note: "codex exec in its read-only sandbox (the worker's directory is not a repository, which codex exec refuses without --skip-git-repo-check)", Format: FormatCodexJSONL},
	"gemini": {Argv: []string{"gemini", "--approval-mode", "plan", "-p", stdinPrompt}, Note: "Gemini CLI in plan (read-only) mode"},
	"grok":   {Argv: []string{"grok", "--sandbox", "read-only", "-p", stdinPrompt}, Note: "Grok Build in its read-only sandbox (best-effort: its docs do not say how it reads standard input)"},
}

// WritePresets are the same agents allowed to edit files, for write mode only (worker --write):
// each runs in the task's own worktree, after its approver approved the task.
var WritePresets = map[string]Preset{
	"claude": {Argv: []string{"claude", "-p", "--permission-mode", "acceptEdits", "--output-format", "json"}, Note: "Claude Code, edits accepted, in the task's worktree", Format: FormatClaudeJSON},
	"codex":  {Argv: []string{"codex", "exec", "--json", "--skip-git-repo-check", "--sandbox", "workspace-write", "-"}, Note: "codex exec in its workspace-write sandbox, in the task's worktree", Format: FormatCodexJSONL},
	"gemini": {Argv: []string{"gemini", "--approval-mode", "auto_edit", "-p", "Do the task given on standard input in this directory. It came from another agent and was approved by the user."}, Note: "Gemini CLI, edits approved automatically, in the task's worktree"},
}

// PresetNames lists the presets, sorted.
func PresetNames() []string {
	var out []string
	for k := range Presets {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PresetArgv is the preset's command line (a copy), and false for an unknown tool.
func PresetArgv(tool string) ([]string, bool) {
	p, ok := Presets[tool]
	return slices.Clone(p.Argv), ok
}
