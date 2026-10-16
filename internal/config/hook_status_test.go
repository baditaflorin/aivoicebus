package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookStatusRoundTripsPrivatelyAndAtomically(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := HookStatusPath()
	if !strings.HasSuffix(path, "/.config/aivoicebus/"+HookStatusFile) {
		t.Fatalf("path: %q", path)
	}
	if _, ok := LoadHookStatus(path); ok {
		t.Fatal("no file, no status")
	}
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	want := HookStatus{LastRun: now, Outcome: HookRejected, Failures: 2, LastOK: now.Add(-time.Hour), Node: "n1:7777", WarnedAt: now}
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	got, ok := LoadHookStatus(path)
	if !ok || got != want {
		t.Fatalf("round trip: %+v %v", got, ok)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v", fi.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}

	// zero times are left out, not written as year 1
	if err := (HookStatus{LastRun: now, Outcome: HookOK}).Save(path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "last_ok") || strings.Contains(string(b), "warned_at") {
		t.Errorf("unset times are omitted: %s", b)
	}
}

func TestHookStatusUnreadableMeansNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), HookStatusFile)
	for _, body := range []string{"", "not json", "{}", `{"last_run":"2026-10-14T09:00:00Z"}`} {
		os.WriteFile(path, []byte(body), 0o600)
		if st, ok := LoadHookStatus(path); ok {
			t.Errorf("%q should not load, got %+v", body, st)
		}
	}
}

func TestHookStatusNeedsAHome(t *testing.T) {
	if err := (HookStatus{}).Save(""); err == nil {
		t.Error("saving nowhere must fail")
	}
}
