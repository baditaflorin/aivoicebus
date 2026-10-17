package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Write mode: the agent may change files, so every task is bounded three ways. It runs only after
// the approver said "approve" to it (a signed reply to the worker's request, within Within); it runs
// in a new git worktree of Repo, on its own branch aivoicebus/task-ID made from Repo's HEAD, never in
// a checkout someone works in; and nothing leaves the machine: the branch is committed, not pushed,
// and the answer says where it is and how to look at it. Merging is a person's step.

// WriteMode configures it.
type WriteMode struct {
	Repo     string        // the repository a task's worktree is made from
	Root     string        // where worktrees go (default: the worker's directory)
	Approver string        // who approves each task: a machine, machine/tool or one session (an allowlist entry)
	Within   time.Duration // how long to wait for the approval
	Notify   func(text string)
}

// approve asks the approver and waits for the answer. It reports whether to run, and why not.
func (w *Worker) approve(ctx context.Context, m msg.Message) (bool, string) {
	wm := w.Write
	who, _ := ParseAllow(wm.Approver)
	host := msg.HostOf(w.Me)
	text := fmt.Sprintf("[approval needed] the worker on %s would run this task from %s with write access, in a new branch aivoicebus/task-%s of %s (not pushed):\n%s\n\nApprove: aivoicebus approve <this id>    Deny: aivoicebus deny <this id>    (it waits %s)",
		host, view.Name(m.From, msg.MaxIdentity), m.ID, wm.Repo, view.OneLine(m.Body, 600), wm.Within)
	to := wm.Approver
	plan, err := outbox.Prepare(w.C, outbox.Request{To: to, Body: text, Prio: msg.PrioUrgent, Queue: true, TargetSaid: true}, io.Discard)
	if err != nil {
		return false, "the approval request could not be prepared: " + err.Error()
	}
	req, err := plan.Send(w.C)
	if err != nil {
		return false, "the approval request could not be sent: " + err.Error()
	}
	w.say("task %s: asked %s to approve (%s), waiting up to %s", m.ID, view.Name(to, msg.MaxIdentity), req.ID, wm.Within)
	if wm.Notify != nil {
		wm.Notify(fmt.Sprintf("aivoicebus: approval needed for task %s from %s on %s. aivoicebus approve %s", m.ID, msg.SafeName(m.From), host, req.ID))
	}
	deadline := time.Now().Add(wm.Within)
	for time.Until(deadline) > time.Second {
		got, err := tasks.Wait(ctx, w.C, w.Me, tasks.Filter{Thread: req.Thread}, time.Until(deadline), 3*time.Second)
		if err != nil {
			break // timed out, or stopped
		}
		for _, a := range got {
			if _, ok := who.Allows(a.From); !ok {
				continue // someone else in the thread: not the approver
			}
			if a.Verified != msg.VerifiedSigned && !w.AllowUnsigned {
				continue // an approval is taken only signed by the approver's machine
			}
			switch strings.ToLower(strings.TrimSpace(a.Body)) {
			case "approve", "approved", "yes":
				return true, ""
			case "deny", "denied", "no":
				return false, "denied by " + msg.SafeName(a.From)
			}
		}
	}
	return false, fmt.Sprintf("no approval from %s within %s", msg.SafeName(wm.Approver), wm.Within)
}

type worktree struct{ repo, dir, branch string }

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(view.OneLine(string(out), 300)))
	}
	return strings.TrimSpace(string(out)), nil
}

// newWorktree makes aivoicebus/task-ID from repo's HEAD in a directory of its own.
func newWorktree(ctx context.Context, repo, root, id string) (*worktree, error) {
	if root == "" {
		root, _ = os.Getwd()
	}
	dir := filepath.Join(root, "wt-"+id)
	branch := "aivoicebus/task-" + id
	if _, err := git(ctx, repo, "worktree", "add", "-b", branch, dir, "HEAD"); err != nil {
		return nil, err
	}
	return &worktree{repo: repo, dir: dir, branch: branch}, nil
}

// finish commits whatever the run changed on the branch, removes the worktree (the branch stays),
// and says where the change is.
func (wt *worktree) finish(ctx context.Context, m msg.Message) string {
	defer func() { _, _ = git(context.Background(), wt.repo, "worktree", "remove", "--force", wt.dir) }()
	if _, err := git(ctx, wt.dir, "add", "-A"); err != nil {
		return "[write mode] " + err.Error()
	}
	if st, _ := git(ctx, wt.dir, "status", "--porcelain"); st == "" {
		_, _ = git(context.Background(), wt.repo, "branch", "-D", wt.branch)
		return "[write mode] it changed no file; no branch was kept."
	}
	subject := "aivoicebus task " + m.ID + " from " + msg.SafeName(m.From) + ": " + view.OneLine(m.Body, 60)
	if _, err := git(ctx, wt.dir, "-c", "user.name=aivoicebus worker", "-c", "user.email=worker@aivoicebus.invalid", "commit", "-q", "-m", subject); err != nil {
		return "[write mode] " + err.Error()
	}
	stat, _ := git(ctx, wt.dir, "show", "--stat", "--format=%h", "HEAD")
	return fmt.Sprintf("[write mode] committed on branch %s of %s (not pushed):\n%s\nLook: git -C %s show %s    Keep it: merge it yourself    Drop it: git -C %s branch -D %s",
		wt.branch, wt.repo, tail(stat, 1500), wt.repo, wt.branch, wt.repo, wt.branch)
}
