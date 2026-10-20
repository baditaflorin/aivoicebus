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

// task --needs repo:X goes to the session that has repo X checked out, and to no other; with nobody
// that has everything, nothing is sent and the closest sessions say what they lack.
func TestTaskNeedsPicksTheSessionOnTheRepo(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	t.Setenv("AIVOICEBUS_CAPS", "")
	session(t, "mac/claude.need1", "repo=go-common,origin=baditaflorin/go-common,branch=main,tool=claude")
	run(t, "", "inbox")
	session(t, "air/claude.need2", "repo=web,origin=baditaflorin/web,branch=main,tool=claude")
	run(t, "", "inbox") // read last: the most recent, so a pick by recency alone would take it

	session(t, "boss/claude.need0", "")
	code, out, e := run(t, "", "task", "--needs", "repo:baditaflorin/go-common", "bump the logger in go-common")
	if code != 0 {
		t.Fatalf("task --needs: %d\n%s\n%s", code, out, e)
	}
	if !strings.Contains(e, "--needs: mac/claude.need1: has repo:baditaflorin/go-common") {
		t.Errorf("the pick is explained in one line:\n%s", e)
	}

	session(t, "mac/claude.need1", "repo=go-common,origin=baditaflorin/go-common,branch=main,tool=claude")
	if _, out, _ = run(t, "", "inbox"); !strings.Contains(out, "bump the logger in go-common") || !strings.Contains(out, "-> mac/claude.need1") {
		t.Errorf("the go-common session gets it:\n%s", out)
	}
	session(t, "air/claude.need2", "repo=web,origin=baditaflorin/web,branch=main,tool=claude")
	if _, out, _ = run(t, "", "inbox"); strings.Contains(out, "bump the logger") {
		t.Errorf("the web session must not:\n%s", out)
	}

	session(t, "boss/claude.need0", "")
	code, out, e = run(t, "", "task", "--needs", "repo:baditaflorin/go-common,gpu", "train it")
	if code != exitNotFound || !strings.Contains(e, "nothing was sent") ||
		!strings.Contains(e, "mac/claude.need1 (seen ") || !strings.Contains(e, "lacks gpu") ||
		!strings.Contains(e, "air/claude.need2 (seen ") || !strings.Contains(e, "lacks repo:baditaflorin/go-common,gpu") {
		t.Errorf("refused with the closest: %d\n%s\n%s", code, out, e)
	}
	if strings.Index(e, "mac/claude.need1") > strings.Index(e, "air/claude.need2") {
		t.Errorf("the one lacking less comes first:\n%s", e)
	}
	_, out, _ = run(t, "", "ask", "--needs", "gpu", "--json", "which gpu?")
	var res struct{ Outcome string }
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Outcome != "nobody" {
		t.Errorf("ask --needs --json with nobody: %v\n%s", err, out)
	}
	if code, _, e = run(t, "", "task", "--needs", "gpu", "--any", "x"); code != exitUsage || !strings.Contains(e, "leave out --any") {
		t.Errorf("--needs picks itself: %d %s", code, e)
	}
	if code, _, e = run(t, "", "task", "--needs", "colour:blue", "x"); code != exitUsage || !strings.Contains(e, "not a capability kind") {
		t.Errorf("a bad need: %d %s", code, e)
	}
}
