package config

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// TitleEnv is the variable that sets a session's title by hand, for tools that do not keep one the
// hook can read; "off" (or "none") sends no title at all, whatever a hook found.
const TitleEnv = "AIVOICEBUS_TITLE"

// titleKeep is how long a session's saved title stays: the cache holds one small file per session
// and nothing reads one after the session has gone.
const titleKeep = 30 * 24 * time.Hour

// ModelEnv sets a session's model by hand, for tools whose model the hook cannot read; "off" (or
// "none") declares none.
const ModelEnv = "AIVOICEBUS_MODEL"

// sessionFile names the cache file of one identity in the cache kind ("titles", "models"): a hash,
// so an identity can never name a path.
func sessionFile(kind, me string) string {
	d := Dir()
	if d == "" || me == "" {
		return ""
	}
	h := sha256.Sum256([]byte(me))
	return filepath.Join(d, kind, hex.EncodeToString(h[:12]))
}

func titleFile(me string) string { return sessionFile("titles", me) }

// Title is the title to declare for the session me: AIVOICEBUS_TITLE when it is set ("off" and
// "none" give ""), else the one the session-start hook last saved for me, else the Antigravity
// conversation's annotation title when me is the current Antigravity session, else "". The text is
// the caller's to clean (msg.CleanTitle); config knows nothing of the wire.
func Title(me string) string {
	if v := strings.TrimSpace(os.Getenv(TitleEnv)); v != "" {
		switch strings.ToLower(v) {
		case "off", "none":
			return ""
		}
		return v
	}
	if v := sessionValue("", "titles", me); v != "" {
		return v
	}
	return antigravityTitleFor(me)
}

// AntigravityTitle reads the title Google Antigravity (Gemini.app / agy) stores for conversation
// convID in $ANTIGRAVITY_APP_DATA_DIR/annotations/<convID>.pbtxt (or ~/.gemini/antigravity/annotations).
// It returns "" when convID is not a clean filename or the annotation has no title.
func AntigravityTitle(convID string) string {
	if !cleanAnnotationID(convID) {
		return ""
	}
	appDir := strings.TrimSpace(os.Getenv("ANTIGRAVITY_APP_DATA_DIR"))
	if appDir == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return ""
		}
		appDir = filepath.Join(h, ".gemini", "antigravity")
	}
	if !filepath.IsAbs(appDir) || filepath.Clean(appDir) != appDir {
		return ""
	}
	p := filepath.Join(appDir, "annotations", convID+".pbtxt")
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 16<<10 {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return parsePbtxtTitle(string(b))
}

// antigravityTitleFor returns the current Antigravity conversation's title when ANTIGRAVITY_CONVERSATION_ID
// is set and me belongs to that conversation (or is bare /gemini on this machine).
func antigravityTitleFor(me string) string {
	convID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	if convID == "" || !matchesAntigravitySession(me, convID) {
		return ""
	}
	return AntigravityTitle(convID)
}

func matchesAntigravitySession(me, convID string) bool {
	_, agent, ok := strings.Cut(strings.ToLower(me), "/")
	if !ok {
		agent = strings.ToLower(me)
	}
	if agent == "gemini" {
		return true
	}
	if suf, ok := strings.CutPrefix(agent, "gemini."); ok && suf != "" {
		return suf == shortAlnum4(convID)
	}
	return false
}

func shortAlnum4(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			if b.Len() == 4 {
				break
			}
		}
	}
	return b.String()
}

func cleanAnnotationID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// parsePbtxtTitle extracts the string literal after `title:` in an Antigravity annotation .pbtxt
// file (e.g. `title:"AI Voice Bus Setup" last_user_view_time:{...}`).
func parsePbtxtTitle(s string) string {
	for len(s) > 0 {
		i := strings.Index(s, "title:")
		if i < 0 {
			return ""
		}
		if i == 0 || s[i-1] == ' ' || s[i-1] == '\n' || s[i-1] == '\t' || s[i-1] == '\r' {
			rest := strings.TrimLeft(s[i+len("title:"):], " \t")
			if len(rest) > 0 && rest[0] == '"' {
				for j := 1; j < len(rest); j++ {
					if rest[j] == '\\' {
						j++
						continue
					}
					if rest[j] == '"' {
						if v, err := strconv.Unquote(rest[:j+1]); err == nil {
							return strings.TrimSpace(v)
						}
						break
					}
				}
			}
		}
		s = s[i+len("title:"):]
	}
	return ""
}

// Model is Title for the model the session runs: AIVOICEBUS_MODEL, else what a hook saved for me.
// The caller cleans it (msg.CleanModel).
func Model(me string) string { return sessionValue(ModelEnv, "models", me) }

// Rooms is the '+'-joined list of rooms the session me is in (`aivoicebus room join`), "" for none.
func Rooms(me string) string { return sessionValue("AIVOICEBUS_ROOMS", "rooms", me) }

// SaveRooms records the rooms the session me is in ("" for none).
func SaveRooms(me, rooms string) { saveSessionValue("AIVOICEBUS_ROOMS", "rooms", me, rooms) }

// SaveModel is SaveTitle for the model.
func SaveModel(me, model string) { saveSessionValue(ModelEnv, "models", me, model) }

func sessionValue(env, kind, me string) string {
	if env != "" {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			switch strings.ToLower(v) {
			case "off", "none":
				return ""
			}
			return v
		}
	}
	p := sessionFile(kind, me)
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
func SaveTitle(me, title string) { saveSessionValue(TitleEnv, "titles", me, title) }

func saveSessionValue(env, kind, me, title string) {
	p := sessionFile(kind, me)
	if p == "" {
		return
	}
	if title == "" {
		_ = os.Remove(p)
		return
	}
	if cur := sessionValue(env, kind, me); cur == title && os.Getenv(env) == "" {
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
