package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/worker"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Write mode runs a task only after its approver said so (signed; anyone else's "approve" does
// not count), in a worktree on its own branch, and leaves the checkout alone; a deny runs nothing.
func TestWriteModeRunsOnlyWhatItsApproverApprovedOnABranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	workerSetup(t, "desk")
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o644)
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	t.Setenv("AIVOICEBUS_ME", wk)
	c, _ := client.FromEnv()
	allow, _ := worker.LoadAllow()
	const approver = "desk/claude.ok"
	akPub, akPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	w := &worker.Worker{C: c, Me: wk, Allow: allow, Argv: []string{"sh", "-c", "echo made > made-by-task.txt"}, Env: os.Environ(),
		Limits: worker.Limits{Timeout: 20 * time.Second, MaxOutput: 4000}, MaxAge: time.Hour, Out: io.Discard, AllowUnsigned: true,
		Write: &worker.WriteMode{Repo: repo, Root: t.TempDir(), Approver: approver, ApproverKey: akPub, Within: 20 * time.Second}}
	signed := func(key ed25519.PrivateKey, req, word string) string {
		return msg.ApprovalBody(word, ed25519.Sign(key, msg.ApprovalStatement(req, word, wk)))
	}

	answer := func(word string) { // as the approver (and first as someone else)
		for i := 0; i < 100; i++ {
			ms, _ := c.Inbox(approver, false)
			for _, m := range ms {
				if strings.HasPrefix(m.Body, "[approval needed]") {
					c.Send(msg.Message{ID: msg.NewID(), From: "desk/claude.other", To: wk, Re: m.ID, Thread: m.Thread, Body: signed(akPriv, m.ID, "approve")})
					c.Send(msg.Message{ID: msg.NewID(), From: approver, To: wk, Re: m.ID, Thread: m.Thread, Body: "approve"})                          // the machine key only
					c.Send(msg.Message{ID: msg.NewID(), From: approver, To: wk, Re: m.ID, Thread: m.Thread, Body: signed(otherPriv, m.ID, "approve")}) // another key
					time.Sleep(300 * time.Millisecond)
					c.Send(msg.Message{ID: msg.NewID(), From: approver, To: wk, Re: m.ID, Thread: m.Thread, Body: signed(akPriv, m.ID, word)})
					c.Ack(m.ID, approver)
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	task := func(text string) string {
		m, err := c.Send(msg.Message{ID: msg.NewID(), From: boss, To: "lap", Kind: msg.KindTask, Prio: msg.PrioUrgent, Body: text})
		if err != nil {
			t.Fatal(err)
		}
		return m.ID
	}

	id := task("write the file")
	go answer("approve")
	if n, err := w.Pass(context.Background()); n != 1 || err != nil {
		t.Fatalf("approved pass: %d %v", n, err)
	}
	branch := "aivoicebus/task-" + id
	if got := gitT(t, repo, "show", branch+":made-by-task.txt"); got != "made" {
		t.Errorf("the branch lacks the change: %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "made-by-task.txt")); err == nil {
		t.Error("the checkout itself was changed")
	}
	if gitT(t, repo, "rev-parse", "--abbrev-ref", "HEAD") != "main" {
		t.Error("the checkout moved off its branch")
	}
	ms, _ := c.Thread(id)
	if last := ms[len(ms)-1]; !strings.Contains(last.Body, "committed on branch "+branch) {
		t.Errorf("the answer does not say where: %q", last.Body)
	}

	id2 := task("write it again")
	go answer("deny")
	w.Pass(context.Background())
	if out, _ := exec.Command("git", "-C", repo, "rev-parse", "--verify", "aivoicebus/task-"+id2).CombinedOutput(); !strings.Contains(string(out), "fatal") {
		t.Error("a denied task left a branch")
	}
	ms, _ = c.Thread(id2)
	if last := ms[len(ms)-1]; !strings.Contains(last.Body, "not run: denied by "+approver) {
		t.Errorf("deny: %q", last.Body)
	}

	// only a bare "approve", one signed by another key, and the approver key in someone else's
	// hands: nothing runs
	w.Write.Within = 3 * time.Second
	id3 := task("write it a third time")
	go func() {
		for i := 0; i < 100; i++ {
			ms, _ := c.Inbox(approver, false)
			for _, m := range ms {
				if strings.HasPrefix(m.Body, "[approval needed]") && strings.Contains(m.Body, id3) {
					c.Send(msg.Message{ID: msg.NewID(), From: "desk/claude.other", To: wk, Re: m.ID, Thread: m.Thread, Body: signed(akPriv, m.ID, "approve")})
					c.Send(msg.Message{ID: msg.NewID(), From: approver, To: wk, Re: m.ID, Thread: m.Thread, Body: "approve"})
					c.Send(msg.Message{ID: msg.NewID(), From: approver, To: wk, Re: m.ID, Thread: m.Thread, Body: signed(otherPriv, m.ID, "approve")})
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	w.Pass(context.Background())
	if out, _ := exec.Command("git", "-C", repo, "rev-parse", "--verify", "aivoicebus/task-"+id3).CombinedOutput(); !strings.Contains(string(out), "fatal") {
		t.Error("a task ran without the approver's own signed decision")
	}
	ms, _ = c.Thread(id3)
	if last := ms[len(ms)-1]; !strings.Contains(last.Body, "no approval from "+approver) {
		t.Errorf("not the approver: %q", last.Body)
	}
}
