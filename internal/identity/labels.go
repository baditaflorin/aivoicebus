package identity

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Labels describes this session for selector routing (`--to @repo=x`):
//
//	caps     what the session offers, from AIVOICEBUS_CAPS    ask+run   (read on every call, never pinned)
//	machine  the host part of the identity        laptop-a
//	tool     the tool part of the agent           claude
//	host     the plain hostname                   laptop        (not when AIVOICEBUS_ME overrides the identity)
//	chip     the Mac's chip                       m1pro
//	product  the Mac's product                    mbp
//	repo     the git repository of cwd (origin)   go-url-categorizer-api
//	origin   its owner/name when the remote has one  example-org/go-url-categorizer-api
//	branch   its checked-out branch               feature/x
//	role     the roles it claims (AIVOICEBUS_ROLE) build+review  (a send to @build finds it)
//
// cwd is the directory the session works in ("" means the current one). A
// AIVOICEBUS_LABELS="key=value,..." in the environment is laid over everything
// derived: the session-start hook exports the full set once, so later commands
// in the session agree with it even if they run from another directory. One
// thing is not pinned: when cwd is in the same repository the session started in,
// the branch is read again, so switching branches mid-session is followed. A
// command run from another repository never rewrites the session's repo or branch.
func Labels(cwd string) msg.Labels {
	l := msg.Labels{}
	host, agent, _ := strings.Cut(Me(), "/")
	if v := msg.CleanValue(host); v != "" {
		l["machine"] = v
	}
	if tool, _, _ := strings.Cut(agent, "."); tool != "" {
		if v := msg.CleanValue(tool); v != "" {
			l["tool"] = v
		}
	}
	if v := roleLabel(os.Getenv("AIVOICEBUS_ROLE")); v != "" {
		l[msg.RoleLabel] = v
	}
	if os.Getenv("AIVOICEBUS_ME") == "" { // an overridden identity says nothing about this machine
		if v := msg.CleanValue(Host()); v != "" {
			l["host"] = v
		}
		if suffix := hardwareSuffix(); suffix != "" {
			if chip, product, ok := strings.Cut(suffix, "-"); ok {
				l["chip"], l["product"] = chip, product
			} else {
				l["product"] = suffix
			}
		}
	}
	repo, branch, origin := gitInfo(cwd)
	if repo != "" {
		l["repo"] = repo
		if origin != "" { // for telling two repositories of one name apart (the collision radar)
			l["origin"] = origin
		}
		if branch != "" {
			l["branch"] = branch
		}
	}
	pinned := msg.ParseLabels(os.Getenv("AIVOICEBUS_LABELS"))
	l = l.Overlay(pinned)
	if repo != "" && branch != "" && pinned["repo"] == repo {
		l = l.Overlay(msg.Labels{"branch": branch}) // a detached HEAD (mid-rebase, say) keeps the pinned branch
	}
	// Declared, never guessed (a tool cannot know what its user allows), and read afresh on every
	// call rather than pinned with the rest: dropping to read-only must take effect at once.
	if caps := msg.CleanCaps(os.Getenv("AIVOICEBUS_CAPS")); caps != "" {
		l = l.Overlay(msg.Labels{msg.CapsLabel: caps})
		if l[msg.CapsLabel] == "" && !capsDropWarned.Swap(true) {
			fmt.Fprintf(warnOut, "aivoicebus: warning: AIVOICEBUS_CAPS is not advertised: a session holds at most %d labels and this one's are full (trim AIVOICEBUS_LABELS)\n", msg.MaxLabels)
		}
	}
	return l
}

// capsDropWarned makes the warning above appear once per process; warnOut is where it goes.
var (
	capsDropWarned atomic.Bool
	warnOut        io.Writer = os.Stderr
)

// gitOutput runs git in cwd and returns its trimmed output, or "" on any
// failure. It is a variable so tests need no repository for the parsing parts.
var gitOutput = func(cwd string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitInfo is the repository name and branch of the git checkout containing cwd.
// Only plumbing that reads (rev-parse, symbolic-ref, config --get) is used. The
// name comes from the origin remote, so a worktree directory named after a task
// still reports the real repository; without a remote it is the checkout's
// directory name.
func gitInfo(cwd string) (repo, branch, origin string) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if cwd == "" {
		return "", "", ""
	}
	top := gitOutput(cwd, "rev-parse", "--show-toplevel")
	if top == "" {
		return "", "", ""
	}
	url := gitOutput(cwd, "config", "--get", "remote.origin.url")
	repo, origin = repoFromURL(url), ownerRepoFromURL(url)
	if repo == "" {
		repo = msg.CleanValue(filepath.Base(top))
	}
	// symbolic-ref fails on a detached HEAD, which then simply has no branch label
	branch = msg.CleanValue(gitOutput(cwd, "symbolic-ref", "--short", "-q", "HEAD"))
	return repo, branch, origin
}

// ownerRepoFromURL is owner/name in a remote URL (git@github.com:owner/name.git,
// https://github.com/owner/name/), "" for one that has no owner part, such as a path on disk.
func ownerRepoFromURL(u string) string {
	u = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(u), "/"), ".git")
	if !strings.Contains(u, "://") && !strings.Contains(u, "@") {
		return ""
	}
	parts := strings.FieldsFunc(u, func(r rune) bool { return r == '/' || r == ':' })
	need := 3 // user@host, owner, name
	if strings.Contains(u, "://") {
		need = 4 // scheme, host, owner, name
	}
	if len(parts) < need {
		return ""
	}
	return msg.CleanValue(parts[len(parts)-2] + "/" + parts[len(parts)-1])
}

// repoFromURL is the repository name in a remote URL:
// git@github.com:owner/name.git, https://github.com/owner/name/, /srv/git/name.git.
func repoFromURL(u string) string {
	u = strings.TrimSuffix(strings.TrimSpace(u), "/")
	u = strings.TrimSuffix(u, ".git")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	return msg.CleanValue(u)
}

// roleLabel is the role label for a AIVOICEBUS_ROLE of "build" or "build,review": the valid role
// names, lower-cased, joined with '+' (msg.RolesOf reads it back). "" when none is valid.
func roleLabel(v string) string {
	var roles []string
	for _, r := range strings.FieldsFunc(strings.ToLower(v), func(r rune) bool { return r == ',' || r == '+' || r == ' ' }) {
		if msg.ValidRoleName(r) && !slices.Contains(roles, r) {
			roles = append(roles, r)
		}
	}
	return strings.Join(roles[:min(len(roles), msg.MaxRoles)], "+")
}
