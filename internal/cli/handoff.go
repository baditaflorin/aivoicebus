package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// handoffCmd hands work to another agent, of any make, as a task whose text says everything it
// needs to carry on: the goal, where the code is (repo, branch, commit, whether that commit is
// pushed), the uncommitted changes as an attached diff, and what "done" means.
func handoffCmd(args []string, s Streams) error {
	usage := outbox.Usagef(`usage: handoff [MACHINE] "goal" [--done "what done means"] [--diff] [--tool codex] [--can X] [--wait-reply 10m]`)
	var done string
	diff := false
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		key, val, hasEq := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch {
		case strings.HasPrefix(a, "-") && key == "done":
			if !hasEq {
				if i+1 >= len(args) {
					return usage
				}
				i++
				val = args[i]
			}
			done = val
		case strings.HasPrefix(a, "-") && key == "diff":
			diff = true
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		return usage
	}
	// The goal is the last plain word (not a flag, not a flag's value); task takes the rest.
	valued := map[string]bool{"ttl": true, "wait-reply": true, "via": true, "prio": true, "tool": true, "can": true, "model": true}
	goalIdx := -1
	for i := 0; i < len(rest); i++ {
		if strings.HasPrefix(rest[i], "-") {
			if key, _, hasEq := strings.Cut(strings.TrimLeft(rest[i], "-"), "="); valued[key] && !hasEq {
				i++
			}
			continue
		}
		goalIdx = i
	}
	if goalIdx < 0 {
		return usage
	}
	g := gitState(".")
	var b strings.Builder
	fmt.Fprintf(&b, "[handoff] %s\n", strings.TrimSpace(rest[goalIdx]))
	if g.repo != "" {
		fmt.Fprintf(&b, "repo: %s\n", g.repo)
		fmt.Fprintf(&b, "branch: %s at %s %q\n", g.branch, g.commit, g.subject)
		switch {
		case g.unpushed < 0:
			fmt.Fprintf(&b, "pushed: unknown (no upstream for %s): push it, or the receiver cannot see it\n", g.branch)
		case g.unpushed > 0:
			fmt.Fprintf(&b, "pushed: NO, %d commit(s) not on the remote yet: the receiver cannot see them until they are pushed\n", g.unpushed)
		default:
			b.WriteString("pushed: yes\n")
		}
		fmt.Fprintf(&b, "start: git fetch origin && git checkout %s && git pull --ff-only\n", g.branch)
	}
	tmpDir := ""
	if diff && g.repo != "" {
		d, err := gitOut(".", "diff", "HEAD")
		switch {
		case err != nil:
			return fmt.Errorf("git diff: %v", err)
		case strings.TrimSpace(d) == "":
			b.WriteString("uncommitted: none\n")
		default:
			tmpDir, err = os.MkdirTemp("", "aivoicebus-handoff-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmpDir)
			f := filepath.Join(tmpDir, "handoff-"+strings.NewReplacer("/", "-", " ", "-").Replace(g.branch)+".diff")
			if err := os.WriteFile(f, []byte(d), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(&b, "uncommitted: %d file(s) changed, attached as a diff (untracked files are not in it): `aivoicebus get ID --out %s` then `git apply %s`\n", g.dirty, filepath.Base(f), filepath.Base(f))
			rest = append(rest, "--file", f)
		}
	} else if g.dirty > 0 {
		fmt.Fprintf(&b, "uncommitted: %d file(s) changed on the sender's machine and NOT sent (--diff sends them)\n", g.dirty)
	}
	if done != "" {
		fmt.Fprintf(&b, "done when: %s\n", strings.TrimSpace(done))
	}
	b.WriteString("Claim it (aivoicebus claim ID), and answer in the thread when it is done or blocked.")
	rest[goalIdx] = b.String()
	return taskCmd(rest, s)
}

type gitInfo struct {
	repo, branch, commit, subject string
	unpushed                      int // -1: no upstream
	dirty                         int
}

func gitOut(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// gitState describes the repository dir is in; empty when it is not one.
func gitState(dir string) gitInfo {
	var g gitInfo
	if out, err := gitOut(dir, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return g
	}
	origin, _ := gitOut(dir, "remote", "get-url", "origin")
	g.repo = cleanRemote(strings.TrimSpace(origin))
	if g.repo == "" {
		top, _ := gitOut(dir, "rev-parse", "--show-toplevel")
		g.repo = filepath.Base(strings.TrimSpace(top)) + " (no origin remote)"
	}
	b, _ := gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD")
	g.branch = strings.TrimSpace(b)
	c, _ := gitOut(dir, "log", "-1", "--format=%h%x00%s")
	g.commit, g.subject, _ = strings.Cut(strings.TrimSpace(c), "\x00")
	if len(g.subject) > 80 {
		g.subject = g.subject[:80] + "…"
	}
	g.unpushed = -1
	if n, err := gitOut(dir, "rev-list", "--count", "@{upstream}..HEAD"); err == nil {
		fmt.Sscanf(strings.TrimSpace(n), "%d", &g.unpushed)
	}
	st, _ := gitOut(dir, "status", "--porcelain")
	for _, l := range strings.Split(st, "\n") {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "??") {
			g.dirty++
		}
	}
	return g
}

// cleanRemote is a remote URL without any credentials in it (https://user:token@host/... would
// otherwise travel in the message).
func cleanRemote(u string) string {
	if u == "" {
		return ""
	}
	if p, err := url.Parse(u); err == nil && p.Scheme != "" && p.Host != "" {
		p.User = nil
		p.RawQuery, p.Fragment = "", ""
		return p.String()
	}
	if at := strings.Index(u, "@"); at >= 0 && strings.Contains(u[at:], ":") { // git@host:owner/repo.git
		return u
	}
	return u
}
