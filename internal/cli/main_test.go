package cli

import (
	"os"
	"testing"
)

// These tests are about aivoicebus, not about whichever agent tool runs `go test`: without this a
// run inside Claude Code or Codex would see claude.xxxx where the tests expect "agent".
func TestMain(m *testing.M) {
	os.Setenv("AIVOICEBUS_DETECT", "off")
	os.Exit(m.Run())
}
