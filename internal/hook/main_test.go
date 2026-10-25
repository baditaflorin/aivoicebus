package hook

import (
	"os"
	"testing"
)

// The tests are about the hook, not about whichever agent tool runs `go test`. No test raises a real
// notification or starts a real update run (the test binary is not aivoicebus): those that look at
// either put their own in.
func TestMain(m *testing.M) {
	os.Setenv("AIVOICEBUS_DETECT", "off")
	notifyPerson = func(int) {}
	startAuto = func() error { return nil }
	os.Exit(m.Run())
}
