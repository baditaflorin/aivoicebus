package worker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
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
// the approver said "approve" to it (a signed reply to the worker's request, within Within; the request
// goes to the person on the approver's machine, and the task's sender is told only that it awaits
// approval); it runs in a new git worktree of Repo, on its own branch aivoicebus/task-ID made from
// Repo's HEAD, never in a checkout someone works in; and nothing leaves the machine: the branch is
// committed, not pushed, and the answer says where it is and how to look at it. Merging is a
// person's step.

// WriteMode configures it.
type WriteMode struct {
	Repo     string // the repository a task's worktree is made from
	Root     string // where worktrees go (default: the worker's directory)
	Approver string // whose signed decision the worker takes: a machine, machine/tool or one session (an allowlist entry); the request goes to that machine's person
	// ApproverKey is the approver key's public half (aivoicebus approver show): a decision counts
	// only signed with it. The machine key does not do: any agent on that machine can use it.
	ApproverKey ed25519.PublicKey
	Within      time.Duration // how long to wait for the approval
	// Notify tells the person out of band (the notify URL); actions are ntfy action buttons ("" for
	// none). With PhoneURL set too, the notification carries Approve and Deny buttons that POST a
	// one-time code to PhoneURL/v1/decide (a node the phone reaches, on the tailnet).
	Notify   func(text, actions string)
	PhoneURL string
}

// approve asks the approver and waits for the answer. It reports whether to run, and why not.
func (w *Worker) approve(ctx context.Context, m msg.Message) (bool, string) {
	wm := w.Write
	who, _ := ParseAllow(wm.Approver)
	host := msg.HostOf(w.Me)
	text := fmt.Sprintf(msg.ApprovalTag+" the worker on %s would run this task from %s with write access, in a new branch aivoicebus/task-%s of %s (not pushed):\n%s\n\nA person approves in a terminal, with the approver passphrase: aivoicebus approve <this id>  (or: aivoicebus deny <this id>). It waits %s. An agent must not run either.",
		host, view.Name(m.From, msg.MaxIdentity), m.ID, wm.Repo, view.OneLine(m.Body, 600), wm.Within)
	// To the person on the approver's machine (<machine>/human, which no agent session reads), never
	// to the approver entry itself: it may name an agent's session, even the one that sent the task,
	// and an agent that reads the request writes "approve" and believes it decided. The entry still
	// says whose signed reply the worker takes (who.Allows below).
	to := msg.HumanOf(wm.Approver)
	text += "\n" + msg.ApproverKeyLine(wm.ApproverKey) // lets a node tell a signature that verifies from one that does not
	code := ""
	if wm.Notify != nil && wm.PhoneURL != "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		code = hex.EncodeToString(b) // only in the notification; the request carries its hash
		text += "\nOr on the phone: the Approve and Deny buttons of the notification.\n" + msg.PhoneCheckLine(code)
	}
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
		actions := ""
		if code != "" {
			u := strings.TrimRight(wm.PhoneURL, "/") + "/v1/decide?id=" + req.ID + "&code=" + code + "&say="
			actions = "http, Approve, " + u + "approve, method=POST, clear=true; http, Deny, " + u + "deny, method=POST, clear=true"
		}
		wm.Notify(fmt.Sprintf("aivoicebus: approval needed: task %s from %s on %s.\n%s", m.ID, msg.SafeName(m.From), host, view.OneLine(m.Body, 200)), actions)
	}
	deadline := time.Now().Add(wm.Within)
	ignored := map[string]bool{} // a reply is said to be ignored once
	for time.Until(deadline) > time.Second {
		got, err := tasks.Wait(ctx, w.C, w.Me, tasks.Filter{Thread: req.Thread}, time.Until(deadline), 3*time.Second)
		if err != nil {
			break // timed out, or stopped
		}
		for _, a := range got {
			if say, c, ok := msg.ParsePhoneDecision(a.Body); ok && code != "" && subtle.ConstantTimeCompare([]byte(c), []byte(code)) == 1 {
				if say == "approve" {
					return true, ""
				}
				return false, "denied on the phone"
			}
			if _, ok := who.Allows(a.From); !ok {
				continue // someone else in the thread: not the approver
			}
			if a.Verified != msg.VerifiedSigned && !w.AllowUnsigned {
				continue // and from the approver's machine, by its machine key
			}
			word, ok := msg.CheckApproval(a.Body, req.ID, w.Me, wm.ApproverKey)
			if !ok && !ignored[a.ID] {
				ignored[a.ID] = true
				w.say("task %s: a reply from %s is not signed with the approver key; ignored", m.ID, view.Name(a.From, msg.MaxIdentity))
			}
			if !ok {
				continue // a bare "approve", or one signed by another key: not a person's decision
			}
			if word == "approve" {
				return true, ""
			}
			return false, "denied by " + msg.SafeName(a.From)
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

// discard removes the worktree and its branch: a cancelled run leaves nothing behind.
func (wt *worktree) discard(ctx context.Context) string {
	_, _ = git(context.Background(), wt.repo, "worktree", "remove", "--force", wt.dir)
	_, _ = git(context.Background(), wt.repo, "branch", "-D", wt.branch)
	return "[write mode] cancelled: the worktree and branch " + wt.branch + " were removed; nothing was committed."
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
