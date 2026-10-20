package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

func TestWhoInboxAndThreadShowTheSessionTitles(t *testing.T) {
	mesh(t)
	t.Setenv(config.TitleEnv, "")
	config.SaveTitle("h/claude.a1b2", "Fix the hub login")
	t.Setenv("AIVOICEBUS_ME", "h/claude.a1b2")
	code, out, e := run(t, "", "send", "--to", "r/codex.q1w2", "--queue", "the login is fixed")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, e)
	}
	id := strings.Fields(out)[1]

	t.Setenv("AIVOICEBUS_ME", "r/codex.q1w2")
	t.Setenv(config.TitleEnv, "Review the parser")
	_, out, _ = run(t, "", "inbox")
	if !strings.Contains(out, `h/claude.a1b2 "Fix the hub login" -> r/codex.q1w2`) {
		t.Errorf("inbox:\n%s", out)
	}
	_, out, _ = run(t, "", "thread", id)
	if !strings.Contains(out, `h/claude.a1b2 "Fix the hub login"`) {
		t.Errorf("thread:\n%s", out)
	}
	_, out, _ = run(t, "", "who")
	if !strings.Contains(out, `"Fix the hub login"`) || !strings.Contains(out, `"Review the parser"`) {
		t.Errorf("who:\n%s", out)
	}
	var w struct {
		Sessions []struct{ Me, Title string }
	}
	_, out, _ = run(t, "", "who", "--json")
	if json.Unmarshal([]byte(out), &w) != nil || len(w.Sessions) < 2 {
		t.Fatalf("%s", out)
	}
	titles := map[string]string{}
	for _, s := range w.Sessions {
		titles[s.Me] = s.Title
	}
	if titles["h/claude.a1b2"] != "Fix the hub login" || titles["r/codex.q1w2"] != "Review the parser" {
		t.Errorf("%v", titles)
	}
	var ms []struct {
		FromTitle string `json:"from_title"`
	}
	_, out, _ = run(t, "", "inbox", "--all", "--json")
	if json.Unmarshal([]byte(out), &ms) != nil || len(ms) != 1 || ms[0].FromTitle != "Fix the hub login" {
		t.Errorf("inbox --json: %s", out)
	}
}
