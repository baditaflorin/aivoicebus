package config

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TitleEnv is the variable that sets a session's title by hand, for tools that do not keep one the
// hook can read; "off" (or "none") sends no title at all, whatever a hook found.
const TitleEnv = "AIVOICEBUS_TITLE"

// titleKeep is how long a session's saved title stays: the cache holds one small file per session
// and nothing reads one after the session has gone.
const titleKeep = 30 * 24 * time.Hour

func titleDir() string {
	d := Dir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "titles")
}

// titleFile names the cache file of one identity: a hash, so an identity can never name a path.
func titleFile(me string) string {
	d := titleDir()
	if d == "" || me == "" {
		return ""
	}
	h := sha256.Sum256([]byte(me))
	return filepath.Join(d, hex.EncodeToString(h[:12]))
}

// Title is the title to declare for the session me: AIVOICEBUS_TITLE when it is set ("off" and
// "none" give ""), else the one the session-start hook last saved for me, else "". The text is the
// caller's to clean (msg.CleanTitle); config knows nothing of the wire.
func Title(me string) string {
	if v := strings.TrimSpace(os.Getenv(TitleEnv)); v != "" {
		switch strings.ToLower(v) {
		case "off", "none":
			return ""
		}
		return v
	}
	p := titleFile(me)
	if p == "" {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 1024))
	return strings.TrimSpace(string(b))
}

// SaveTitle records title as the current title of the session me, privately (a directory only the
// user can read, files mode 0600), replacing the last one, and forgets the titles of sessions not
// seen for a month. An empty title removes the file. Best effort: a title is a convenience.
func SaveTitle(me, title string) {
	p := titleFile(me)
	if p == "" {
		return
	}
	if title == "" {
		_ = os.Remove(p)
		return
	}
	if cur := Title(me); cur == title && os.Getenv(TitleEnv) == "" {
		return // unchanged: the hook runs on every prompt, and a rewrite is a disk write for nothing
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, []byte(title+"\n"), 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
	pruneTitles(filepath.Dir(p), time.Now())
}

func pruneTitles(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) < 32 { // a handful of sessions need no sweep
		return
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > titleKeep {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
