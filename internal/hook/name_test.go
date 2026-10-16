package hook

import (
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/identity"
)

// A registered long-lived name replaces the per-session suffix for the hook's own tool, and for
// no other tool's session.
func TestTheHookWearsARegisteredLongLivedName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := identity.AddName("codex.build1"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIVOICEBUS_NAME", "codex.build1")
	if got := sessionAgent("codex", "0199aaaabbbb"); got != "codex.build1" {
		t.Errorf("%q", got)
	}
	if got := sessionAgent("claude", "9058aa53-5065"); got != "claude.9058" {
		t.Errorf("a Codex name must not label a Claude session: %q", got)
	}
	t.Setenv("AIVOICEBUS_NAME", "codex.other99") // not registered here
	if got := sessionAgent("codex", "0199aaaabbbb"); got == "codex.other99" {
		t.Errorf("an unregistered name is not used: %q", got)
	}
}
