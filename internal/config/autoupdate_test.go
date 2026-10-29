package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Off unless written on: a missing, malformed or planted (symlinked) setting is off.
func TestAutoUpdateIsOffUnlessWrittenOn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if LoadAutoUpdate().On {
		t.Fatal("on with no setting")
	}
	at := time.Date(2026, 10, 25, 20, 0, 0, 0, time.UTC)
	if err := SaveAutoUpdate(AutoUpdate{On: true, At: at, By: "air/human"}); err != nil {
		t.Fatal(err)
	}
	if a := LoadAutoUpdate(); !a.On || !a.At.Equal(at) || a.By != "air/human" {
		t.Fatalf("round trip: %+v", a)
	}
	p := filepath.Join(Dir(), autoFile)
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the setting's mode: %v %v", fi.Mode(), err)
	}
	os.WriteFile(p, []byte("on\n"), 0o600)
	if LoadAutoUpdate().On {
		t.Error("a malformed setting is off")
	}
	target := filepath.Join(home, "elsewhere")
	os.WriteFile(target, []byte(`{"on":true}`), 0o600)
	os.Remove(p)
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	if LoadAutoUpdate().On {
		t.Error("a symlinked setting is off")
	}
}

// The record of the last run survives edits, keeps what an edit leaves alone, and says which results failed.
func TestAutoStateEditsAndFailures(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if s := LoadAutoState(); !s.Tried.IsZero() || s.Result != "" {
		t.Fatalf("a fresh record: %+v", s)
	}
	now := time.Date(2026, 10, 25, 20, 0, 0, 0, time.UTC)
	if err := EditAutoState(func(s *AutoState) bool { s.Tried, s.To, s.Skip = now, "0.14.0", "0.13.9"; return true }); err != nil {
		t.Fatal(err)
	}
	if err := EditAutoState(func(s *AutoState) bool { s.Result = AutoRolledBack; return true }); err != nil {
		t.Fatal(err)
	}
	_ = EditAutoState(func(s *AutoState) bool { s.Result = "never written"; return false })
	s := LoadAutoState()
	if !s.Tried.Equal(now) || s.To != "0.14.0" || s.Skip != "0.13.9" || s.Result != AutoRolledBack {
		t.Fatalf("after the edits: %+v", s)
	}
	for r, want := range map[string]bool{AutoFailed: true, AutoRolledBack: true, AutoInstalled: false, AutoCurrent: false, AutoSkipped: false, "": false} {
		if got := (AutoState{Result: r}).Failed(); got != want {
			t.Errorf("Failed() for %q: %v, want %v", r, got, want)
		}
	}
}
