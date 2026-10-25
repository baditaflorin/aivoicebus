package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

// Automatic update (`aivoicebus update --auto`): off unless the person on this machine turned it
// on, once, at a terminal. The setting is in auto-update, which only that command writes; what the
// last automatic run found and did is in auto-update-state.json, which the run writes (the
// session-start hook starts it). Both are small JSON files read as ReadSmall reads (no symlinks).

const (
	autoFile      = "auto-update"
	autoStateFile = "auto-update-state.json"
	autoLockFile  = "auto-update.lock"
)

// AutoUpdate is the setting: whether this machine installs newer signed releases by itself.
type AutoUpdate struct {
	On bool      `json:"on"`
	At time.Time `json:"at,omitempty"` // when it was last switched
	By string    `json:"by,omitempty"` // who switched it: the identity of that terminal or session
}

// LoadAutoUpdate reads the setting; anything missing, unreadable or malformed is off.
func LoadAutoUpdate() AutoUpdate {
	var a AutoUpdate
	if d := Dir(); d != "" {
		if s, err := ReadSmall(filepath.Join(d, autoFile)); err == nil && json.Unmarshal([]byte(strings.TrimSpace(s)), &a) != nil {
			a = AutoUpdate{}
		}
	}
	return a
}

// SaveAutoUpdate stores the setting (atomically, mode 0600).
func SaveAutoUpdate(a AutoUpdate) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return UpdateSmall(autoFile, func(string) (string, error) { return string(b) + "\n", nil })
}

// What an automatic run did, in AutoState.Result.
const (
	AutoInstalled  = "installed"   // the newer release is installed
	AutoRolledBack = "rolled back" // it was installed, its setup failed, and the previous binary is back
	AutoFailed     = "failed"      // nothing was installed (no release, a refused signature, no network...)
	AutoCurrent    = "up to date"  // the nodes hold nothing newer than what runs here
	AutoSkipped    = "skipped"     // the nodes hold the release the person rolled back from (Skip)
)

// AutoState is what the last automatic run found and did.
type AutoState struct {
	Tried  time.Time `json:"tried"`            // when a run was last started: the hook stamps it before it starts one
	From   string    `json:"from,omitempty"`   // the release that ran here
	To     string    `json:"to,omitempty"`     // the release it tried to install
	Result string    `json:"result,omitempty"` // one of the Auto* results; "" while a run is under way
	Error  string    `json:"error,omitempty"`  // why it failed or was rolled back, one line
	Told   bool      `json:"told,omitempty"`   // a session start has already told the user about Result
	// Skip is the last release rolled back from on this machine (by the person, or by a run whose
	// setup failed): no run installs it, or anything older, by itself.
	Skip string `json:"skip,omitempty"`
}

// Failed reports whether the last run ended without the release it went for.
func (s AutoState) Failed() bool { return s.Result == AutoFailed || s.Result == AutoRolledBack }

// LoadAutoState reads the last run's record; the zero record when there is none.
func LoadAutoState() AutoState {
	var s AutoState
	if d := Dir(); d != "" {
		if t, err := ReadSmall(filepath.Join(d, autoStateFile)); err == nil && json.Unmarshal([]byte(strings.TrimSpace(t)), &s) != nil {
			s = AutoState{}
		}
	}
	return s
}

// EditAutoState changes the record under the settings lock, so two session starts at once cannot
// both find a run due: edit gets the record and reports whether to write the one it changed.
func EditAutoState(edit func(*AutoState) bool) error {
	return UpdateSmall(autoStateFile, func(old string) (string, error) {
		var s AutoState
		if json.Unmarshal([]byte(strings.TrimSpace(old)), &s) != nil {
			s = AutoState{}
		}
		if !edit(&s) {
			return old, nil
		}
		b, err := json.Marshal(s)
		if err != nil {
			return old, err
		}
		return string(b) + "\n", nil
	})
}

// AutoLockPath is the file an automatic run holds locked while it runs (see Lock), so two runs
// never install at once.
func AutoLockPath() string { return filepath.Join(Dir(), autoLockFile) }
