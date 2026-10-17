package config

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// What aivoicebus puts into agents' context on this machine: every hook output, MCP tool result
// and agent-facing CLI read is one line of usage.jsonl (mode 0600; at most usageKeep bytes, then
// usage.jsonl.1), so `aivoicebus usage` can say what the bus costs a session. Counts only: never
// the text.

const (
	usageFile = "usage.jsonl"
	usageKeep = 2 << 20
)

// UsageLine is one record: who got how many characters, from what ("hook:start", "mcp:aivoicebus_wait", "cli:inbox").
type UsageLine struct {
	At    time.Time `json:"at"`
	Me    string    `json:"me"`
	What  string    `json:"what"`
	Chars int       `json:"chars"`
}

// NoteUsage records that what gave me chars characters of context. Best effort: a failure to
// write is not the caller's problem.
func NoteUsage(me, what string, chars int) {
	if chars <= 0 || os.Getenv("AIVOICEBUS_USAGE") == "off" {
		return
	}
	dir := Dir()
	if dir == "" || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	p := filepath.Join(dir, usageFile)
	if fi, err := os.Stat(p); err == nil && fi.Size() > usageKeep {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(UsageLine{At: time.Now().UTC(), Me: me, What: what, Chars: chars})
	_, _ = f.Write(append(b, '\n'))
}

// ReadUsage returns the records since since, the rotated file's first.
func ReadUsage(since time.Time) []UsageLine {
	var out []UsageLine
	for _, name := range []string{usageFile + ".1", usageFile} {
		f, err := os.Open(filepath.Join(Dir(), name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var u UsageLine
			if json.Unmarshal(sc.Bytes(), &u) == nil && !u.At.Before(since) {
				out = append(out, u)
			}
		}
		f.Close()
	}
	return out
}
