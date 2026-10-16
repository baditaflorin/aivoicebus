package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
)

func assistant(model, sid string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "sessionId": sid, "message": map[string]any{"model": model, "content": "x"}})
	return string(b)
}

func TestTheLastRealModelInTheTranscriptIsTheSessionsModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := transcript(t,
		assistant("claude-sonnet-5-5", "s1"),
		`{"type":"user","message":{"content":"/model opus"}}`,
		assistant("claude-opus-5-5", "s1"),
		assistant("<synthetic>", "s1"), // not a model
		`{"type":"user","message":{"content":"a \"type\":\"assistant\" inside a prompt is not a record"}}`)
	if got := transcriptModel(p, "s1", 4<<20); got != "claude-opus-5-5" {
		t.Errorf("%q", got)
	}
	if got := transcriptModel(p, "another", 4<<20); got != "" {
		t.Errorf("another session's model: %q", got)
	}
	if got := transcriptModel(filepath.Join(t.TempDir(), "x.jsonl"), "", 4<<20); got != "" {
		t.Errorf("a file outside ~/.claude was read: %q", got)
	}
}

func TestTheHookDeclaresTheModelAsALabelButDoesNotPinIt(t *testing.T) {
	s, _ := mesh(t)
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Setenv(config.ModelEnv, "")
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	p := transcript(t, assistant("claude-opus-5-5", "a1b2c3"))
	in, _ := json.Marshal(map[string]string{"session_id": "a1b2c3", "cwd": t.TempDir(), "transcript_path": p})
	var out strings.Builder
	Run([]string{"--tool", "claude"}, strings.NewReader(string(in)), &out)
	found := false
	for _, e := range s.Roster() {
		if strings.HasSuffix(e.Me, "/claude.a1b2") {
			found = e.Labels["model"] == "claude-opus-5-5"
		}
	}
	if !found {
		t.Errorf("the roster does not have the model: %+v", s.Roster())
	}
	b, _ := os.ReadFile(envFile)
	if strings.Contains(string(b), "model=") {
		t.Errorf("the model was pinned in the session's environment; it can change: %s", b)
	}
	// a later command of the session declares it too, from the saved value
	t.Setenv("AIVOICEBUS_AGENT", "claude.a1b2")
	if got := identity.Labels(t.TempDir())["model"]; got != "claude-opus-5-5" {
		t.Errorf("a later command's labels: %q", got)
	}
}

func TestTheModelCanBeSetByHandOrTurnedOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_ME", "h/codex.q1w2")
	p := transcript(t, assistant("claude-opus-5-5", ""))
	t.Setenv(config.ModelEnv, "gpt-5.1-codex")
	if got := sessionModel(hookInput{TranscriptPath: p}, "h/codex.q1w2", 4<<20); got != "gpt-5.1-codex" {
		t.Errorf("set by hand: %q", got)
	}
	t.Setenv(config.ModelEnv, "off")
	if got := sessionModel(hookInput{TranscriptPath: p}, "h/codex.q1w2", 4<<20); got != "" {
		t.Errorf("off: %q", got)
	}
	if got := identity.Labels(t.TempDir())["model"]; got != "" {
		t.Errorf("off, in the labels: %q", got)
	}
}
