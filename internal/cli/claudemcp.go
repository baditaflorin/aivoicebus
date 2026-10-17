package cli

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/agentcfg"
)

// claudeMCP registers the MCP server with Claude Code through its own command, which Grok Build
// reads too. AIVOICEBUS_NO_CLAUDE_MCP skips it (tests, or a machine where it is not wanted).
func claudeMCP(bin string, dryRun bool) (string, string) {
	claude, ok := claudeCommand()
	if !ok {
		return agentcfg.Skipped, claude
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, claude, "mcp", "get", "aivoicebus").Run() == nil {
		return agentcfg.OK, "registered with Claude Code (claude mcp get aivoicebus); Grok Build reads it too"
	}
	if dryRun {
		return agentcfg.Changed, "claude mcp add --scope user aivoicebus -- " + bin + " mcp"
	}
	out, err := exec.CommandContext(ctx, claude, "mcp", "add", "--scope", "user", "aivoicebus", "--", bin, "mcp").CombinedOutput()
	if err != nil {
		return agentcfg.Failed, "claude mcp add failed: " + strings.TrimSpace(firstLine(string(out)))
	}
	return agentcfg.Changed, "claude mcp add --scope user aivoicebus (Grok Build reads it too)"
}

func claudeMCPRemove(dryRun bool) (string, string) {
	claude, ok := claudeCommand()
	if !ok {
		return agentcfg.Skipped, claude
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, claude, "mcp", "get", "aivoicebus").Run() != nil {
		return agentcfg.OK, "not registered with Claude Code"
	}
	if dryRun {
		return agentcfg.Changed, "claude mcp remove --scope user aivoicebus"
	}
	if out, err := exec.CommandContext(ctx, claude, "mcp", "remove", "--scope", "user", "aivoicebus").CombinedOutput(); err != nil {
		return agentcfg.Failed, "claude mcp remove failed: " + strings.TrimSpace(firstLine(string(out)))
	}
	return agentcfg.Changed, "claude mcp remove --scope user aivoicebus"
}

// claudeCommand finds the claude command; when there is none, the second result is false and
// the first says why.
func claudeCommand() (string, bool) {
	if os.Getenv("AIVOICEBUS_NO_CLAUDE_MCP") != "" {
		return "AIVOICEBUS_NO_CLAUDE_MCP is set", false
	}
	p, err := exec.LookPath("claude")
	if err != nil {
		return "no claude command on PATH (Grok Build reads Claude Code's MCP servers, so it has none either)", false
	}
	return p, true
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
