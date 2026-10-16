package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestReadSmallRefusesLinksHugeFilesAndNonFiles(t *testing.T) {
	dir := t.TempDir()
	if text, err := ReadSmall(filepath.Join(dir, "absent")); text != "" || err != nil {
		t.Errorf("absent is empty: %q %v", text, err)
	}
	ok := filepath.Join(dir, "ok")
	os.WriteFile(ok, []byte("a=b\n"), 0o600)
	if text, err := ReadSmall(ok); text != "a=b\n" || err != nil {
		t.Errorf("%q %v", text, err)
	}
	if err := os.Symlink(ok, filepath.Join(dir, "link")); err == nil {
		if _, err := ReadSmall(filepath.Join(dir, "link")); err == nil {
			t.Error("a symlink is refused")
		}
	}
	if _, err := ReadSmall(dir); err == nil {
		t.Error("a directory is refused")
	}
	big := filepath.Join(dir, "big")
	os.WriteFile(big, []byte(strings.Repeat("x", MaxSmallFile+1)), 0o600)
	if _, err := ReadSmall(big); err == nil {
		t.Error("over the cap is refused")
	}
	edge := filepath.Join(dir, "edge")
	os.WriteFile(edge, []byte(strings.Repeat("x", MaxSmallFile)), 0o600)
	if text, err := ReadSmall(edge); err != nil || len(text) != MaxSmallFile {
		t.Errorf("exactly the cap is fine: %v", err)
	}
}

func TestUpdateSmallIsAtomicPrivateAndSerialised(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := UpdateSmall("thing", func(old string) (string, error) { return old + "x\n", nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	path := home + "/.config/aivoicebus/thing"
	b, _ := os.ReadFile(path)
	if fi, _ := os.Stat(path); strings.Count(string(b), "x\n") != 20 || fi.Mode().Perm() != 0o600 {
		t.Errorf("no update lost, mode 0600: %d %v", strings.Count(string(b), "x\n"), fi.Mode())
	}
	if matches, _ := filepath.Glob(home + "/.config/aivoicebus/thing-*.tmp"); len(matches) != 0 {
		t.Errorf("temp files left: %v", matches)
	}
	// a link in the way is refused, and nothing is written through it
	os.Remove(path)
	target := home + "/elsewhere"
	os.WriteFile(target, []byte("keep\n"), 0o600)
	if err := os.Symlink(target, path); err == nil {
		if err := UpdateSmall("thing", func(old string) (string, error) { return old + "y\n", nil }); err == nil {
			t.Error("a symlink is refused")
		}
		if b, _ := os.ReadFile(target); string(b) != "keep\n" {
			t.Errorf("written through the link: %q", b)
		}
	}
}
