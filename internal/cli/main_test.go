package cli

import (
	"os"
	"testing"
)

// These tests are about aivoicebus, not about whichever agent tool runs `go test`: without this a
// run inside Claude Code or Codex would see claude.xxxx where the tests expect "agent".
func TestMain(m *testing.M) {
	// The test binary is also the CLI, for tests that need two machines at once (each its own
	// process, with its own home and identity): see asCLI.
	if os.Getenv("AIVOICEBUS_TEST_AS_CLI") == "1" {
		os.Exit(Run(os.Args[1:], Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}))
	}
	os.Setenv("AIVOICEBUS_DETECT", "off")
	// Never upload to the public pipe from a test: a test that sends a file sets up its own.
	os.Setenv("AIVOICEBUS_PIPE_URL", "off")
	// Never run the real `claude mcp add` from a test.
	os.Setenv("AIVOICEBUS_NO_CLAUDE_MCP", "1")
	os.Exit(m.Run())
}
