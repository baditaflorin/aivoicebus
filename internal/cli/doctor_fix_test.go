package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// brokenMachine is a home whose aivoicebus footprint has drifted every way setup can mend, next to
// content of the person's own that no repair may touch.
func brokenMachine(t *testing.T) (home string) {
	t.Helper()
	home, _ = bareMachine(t)
	t.Setenv("AIVOICEBUS_URL", "http://node1:7777")
	w := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	w(".claude/CLAUDE.md", "# my rules\nalways be kind\n\n<!-- aivoicebus:begin id=00000000 -->\nold snippet\n<!-- aivoicebus:end -->\n\n## mine too\nkeep me\n", 0o644)
	w(".claude/settings.json", `{"model":"opus","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo mine"},{"type":"command","command":"/old/aivoicebus hook"}]}]}}`, 0o644)
	w(".config/aivoicebus/token", goodToken+"\n", 0o644)
	return home
}

func TestDoctorFixRepairsDriftKeepsUserContentAndIsIdempotent(t *testing.T) {
	home := brokenMachine(t)
	code, out, e := run(t, "", "doctor", "--fix", "--offline")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, e)
	}
	for _, want := range []string{"changed      install", "changed      claude hook", "changed      claude instructions", "0600"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	md, _ := os.ReadFile(filepath.Join(home, ".claude/CLAUDE.md"))
	for _, keep := range []string{"# my rules\nalways be kind", "## mine too\nkeep me"} {
		if !strings.Contains(string(md), keep) {
			t.Errorf("user text %q lost:\n%s", keep, md)
		}
	}
	if strings.Contains(string(md), "old snippet") {
		t.Errorf("the stale managed block survived:\n%s", md)
	}
	st, _ := os.ReadFile(filepath.Join(home, ".claude/settings.json"))
	for _, keep := range []string{`"model": "opus"`, "echo mine"} {
		if !strings.Contains(string(st), keep) {
			t.Errorf("user setting %q lost:\n%s", keep, st)
		}
	}
	if strings.Contains(string(st), "/old/aivoicebus") {
		t.Errorf("the old hook form survived:\n%s", st)
	}
	if fi, _ := os.Stat(filepath.Join(home, ".config/aivoicebus/token")); fi.Mode().Perm() != 0o600 {
		t.Errorf("token mode %v", fi.Mode().Perm())
	}

	// the second run has nothing left to do, and changes no byte
	before := snapshot(t, home)
	code, out, _ = run(t, "", "doctor", "--fix", "--offline")
	if code != 0 || strings.Contains(out, "changed ") {
		t.Errorf("second run: %d\n%s", code, out)
	}
	if after := snapshot(t, home); after != before {
		t.Errorf("an idempotent run changed files:\n%s\n--\n%s", before, after)
	}
}

func TestDoctorFixNeverReplacesAnInstalledBinary(t *testing.T) {
	home := brokenMachine(t)
	bin := filepath.Join(home, ".local/bin/aivoicebus")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte("the installed one"), 0o755)
	if code, out, _ := run(t, "", "doctor", "--fix", "--offline"); code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	if got, _ := os.ReadFile(bin); string(got) != "the installed one" {
		t.Errorf("an installed binary was replaced: %q", got)
	}
}

func TestDoctorFixDryRunShowsThePlanAndWritesNothing(t *testing.T) {
	home := brokenMachine(t)
	before := snapshot(t, home)
	code, out, e := run(t, "", "doctor", "--fix", "--dry-run")
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	for _, want := range []string{"would change install", "would change claude hook", "would change token"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if after := snapshot(t, home); after != before {
		t.Errorf("dry run changed files:\n%s\n--\n%s", before, after)
	}
	if code, _, e := run(t, "", "doctor", "--dry-run"); code != 1 || !strings.Contains(e, "--dry-run goes with --fix") {
		t.Errorf("--dry-run alone: %d %q", code, e)
	}
}

func TestDoctorFixRefusesToRewriteAnInstructionFileItCannotReadSafely(t *testing.T) {
	cases := map[string]string{
		"begin marker without an end":     "# mine\n<!-- aivoicebus:begin id=0000 -->\nmy own words after the marker\n",
		"old section the person added to": "## aivoicebus: old\n\nold paragraph\n\nmy own second paragraph\n\n## other\nx\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			home := brokenMachine(t)
			md := filepath.Join(home, ".claude/CLAUDE.md")
			os.WriteFile(md, []byte(content), 0o644)
			code, out, _ := run(t, "", "doctor", "--fix", "--offline")
			if code != 1 || !strings.Contains(out, "refusing to rewrite "+md) {
				t.Errorf("%d\n%s", code, out)
			}
			if got, _ := os.ReadFile(md); string(got) != content {
				t.Errorf("the file was changed:\n%s", got)
			}
		})
	}
}

func TestDoctorFixDryRunReportsAFailingStep(t *testing.T) {
	home := brokenMachine(t)
	os.WriteFile(filepath.Join(home, ".claude/settings.json"), []byte("{not json"), 0o644)
	if code, out, _ := run(t, "", "doctor", "--fix", "--dry-run"); code != 1 || !strings.Contains(out, "failed") {
		t.Errorf("%d\n%s", code, out)
	}
}

// snapshot is every file under dir with its mode and content, as one string.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range tree(t, dir) {
		fi, _ := os.Stat(p)
		c, _ := os.ReadFile(p)
		b.WriteString(p + " " + fi.Mode().String() + "\n" + string(c) + "\n")
	}
	return b.String()
}
