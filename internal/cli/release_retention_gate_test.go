package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type retentionReleaseFixture struct{ root, out, commit, tree, binarySHA, pack, network string }

func newRetentionReleaseFixture(t *testing.T, indexed bool) *retentionReleaseFixture {
	t.Helper()
	base := t.TempDir()
	f := &retentionReleaseFixture{root: filepath.Join(base, "source"), out: filepath.Join(base, "release"), pack: filepath.Join(base, "pack-called"), network: filepath.Join(base, "network-called")}
	for _, p := range []string{f.root, f.out, filepath.Join(f.root, "scripts")} {
		if e := os.MkdirAll(p, 0700); e != nil {
			t.Fatal(e)
		}
	}
	for _, name := range []string{"release.sh", "compat-record.sh"} {
		b, e := os.ReadFile(filepath.Join("..", "..", "scripts", name))
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(f.root, "scripts", name), b, 0755); e != nil {
			t.Fatal(e)
		}
	}
	for name, text := range map[string]string{"VERSION": "0.15.2\n", "scripts/compat-releases.txt": "0.15.1\n"} {
		if e := os.WriteFile(filepath.Join(f.root, name), []byte(text), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if indexed {
		p := filepath.Join(f.root, "internal", "store")
		if e := os.MkdirAll(p, 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(p, "lease_history.go"), []byte("package store\n"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", f.root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("fixture git %v: %v %s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-q")
	git("add", ".")
	git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "synthetic release gate")
	f.commit = git("rev-parse", "HEAD")
	f.tree = git("rev-parse", "HEAD^{tree}")
	binary := []byte(fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = version ]; then printf 'aivoicebus 0.15.2 (commit %s, fixture)\\n'; elif [ \"$1 $2\" = 'release pack' ]; then : >\"$GATE_PACK_MARKER\"; elif [ \"$1 $2\" = 'release verify' ]; then exit 0; else exit 2; fi\n", f.commit))
	for _, name := range []string{"aivoicebus-linux-amd64", "aivoicebus-linux-arm64", "aivoicebus-darwin-amd64", "aivoicebus-darwin-arm64"} {
		if e := os.WriteFile(filepath.Join(f.out, name), binary, 0755); e != nil {
			t.Fatal(e)
		}
	}
	f.binarySHA = fmt.Sprintf("%x", sha256.Sum256(binary))
	f.writeRecords(t)
	return f
}
func (f *retentionReleaseFixture) writeRecords(t *testing.T) {
	t.Helper()
	base := fmt.Sprintf("commit=%s\ntree=%s\nbinary_sha256=%s\nresult=pass\nchecks=16\nscenarios=10\nold=0.15.1\n", f.commit, f.tree, f.binarySHA)
	if e := os.WriteFile(filepath.Join(f.out, "compat-pass.txt"), []byte(base), 0600); e != nil {
		t.Fatal(e)
	}
	on := "format=aivoicebus-compat-on-v1\nmode=on\n" + base + "aged_managed=64\nactive_managed=1\nordinary=16\n"
	for _, key := range []string{"old_readonly_exports", "raw_log_preserved", "vv_preserved", "no_tombstone_or_quarantine", "by_id_truth", "live_lease_renew_release", "strict_signed_new_writes", "rollback_old_new_on"} {
		on += key + "=pass\n"
	}
	if e := os.WriteFile(filepath.Join(f.out, "compat-on-pass.txt"), []byte(on), 0600); e != nil {
		t.Fatal(e)
	}
}
func (f *retentionReleaseFixture) run(t *testing.T, stage string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(f.root, "scripts", "release.sh"), stage, f.out)
	cmd.Dir = f.root
	cmd.Env = append(os.Environ(), "AIVOICEBUS_SKIP_COMPAT=", "COMPAT_RECORD=", "COMPAT_ON_RECORD=", "GATE_PACK_MARKER="+f.pack, "GATE_NETWORK_MARKER="+f.network)
	cmd.Env = append(cmd.Env, env...)
	b, e := cmd.CombinedOutput()
	return string(b), e
}
func TestReleaseRetentionGateRequiresCurrentOffAndOnProofs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("canonical release route uses Bash")
	}
	cases := []struct {
		name, old, new              string
		removeOn, removeOff, waiver bool
	}{
		{name: "missing", removeOn: true}, {name: "wrong-mode", old: "mode=on", new: "mode=off"}, {name: "stale-source", old: "commit=", new: "commit=000"}, {name: "wrong-tree", old: "tree=", new: "tree=000"}, {name: "wrong-binary", old: "binary_sha256=", new: "binary_sha256=000"}, {name: "duplicate-mode", old: "mode=on", new: "mode=on\nmode=off"}, {name: "missing-assertion", old: "by_id_truth=pass", new: "by_id_truth=fail"}, {name: "missing-old-version", old: "old=0.15.1", new: "old="}, {name: "short-run", old: "scenarios=10", new: "scenarios=9"}, {name: "missing-off", removeOff: true}, {name: "waiver", waiver: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRetentionReleaseFixture(t, true)
			path := filepath.Join(f.out, "compat-on-pass.txt")
			if c.removeOn {
				if e := os.Remove(path); e != nil {
					t.Fatal(e)
				}
			} else if c.old != "" {
				b, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(path, []byte(strings.Replace(string(b), c.old, c.new, 1)), 0600); e != nil {
					t.Fatal(e)
				}
			}
			if c.removeOff {
				if e := os.Remove(filepath.Join(f.out, "compat-pass.txt")); e != nil {
					t.Fatal(e)
				}
			}
			var env []string
			if c.waiver {
				env = []string{"AIVOICEBUS_SKIP_COMPAT=this must not bypass the indexed release gate"}
			}
			output, e := f.run(t, "sign", env...)
			if e == nil {
				t.Fatalf("invalid proof reached signing: %s", output)
			}
			if _, e = os.Stat(f.pack); !os.IsNotExist(e) {
				t.Fatalf("pack executed despite invalid proof: %v %s", e, output)
			}
		})
	}
	t.Run("complete-both", func(t *testing.T) {
		f := newRetentionReleaseFixture(t, true)
		if b, e := f.run(t, "sign"); e != nil {
			t.Fatalf("complete proofs refused: %v %s", e, b)
		}
		if _, e := os.Stat(f.pack); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("legacy-source-unchanged", func(t *testing.T) {
		f := newRetentionReleaseFixture(t, false)
		if e := os.Remove(filepath.Join(f.out, "compat-on-pass.txt")); e != nil {
			t.Fatal(e)
		}
		if b, e := f.run(t, "sign"); e != nil {
			t.Fatalf("legacy source acquired an On requirement: %v %s", e, b)
		}
	})
}
func TestReleaseRetentionPublicationRefusesMissingOnProof(t *testing.T) {
	f := newRetentionReleaseFixture(t, true)
	if e := os.Remove(filepath.Join(f.out, "compat-on-pass.txt")); e != nil {
		t.Fatal(e)
	}
	for name, text := range map[string]string{"manifest.json": fmt.Sprintf("{\n  \"version\": \"0.15.2\",\n  \"commit\": \"%s\"\n}\n", f.commit), "manifest.sig": "synthetic-not-a-signature", "SHA256SUMS": "synthetic"} {
		if e := os.WriteFile(filepath.Join(f.out, name), []byte(text), 0600); e != nil {
			t.Fatal(e)
		}
	}
	notes := filepath.Join(f.out, "NOTES")
	if e := os.WriteFile(notes, []byte("synthetic fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	bin := filepath.Join(f.root, "fakebin")
	if e := os.Mkdir(bin, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\n: >\"$GATE_NETWORK_MARKER\"\nexit 0\n"), 0755); e != nil {
		t.Fatal(e)
	}
	for _, stage := range []string{"github", "attach"} {
		output, e := f.run(t, stage, "NOTES="+notes, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		if e == nil {
			t.Fatalf("%s published without On proof: %s", stage, output)
		}
		if _, e = os.Stat(f.network); !os.IsNotExist(e) {
			t.Fatalf("%s reached gh despite missing On proof: %v %s", stage, e, output)
		}
	}
}
