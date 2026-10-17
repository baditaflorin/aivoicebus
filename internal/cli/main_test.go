package cli

import (
	"os"
	"testing"
)

// These tests are about aivoicebus, not about whichever agent tool runs `go test`: without this a
// run inside Claude Code or Codex would see claude.xxxx where the tests expect "agent".
func TestMain(m *testing.M) {
	os.Setenv("AIVOICEBUS_DETECT", "off")
	// Never upload to the public pipe from a test: a test that sends a file sets up its own.
	os.Setenv("AIVOICEBUS_PIPE_URL", "off")
	os.Exit(m.Run())
}
