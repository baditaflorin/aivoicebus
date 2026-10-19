package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// A usage error that does not say how to use the command is printed with where its flags are
// listed; one that does is printed as it is, and other errors are left to say their own.
func TestUsageErrorsSayWhereFlagsAre(t *testing.T) {
	bare := outbox.Usagef("--for is 1m to 168h")
	got := withNextStep(bare, "pause")
	if !errors.Is(got, outbox.ErrUsage) || got.Error() != "--for is 1m to 168h (`aivoicebus pause -h` lists its flags)" || !view.HasNextStep(got.Error()) {
		t.Fatalf("%v", got)
	}
	wrapped := fmt.Errorf("lease: %w", outbox.Usagef("one key"))
	if got := withNextStep(wrapped, "lease"); !strings.HasSuffix(got.Error(), "(`aivoicebus lease -h` lists its flags)") {
		t.Fatalf("%v", got)
	}
	for _, err := range []error{outbox.Usagef("usage: held"), outbox.Usagef("x: run `aivoicebus who`"), errors.New("other")} {
		if got := withNextStep(err, "x"); got != err {
			t.Fatalf("%v became %v", err, got)
		}
	}
	// and through Run: a bad flag value, and an unknown command
	code, _, e := run(t, "", "tokens", "--since", "soon")
	if code != exitUsage || !strings.Contains(e, "`aivoicebus tokens -h` lists its flags") {
		t.Fatalf("%d %q", code, e)
	}
	if _, _, e := run(t, "", "frobnicate"); !strings.Contains(e, "`aivoicebus help` lists the commands") {
		t.Fatalf("%q", e)
	}
}
