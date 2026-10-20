package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// Capabilities need no setup: a session that only read its inbox shows, in who, the repository
// it has checked out (owner/name from its origin), its branch, its tool and what it declared.
func TestWhoShowsWhatEachSessionCanDo(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	t.Setenv("AIVOICEBUS_CAPS", "prod")
	session(t, "mac/claude.caps1", "repo=go-common,origin=baditaflorin/go-common,branch=main,tool=claude")
	run(t, "", "inbox")
	t.Setenv("AIVOICEBUS_CAPS", "")
	session(t, "air/codex.caps2", "repo=web,tool=codex")
	run(t, "", "inbox")

	_, out, _ := run(t, "", "who")
	for _, want := range []string{
		"can: prod repo:baditaflorin/go-common branch:main tool:claude",
		"can: repo:web tool:codex",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("who should show %q:\n%s", want, out)
		}
	}
	_, out, _ = run(t, "", "who", "--json")
	var who struct {
		Sessions []struct {
			Me   string
			Caps []string
		}
	}
	if err := json.Unmarshal([]byte(out), &who); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, s := range who.Sessions {
		if s.Me == "mac/claude.caps1" && !slices.Contains(s.Caps, "repo:baditaflorin/go-common") {
			t.Errorf("who --json caps: %v", s.Caps)
		}
	}
}
