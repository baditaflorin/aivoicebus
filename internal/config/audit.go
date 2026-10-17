package config

import (
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The audit log: what was allowed, run, stopped or released on this machine, one JSON line each
// in audit.jsonl (mode 0600). Every line carries the SHA-256 of the line before it and a signature
// by the machine key over itself, so an edited, reordered, inserted or forged line shows up in
// `aivoicebus audit --verify`. Cutting lines off the end does not (a file cannot prove its own
// length): keep a copy elsewhere if that matters.

const auditFile = "audit.jsonl"

// AuditEntry is one line.
type AuditEntry struct {
	Seq    int       `json:"seq"`
	At     time.Time `json:"at"`
	Who    string    `json:"who"`
	Action string    `json:"action"`
	Detail string    `json:"detail,omitempty"`
	Prev   string    `json:"prev"`          // sha256 of the previous line ("" for the first)
	Sig    string    `json:"sig,omitempty"` // machine key, over the line without sig
}

var auditMu sync.Mutex

// Audit appends one entry. Best effort: an audit write that fails does not stop the action, and
// says so on stderr.
func Audit(who, action, detail string) {
	auditMu.Lock()
	defer auditMu.Unlock()
	if os.MkdirAll(Dir(), 0o700) != nil {
		return
	}
	p := filepath.Join(Dir(), auditFile)
	lines := readAuditLines(p)
	e := AuditEntry{Seq: len(lines) + 1, At: time.Now().UTC(), Who: who, Action: action, Detail: detail}
	if n := len(lines); n > 0 {
		sum := sha256.Sum256([]byte(lines[n-1]))
		e.Prev = hex.EncodeToString(sum[:])
	}
	if priv, _, err := MachineKeys(); err == nil {
		unsigned, _ := json.Marshal(e)
		e.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, unsigned))
	}
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aivoicebus: warning: the audit log could not be written:", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

func readAuditLines(p string) []string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			out = append(out, sc.Text())
		}
	}
	return out
}

// ReadAudit returns the entries and, for each line that does not hold, why (line number, 1-based).
func ReadAudit() ([]AuditEntry, map[int]string) {
	lines := readAuditLines(filepath.Join(Dir(), auditFile))
	var out []AuditEntry
	bad := map[int]string{}
	var pub ed25519.PublicKey
	if priv, _, err := MachineKeys(); err == nil {
		pub = priv.Public().(ed25519.PublicKey)
	}
	for i, l := range lines {
		var e AuditEntry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			bad[i+1] = "not an audit line"
			continue
		}
		out = append(out, e)
		if e.Seq != i+1 {
			bad[i+1] = fmt.Sprintf("sequence %d where %d was due (a line removed or inserted before it)", e.Seq, i+1)
		}
		want := ""
		if i > 0 {
			sum := sha256.Sum256([]byte(lines[i-1]))
			want = hex.EncodeToString(sum[:])
		}
		if e.Prev != want {
			bad[i+1] = "it does not follow the line before it (that line was changed, or this one moved)"
		}
		sig, _ := base64.StdEncoding.DecodeString(e.Sig)
		e.Sig = ""
		unsigned, _ := json.Marshal(e)
		if pub == nil || !ed25519.Verify(pub, unsigned, sig) {
			bad[i+1] = "its signature by this machine's key does not hold (the line was changed)"
		}
	}
	return out, bad
}
