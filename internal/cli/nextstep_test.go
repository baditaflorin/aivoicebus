package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
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

// A withdrawn or expired task is not claimed: the session is told so, and to ask for a fresh one.
func TestClaimOfAWithdrawnOrExpiredTaskAsksForAFreshOne(t *testing.T) {
	s := meshStore(t)
	gone := sendTask(t, "imac/claude.1", "air", "one to withdraw")
	if code, _, e := run(t, "", "retract", gone); code != 0 {
		t.Fatalf("retract: %d %q", code, e)
	}
	t.Setenv("AIVOICEBUS_ME", "air/claude.2")
	code, out, e := run(t, "", "claim", gone)
	if code == 0 || !strings.Contains(e, "withdrawn") || !strings.Contains(e, "ask the sender for a fresh one") || strings.Contains(out, "claimed") {
		t.Errorf("a withdrawn task: %d %q %q", code, out, e)
	}
	s.SetClock(func() time.Time { return time.Now().Add(-2 * time.Hour) }) // the node thinks it is still alive
	t.Setenv("AIVOICEBUS_ME", "imac/claude.1")
	code, out, e = run(t, "", "task", "air", "one that runs out", "--ttl", "1m")
	if code != 0 {
		t.Fatalf("task: %d %q", code, e)
	}
	old, _ := sentID(t, out)
	t.Setenv("AIVOICEBUS_ME", "air/claude.2")
	code, out, e = run(t, "", "claim", old)
	if code == 0 || !strings.Contains(e, "expired") || !strings.Contains(e, "ask the sender for a fresh one") {
		t.Errorf("an expired task: %d %q %q", code, out, e)
	}
	for _, m := range s.Thread(old) {
		if m.Kind == msg.KindClaim {
			t.Errorf("a claim was sent for an expired task: %+v", m)
		}
	}
}
