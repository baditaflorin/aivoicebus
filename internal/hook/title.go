package hook

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Claude Code appends a {"type":"custom-title","customTitle":"...","sessionId":"..."} record to a
// session's transcript each time the session's title is set or changes, so the last one is the
// current title. The hook reads only that record, from the end of the file.
const customTitleMark = `"type":"custom-title"`

// tailSizes are how much of the transcript's end is read, smallest first: the title is usually
// within the last few hundred kilobytes, and the per-prompt hook has a budget of a fraction of a second.
var tailSizes = []int64{256 << 10, 1 << 20, 4 << 20}

// transcriptDirOK says whether path may be read: a regular file under the tool's own directory
// (~/.claude, or CLAUDE_CONFIG_DIR). The path comes from the hook's input, so it is not trusted to
// name some other file.
func transcriptDirOK(path string) bool {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	roots := []string{}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		roots = append(roots, d)
	}
	if h, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(h, ".claude"))
	}
	for _, r := range roots {
		if rel, err := filepath.Rel(r, path); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			fi, err := os.Lstat(path)
			return err == nil && fi.Mode().IsRegular()
		}
	}
	return false
}

// transcriptTitle is the title Claude Code last gave the session (cleaned), "" when there is none
// or the transcript cannot be read. sessionID, when both it and the record carry one, must agree.
func transcriptTitle(path, sessionID string, maxTail int64) string {
	if !transcriptDirOK(path) {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	for _, size := range tailSizes {
		if size > maxTail && size != tailSizes[0] {
			break
		}
		start := max(fi.Size()-size, 0)
		buf := make([]byte, fi.Size()-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return ""
		}
		if t, found := lastTitle(buf, start > 0, sessionID); found {
			return t
		}
		if start == 0 {
			break
		}
	}
	return ""
}

// lastTitle finds the last custom-title record in buf. When buf starts mid-file, its first line is
// partial and is skipped.
func lastTitle(buf []byte, partialFirst bool, sessionID string) (string, bool) {
	if partialFirst {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			return "", false
		}
	}
	for end := len(buf); end > 0; {
		i := bytes.LastIndex(buf[:end], []byte(customTitleMark))
		if i < 0 {
			return "", false
		}
		ls := bytes.LastIndexByte(buf[:i], '\n') + 1
		le := bytes.IndexByte(buf[i:], '\n')
		if le < 0 {
			le = len(buf)
		} else {
			le += i
		}
		var rec struct {
			Type      string `json:"type"`
			Title     string `json:"customTitle"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(buf[ls:le], &rec) == nil && rec.Type == "custom-title" &&
			(sessionID == "" || rec.SessionID == "" || rec.SessionID == sessionID) {
			return msg.CleanTitle(rec.Title), true
		}
		end = ls
	}
	return "", false
}

// sessionTitle settles this session's title for the hook: AIVOICEBUS_TITLE when set, else what the
// transcript or Antigravity annotation says now (saved for the commands the agent runs later, which
// cannot see the transcript), else what was saved before. It returns "" when the session has none
// or opted out.
func sessionTitle(in hookInput, me string, maxTail int64) string {
	if os.Getenv(config.TitleEnv) != "" {
		return msg.CleanTitle(config.Title(me))
	}
	if t := transcriptTitle(in.TranscriptPath, in.SessionID, maxTail); t != "" {
		config.SaveTitle(me, t)
		return t
	}
	if t := msg.CleanTitle(config.AntigravityTitle(in.SessionID)); t != "" {
		config.SaveTitle(me, t)
		return t
	}
	return msg.CleanTitle(config.Title(me))
}

// assistantMark finds the records that carry the model: Claude Code writes the id of the model that
// answered on every assistant record ({"type":"assistant","message":{"model":"claude-opus-5-5",...}}),
// so the last one is the model the session runs now (it can change mid-session).
const assistantMark = `"type":"assistant"`

// transcriptModel is the model of the last assistant record in the transcript (cleaned), "" when none
// is found within maxTail. Placeholders such as "<synthetic>" are skipped.
func transcriptModel(path, sessionID string, maxTail int64) string {
	if !transcriptDirOK(path) {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	for _, size := range tailSizes {
		if size > maxTail && size != tailSizes[0] {
			break
		}
		start := max(fi.Size()-size, 0)
		buf := make([]byte, fi.Size()-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return ""
		}
		if m, found := lastModel(buf, start > 0, sessionID); found {
			return m
		}
		if start == 0 {
			break
		}
	}
	return ""
}

func lastModel(buf []byte, partialFirst bool, sessionID string) (string, bool) {
	if partialFirst {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return "", false
		}
		buf = buf[i+1:]
	}
	for end := len(buf); end > 0; {
		i := bytes.LastIndex(buf[:end], []byte(assistantMark))
		if i < 0 {
			return "", false
		}
		ls := bytes.LastIndexByte(buf[:i], '\n') + 1
		le := bytes.IndexByte(buf[i:], '\n')
		if le < 0 {
			le = len(buf)
		} else {
			le += i
		}
		var rec struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
			Message   struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(buf[ls:le], &rec) == nil && rec.Type == "assistant" &&
			(sessionID == "" || rec.SessionID == "" || rec.SessionID == sessionID) {
			if m := msg.CleanModel(rec.Message.Model); m != "" {
				return m, true
			}
		}
		end = ls
	}
	return "", false
}

// sessionModel is sessionTitle for the model: AIVOICEBUS_MODEL when set, else the transcript's latest
// (saved for later commands, which declare it as the model label), else what was saved before.
func sessionModel(in hookInput, me string, maxTail int64) string {
	if os.Getenv(config.ModelEnv) != "" {
		return msg.CleanModel(config.Model(me))
	}
	if m := msg.CleanModel(in.Model); m != "" { // the tool said so (Codex does, in the hook's input)
		config.SaveModel(me, m)
		return m
	}
	if m := transcriptModel(in.TranscriptPath, in.SessionID, maxTail); m != "" {
		config.SaveModel(me, m)
		return m
	}
	return msg.CleanModel(config.Model(me))
}
