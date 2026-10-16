package hook

import (
	"os"
	"testing"
)

// The tests are about the hook, not about whichever agent tool runs `go test`.
func TestMain(m *testing.M) {
	os.Setenv("AIVOICEBUS_DETECT", "off")
	os.Exit(m.Run())
}
