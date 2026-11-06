package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReproducePreparesPrivateVerifiedCacheThenBuildsOffline(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "download-failure"}[fail], func(t *testing.T) {
			dir := t.TempDir()
			trace := filepath.Join(dir, "calls")
			fake := `#!/bin/sh
printf '%s\n' "$*" >> "$TRACE"
cache= out= src=
for arg do
 case "$arg" in
  *:/gomodcache) cache=${arg%:/gomodcache} ;;
  *:/out) out=${arg%:/out} ;;
  *:/src:ro) src=${arg%:/src:ro} ;;
 esac
done
if [ -n "$cache" ]; then
 test -f "$src/go.mod" && test -f "$src/go.sum" || exit 43
 test -z "$FAIL_PREP" || exit 42
elif [ -n "$out" ]; then
 printf identical > "$out/aivoicebus-linux-amd64"
fi
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fake), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "scripts/reproduce.sh", "HEAD")
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "TRACE="+trace, "FAIL_PREP=")
			if fail {
				cmd.Env = append(cmd.Env, "FAIL_PREP=1")
			}
			out, err := cmd.CombinedOutput()
			if fail {
				if err == nil || !strings.Contains(string(out), "dependency preparation failed") {
					t.Fatalf("preparation failure was ignored: %v %s", err, out)
				}
			} else if err != nil {
				t.Fatalf("reproduction: %v %s", err, out)
			}
			raw, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
			want := 3
			if fail {
				want = 1
			}
			if len(calls) != want {
				t.Fatalf("docker calls=%d want=%d: %s", len(calls), want, raw)
			}
			prep := calls[0]
			for _, required := range []string{":/src:ro", ":/gomodcache -w /src", "GOMODCACHE=/gomodcache", "-mod=readonly", "go mod download all; go mod verify"} {
				if !strings.Contains(prep, required) {
					t.Errorf("preparation missing %q: %s", required, prep)
				}
			}
			if strings.Contains(prep, "--network none") {
				t.Fatal("dependency preparation cannot download")
			}
			for _, call := range calls[1:] {
				for _, required := range []string{"--network none", ":/gomodcache:ro", "GOPROXY=off", "GOSUMDB=off", "GOMODCACHE=/gomodcache", "-mod=readonly"} {
					if !strings.Contains(call, required) {
						t.Errorf("offline build missing %q: %s", required, call)
					}
				}
			}
			for _, arg := range strings.Fields(prep) {
				if strings.HasSuffix(arg, ":/gomodcache") {
					cache := strings.TrimSuffix(arg, ":/gomodcache")
					if _, err := os.Stat(cache); !os.IsNotExist(err) {
						t.Fatalf("owned module cache was not cleaned: %v", err)
					}
				}
			}
		})
	}
}
