package worker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// The run's sandbox. An agent's own sandbox limits what it writes, not what it reads: Codex in its
// read-only sandbox listed ~/.ssh when asked (found live). A run that a peer's task can talk into
// anything must not read this user's secrets, so the worker runs it inside a profile of its own
// that denies the places secrets live: on macOS with sandbox-exec (it nests with Codex's own), on
// Linux with bubblewrap when installed. Network, the agent's own config (~/.codex, ~/.claude) and
// the rest of the disk stay as they were.

// SecretPaths are the places, under home, a run may not read or write.
func SecretPaths(home string) []string {
	rel := []string{
		".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker/config.json", ".netrc", ".npmrc", ".pypirc",
		".config/gcloud", ".config/gh", ".config/hub", ".git-credentials", ".password-store",
		".config/aivoicebus/token", ".config/aivoicebus/token.nodes", ".config/aivoicebus/machine.key", ".config/aivoicebus/machine.box",
		".config/aivoicebus/approver.key", ".config/aivoicebus/notify-url", ".config/aivoicebus/credentials",
		"Library/Keychains", "Library/Cookies",
		"Library/Application Support/Google/Chrome", "Library/Application Support/Firefox", "Library/Safari",
		".mozilla", ".config/google-chrome", ".config/chromium",
	}
	out := make([]string, len(rel))
	for i, r := range rel {
		out[i] = filepath.Join(home, r)
	}
	return out
}

// Sandboxed wraps argv so it runs without access to SecretPaths, and says how; ok is false when
// this system has no way to (the run then goes as it is, and the caller says so).
func Sandboxed(argv []string, home string) (out []string, how string, ok bool) {
	switch runtime.GOOS {
	case "darwin":
		se, err := exec.LookPath("sandbox-exec")
		if err != nil {
			return argv, "", false
		}
		// sandbox-exec matches the real path: a home reached through a symlink (/var is
		// /private/var on macOS) would otherwise be left open (found by the test).
		if real, err := filepath.EvalSymlinks(home); err == nil {
			home = real
		}
		var deny []string
		for _, p := range SecretPaths(home) {
			if real, err := filepath.EvalSymlinks(p); err == nil {
				p = real
			}
			deny = append(deny, fmt.Sprintf("(subpath %q)", p))
		}
		profile := "(version 1)(allow default)(deny file-read* file-write* " + strings.Join(deny, " ") + ")"
		return append([]string{se, "-p", profile}, argv...), "sandbox-exec: no reading or writing of " + fmt.Sprint(len(deny)) + " secret locations", true
	case "linux":
		bw, err := exec.LookPath("bwrap")
		if err != nil {
			return argv, "", false
		}
		args := []string{bw, "--dev-bind", "/", "/", "--die-with-parent"}
		n := 0
		for _, p := range SecretPaths(home) {
			fi, err := os.Stat(p)
			switch {
			case err != nil:
				continue
			case fi.IsDir():
				args = append(args, "--tmpfs", p)
			default:
				args = append(args, "--ro-bind", "/dev/null", p)
			}
			n++
		}
		return append(args, argv...), fmt.Sprintf("bubblewrap: %d secret locations hidden", n), true
	}
	return argv, "", false
}
