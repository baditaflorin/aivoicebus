package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/discovery"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/mcp"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/version"
)

// mcpCmd serves the board as an MCP server on standard input and output, for agent
// tools that speak MCP but have no hook. Each server process is its own session
// named after the tool that runs it (host/codex.<4 chars>, found from its parent processes
// and environment; the same identity as the tool's shell commands when the tool's session id
// reaches both, or when the hook exported one and the MCP server inherited it), else after the
// tool's clientInfo.name, else host/mcp.<4 chars>; AIVOICEBUS_ME or AIVOICEBUS_AGENT say otherwise.
func mcpCmd(args []string, s Streams) error {
	fs := newFlagSet("mcp")
	printConfig := fs.Bool("config", false, "print the settings that register this server with a tool, instead of serving")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if *printConfig {
		exe, err := os.Executable()
		if err != nil {
			exe = "aivoicebus"
		}
		b, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{
			"aivoicebus": map[string]any{"command": exe, "args": []string{"mcp"}},
		}}, "", "  ")
		fmt.Fprintln(s.Out, string(b))
		fmt.Fprintln(s.Err, "Claude Code: claude mcp add aivoicebus -- "+exe+" mcp")
		return nil
	}
	discovery.RefreshIfDue(trustedKeys(), time.Now(), discovery.StartBudget) // an MCP-only tool has no session-start hook to learn new nodes in
	srv := &mcp.Server{Name: "aivoicebus", Version: version.Release, Client: client.FromEnv, Log: s.Err}
	userSet := os.Getenv("AIVOICEBUS_ME") != "" || (os.Getenv("AIVOICEBUS_AGENT") != "" && os.Getenv("AIVOICEBUS_AGENT_SRC") != "hook")
	if !userSet {
		// Explain keeps the hook's identity when it is this tool's own, so the MCP server and the
		// shell commands of one Claude session are one identity; it replaces it only when another
		// tool is plainly running here.
		if d := identity.Explain(); d.Tool != "" || d.Name != "" {
			os.Setenv("AIVOICEBUS_AGENT", d.Agent())
		} else {
			// Unknown tool: a random session until the client says what it is (unless detection is off).
			suffix := msg.NewID()[:4]
			os.Setenv("AIVOICEBUS_AGENT", "mcp."+suffix)
			if !identity.DetectOff() {
				srv.OnClient = func(name string) {
					if tool := identity.ToolFromClient(name); tool != "" {
						os.Setenv("AIVOICEBUS_AGENT", tool+"."+suffix)
					}
				}
			}
		}
	}
	return srv.Serve(s.In, s.Out)
}
