package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

// transcript writes a Claude Code transcript under HOME/.claude with the given lines.
func transcript(t *testing.T, lines ...string) string {
	t.Helper()
	h, _ := os.UserHomeDir()
	p := filepath.Join(h, ".claude", "projects", "-work-repo", "11112222-3333.jsonl")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	return p
}

func titleRecord(title, sid string) string {
	b, _ := json.Marshal(map[string]string{"type": "custom-title", "customTitle": title, "sessionId": sid})
	return string(b)
}

func TestTheLastTitleInTheTranscriptIsTheSessionsTitle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := transcript(t,
		`{"type":"user","message":{"content":"a \"type\":\"custom-title\" inside a prompt is not a record"}}`,
		titleRecord("First name", "s1"),
		`{"type":"assistant","message":{"content":"working"}}`,
		titleRecord("Fix the hub\nlogin", "s1"),
		`{"type":"user","message":{"content":"more"}}`)
	if got := transcriptTitle(p, "s1", 4<<20); got != "Fix the hub login" {
		t.Errorf("%q", got)
	}
	if got := transcriptTitle(p, "other-session", 4<<20); got != "" {
		t.Errorf("another session's title was taken: %q", got)
	}
	if got := transcriptTitle(p, "", 4<<20); got != "Fix the hub login" {
		t.Errorf("no session id to compare: %q", got)
	}
}

func TestATitleFarBackIsFoundWithinTheLargerBudgetOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	filler := `{"type":"assistant","message":{"content":"` + strings.Repeat("x", 1000) + `"}}`
	lines := []string{titleRecord("Deep title", "s1")}
	for i := 0; i < 600; i++ { // about 600 KB after the title
		lines = append(lines, filler)
	}
	p := transcript(t, lines...)
	if got := transcriptTitle(p, "s1", 256<<10); got != "" {
		t.Errorf("the per-prompt budget reads only the last 256 KB: %q", got)
	}
	if got := transcriptTitle(p, "s1", 4<<20); got != "Deep title" {
		t.Errorf("%q", got)
	}
}

// The path comes from the hook's input: only a regular file under the tool's own directory is read.
func TestOnlyATranscriptUnderTheToolsDirectoryIsRead(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	outside := filepath.Join(h, "secrets.jsonl")
	os.WriteFile(outside, []byte(titleRecord("stolen", "")+"\n"), 0o600)
	inside := transcript(t, titleRecord("ok", ""))
	link := filepath.Join(filepath.Dir(inside), "link.jsonl")
	os.Symlink(outside, link)
	for _, p := range []string{outside, link, filepath.Join(filepath.Dir(inside), "..", "..", "..", "secrets.jsonl"), "relative.jsonl", ""} {
		if got := transcriptTitle(p, "", 4<<20); got != "" {
			t.Errorf("%q was read: %q", p, got)
		}
	}
	if got := transcriptTitle(inside, "", 4<<20); got != "ok" {
		t.Errorf("%q", got)
	}
}

func TestTheSessionStartHookDeclaresAndSavesTheTitle(t *testing.T) {
	s, _ := mesh(t)
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_TITLE", "")
	t.Setenv("CLAUDE_ENV_FILE", filepath.Join(t.TempDir(), "env"))
	p := transcript(t, titleRecord("Fix the hub login", "a1b2c3"))
	in, _ := json.Marshal(map[string]string{"session_id": "a1b2c3", "cwd": t.TempDir(), "transcript_path": p})
	var out strings.Builder
	Run([]string{"--tool", "claude"}, strings.NewReader(string(in)), &out)

	me := ""
	for _, e := range s.Roster() {
		if strings.HasSuffix(e.Me, "/claude.a1b2") {
			me = e.Me
			if e.Title != "Fix the hub login" {
				t.Errorf("roster title %q", e.Title)
			}
		}
	}
	if me == "" {
		t.Fatalf("the session is not in the roster: %+v", s.Roster())
	}
	if got := config.Title(me); got != "Fix the hub login" {
		t.Errorf("saved for later commands: %q", got)
	}
}

func TestTitlesCanBeTurnedOff(t *testing.T) {
	s, _ := mesh(t)
	t.Setenv("AIVOICEBUS_ME", "h/claude.a1b2")
	t.Setenv("AIVOICEBUS_TITLE", "off")
	p := transcript(t, titleRecord("Private plans", ""))
	in, _ := json.Marshal(map[string]string{"session_id": "a1b2", "transcript_path": p})
	var out strings.Builder
	Run([]string{"--tool", "claude"}, strings.NewReader(string(in)), &out)
	for _, e := range s.Roster() {
		if e.Title != "" {
			t.Errorf("a title went out though titles are off: %+v", e)
		}
	}
	if config.Title("h/claude.a1b2") != "" {
		t.Error("a title was saved though titles are off")
	}
}

func TestAntigravityHookDeclaresAndSavesAnnotationTitle(t *testing.T) {
	s, _ := mesh(t)
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_TITLE", "")
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "")
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", "")
	h, _ := os.UserHomeDir()
	convID := "f40f54b5-5b43-4d64-9db6-3457637698f3"
	annDir := filepath.Join(h, ".gemini", "antigravity", "annotations")
	os.MkdirAll(annDir, 0o700)
	os.WriteFile(filepath.Join(annDir, convID+".pbtxt"), []byte(`title:"AI Voice Bus Setup" last_user_view_time:{seconds:1}`+"\n"), 0o600)

	in, _ := json.Marshal(map[string]any{
		"conversationId": convID,
		"workspacePaths": []string{t.TempDir()},
		"modelName":      "gemini-3-pro",
	})
	var out strings.Builder
	Run([]string{"--tool", "gemini"}, strings.NewReader(string(in)), &out)

	me := ""
	for _, e := range s.Roster() {
		if strings.HasSuffix(e.Me, "/gemini.f40f") {
			me = e.Me
			if e.Title != "AI Voice Bus Setup" {
				t.Errorf("roster title %q", e.Title)
			}
		}
	}
	if me == "" {
		t.Fatalf("the session is not in the roster: %+v", s.Roster())
	}
	if got := config.Title(me); got != "AI Voice Bus Setup" {
		t.Errorf("saved for later commands: %q", got)
	}
}
