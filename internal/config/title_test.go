package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func titleHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv(TitleEnv, "")
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "")
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", "")
	return h
}

func TestTitleIsSavedPerSessionPrivatelyAndReadBack(t *testing.T) {
	h := titleHome(t)
	if got := Title("h/claude.a1b2"); got != "" {
		t.Fatalf("no title yet: %q", got)
	}
	SaveTitle("h/claude.a1b2", "Fix the hub login")
	SaveTitle("h/claude.c3d4", "Other session")
	if Title("h/claude.a1b2") != "Fix the hub login" || Title("h/claude.c3d4") != "Other session" {
		t.Errorf("%q %q", Title("h/claude.a1b2"), Title("h/claude.c3d4"))
	}
	dir := filepath.Join(h, ".config", "aivoicebus", "titles")
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the cache directory must be 0700: %v %v", fi, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "claude") || strings.Contains(e.Name(), "/") {
			t.Errorf("an identity must not name a file: %s", e.Name())
		}
		if info, _ := e.Info(); info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v, want 0600", e.Name(), info.Mode().Perm())
		}
	}
	SaveTitle("h/claude.a1b2", "")
	if Title("h/claude.a1b2") != "" {
		t.Error("an empty title removes the saved one")
	}
}

func TestTheTitleEnvironmentVariableWinsAndCanTurnTitlesOff(t *testing.T) {
	titleHome(t)
	SaveTitle("h/codex.q1w2", "saved title")
	t.Setenv(TitleEnv, "Set by hand")
	if got := Title("h/codex.q1w2"); got != "Set by hand" {
		t.Errorf("%q", got)
	}
	for _, off := range []string{"off", "OFF", "none"} {
		t.Setenv(TitleEnv, off)
		if got := Title("h/codex.q1w2"); got != "" {
			t.Errorf("%s: %q", off, got)
		}
	}
}

func TestOldTitleFilesAreSweptWhenTheCacheGrows(t *testing.T) {
	h := titleHome(t)
	dir := filepath.Join(h, ".config", "aivoicebus", "titles")
	os.MkdirAll(dir, 0o700)
	old := time.Now().Add(-2 * titleKeep)
	for i := 0; i < 40; i++ {
		p := filepath.Join(dir, strings.Repeat("a", 23)+string(rune('a'+i%26))+string(rune('a'+i/26)))
		os.WriteFile(p, []byte("x\n"), 0o600)
		os.Chtimes(p, old, old)
	}
	SaveTitle("h/claude.a1b2", "fresh")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("%d files left, want only the fresh one", len(entries))
	}
}

func TestAnUnsetHomeSavesNothingAndDoesNotPanic(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv(TitleEnv, "")
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "")
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", "")
	SaveTitle("h/a", "x")
	_ = Title("h/a")
}

func TestAntigravityTitleFromAnnotations(t *testing.T) {
	h := titleHome(t)
	convID := "f40f54b5-5b43-4d64-9db6-3457637698f3"
	annDir := filepath.Join(h, ".gemini", "antigravity", "annotations")
	os.MkdirAll(annDir, 0o700)
	pbtxt := `title:"AI Voice Bus \"Mesh\" Setup" last_user_view_time:{seconds:1792358966 nanos:443000000}` + "\n"
	os.WriteFile(filepath.Join(annDir, convID+".pbtxt"), []byte(pbtxt), 0o600)

	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
	if got := Title("h/gemini.f40f"); got != `AI Voice Bus "Mesh" Setup` {
		t.Errorf("gemini.f40f title = %q", got)
	}
	if got := Title("h/gemini"); got != `AI Voice Bus "Mesh" Setup` {
		t.Errorf("bare gemini title = %q", got)
	}
	// Another session or tool on the same machine must not inherit this conversation's title.
	if got := Title("h/gemini.aaaa"); got != "" {
		t.Errorf("different gemini session got title %q", got)
	}
	if got := Title("h/claude.f40f"); got != "" {
		t.Errorf("different tool got title %q", got)
	}
	// AIVOICEBUS_TITLE=off still suppresses it.
	t.Setenv(TitleEnv, "off")
	if got := Title("h/gemini.f40f"); got != "" {
		t.Errorf("Title with off = %q", got)
	}
	t.Setenv(TitleEnv, "")
	// Path traversal or symlinks in conversation ID are rejected.
	if got := AntigravityTitle("../etc/passwd"); got != "" {
		t.Errorf("traversal ID accepted: %q", got)
	}
}
