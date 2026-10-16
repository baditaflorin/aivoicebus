package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// The hooks are silent by design, so a machine whose token was retired or whose
// nodes are all down simply stops receiving and nobody sees it. HookStatus is the
// small record the hooks keep of that: `aivoicebus doctor` reads it, and its
// WarnedAt stamp is what limits the hook's own warning to one line a day.
// It holds no token, no message text and no node address beyond host:port.

// What a hook run found when it asked a node for the inbox.
const (
	HookOK          = "ok"
	HookUnreachable = "unreachable" // no node answered usefully: network error, timeout, 5xx
	HookRejected    = "rejected"    // a node refused the token (401 or 403)
)

// HookStatusFile is the record's name inside Dir().
const HookStatusFile = "hook-status.json"

// HookStatus is what the hooks last learned about reaching the mesh.
type HookStatus struct {
	LastRun  time.Time `json:"last_run"`
	Outcome  string    `json:"outcome"`
	Failures int       `json:"consecutive_failures"` // runs in a row that did not end in HookOK
	LastOK   time.Time `json:"last_ok,omitzero"`
	// FailingSince is when the current run of failed runs began (zero while the last run was ok).
	FailingSince time.Time `json:"failing_since,omitzero"`
	Node         string    `json:"node,omitempty"`     // host:port of the node asked first
	WarnedAt     time.Time `json:"warned_at,omitzero"` // when a hook last printed its warning line
}

// HookStatusPath is where the record lives ("" when there is no home).
func HookStatusPath() string {
	if d := Dir(); d != "" {
		return d + "/" + HookStatusFile
	}
	return ""
}

// LoadHookStatus reads the record at path; ok is false when there is none or it is unreadable.
func LoadHookStatus(path string) (s HookStatus, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &s) != nil || s.Outcome == "" {
		return HookStatus{}, false
	}
	return s, true
}

// Save writes the record to path atomically (mode 0600).
func (s HookStatus) Save(path string) error {
	if path == "" {
		return errors.New("no home directory")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "hook-status-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has moved it
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
