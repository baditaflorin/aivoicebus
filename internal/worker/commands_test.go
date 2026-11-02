package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every command in the repo's commands/ catalog parses as a spec, names only files it ships, and has a README.
func TestTheCommandCatalogParses(t *testing.T) {
	dirs, err := filepath.Glob("../../commands/*/command.json")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no commands found in ../../commands: %v", err)
	}
	for _, f := range dirs {
		dir := filepath.Dir(f)
		name := filepath.Base(dir)
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sp, err := parseSpec(name, []byte(strings.ReplaceAll(string(b), "{{DIR}}", "/home/u/.config/aivoicebus/run.d")))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if sp.Limit() <= 0 || sp.Limit() > MaxRunTimeout {
			t.Errorf("%s: timeout %v", name, sp.Limit())
		}
		if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
			t.Errorf("%s: no README.md", name)
		}
		if files, err := os.ReadFile(filepath.Join(dir, "FILES")); err == nil {
			for _, line := range strings.Fields(string(files)) {
				if strings.ContainsAny(line, "/") || strings.HasPrefix(line, ".") {
					t.Errorf("%s: FILES names %q: a plain file name next to command.json", name, line)
				} else if _, err := os.Stat(filepath.Join(dir, line)); err != nil {
					t.Errorf("%s: FILES names %s, which is missing", name, line)
				}
			}
		}
	}
}
