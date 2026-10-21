package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
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
	_, st := workerSetupStore(t, "desk")
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o644)
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	t.Setenv("AIVOICEBUS_ME", wk)
	c, _ := client.FromEnv()
	allow, _ := worker.LoadAllow()
	const approver = "desk/claude.ok"
	person := msg.HumanOf(approver) // where the worker's request goes: never to the approver entry itself
	// A forgery is planted in the store, as a node that does not guard approval threads would keep it
	// (the node's own refusal is proved in internal/server): the worker must not rely on that guard.
	plant := func(from string, m msg.Message, body string) {
		if _, err := st.Send(msg.Message{ID: msg.NewID(), From: from, To: wk, Re: m.ID, Thread: m.Thread, Body: body}); err != nil {
			t.Error(err)
		}
	}
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
			ms, _ := c.Inbox(person, false)
			for _, m := range ms {
				if strings.HasPrefix(m.Body, "[approval needed]") {
					plant("desk/claude.other", m, signed(akPriv, m.ID, "approve"))
					plant(approver, m, "approve")                          // the machine key only
					plant(approver, m, signed(otherPriv, m.ID, "approve")) // another key
					time.Sleep(300 * time.Millisecond)
					plant(approver, m, signed(akPriv, m.ID, word))
					c.Ack(m.ID, person)
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
			ms, _ := c.Inbox(person, false)
			for _, m := range ms {
				if strings.HasPrefix(m.Body, "[approval needed]") && strings.Contains(m.Body, id3) {
					plant("desk/claude.other", m, signed(akPriv, m.ID, "approve"))
					plant(approver, m, "approve")
					plant(approver, m, signed(otherPriv, m.ID, "approve"))
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

// On the phone: the notification's Approve button POSTs the one-time code to a node's /v1/decide;
// the request itself carries only its hash.
func TestWriteModeTakesAPhoneDecisionWithTheOneTimeCode(t *testing.T) {
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
	akPub, _, _ := ed25519.GenerateKey(rand.Reader)
	actions := make(chan string, 4)
	w := &worker.Worker{C: c, Me: wk, Allow: allow, Argv: []string{"sh", "-c", "echo phone > by-phone.txt"}, Env: os.Environ(),
		Limits: worker.Limits{Timeout: 20 * time.Second, MaxOutput: 4000}, MaxAge: time.Hour, Out: io.Discard, AllowUnsigned: true,
		Write: &worker.WriteMode{Repo: repo, Root: t.TempDir(), Approver: "desk/claude.ok", ApproverKey: akPub, Within: 20 * time.Second,
			PhoneURL: c.URLs[0], Notify: func(text, a string) { actions <- a }}}
	press := func(button string) {
		a := <-actions
		for _, part := range strings.Split(a, ";") {
			f := strings.Split(part, ",")
			if len(f) >= 3 && strings.TrimSpace(f[1]) == button {
				r, err := http.Post(strings.TrimSpace(f[2]), "text/plain", nil)
				if err != nil || r.StatusCode != 200 {
					t.Errorf("pressing %s: %v %v", button, err, r)
				}
				return
			}
		}
		t.Errorf("no %s button in %q", button, a)
	}
	id, _ := c.Send(msg.Message{ID: msg.NewID(), From: boss, To: "lap", Kind: msg.KindTask, Prio: msg.PrioUrgent, Body: "from the phone"})
	go press("Approve")
	if n, err := w.Pass(context.Background()); n != 1 || err != nil {
		t.Fatalf("pass: %d %v", n, err)
	}
	if got := gitT(t, repo, "show", "aivoicebus/task-"+id.ID+":by-phone.txt"); got != "phone" {
		t.Errorf("not run: %q", got)
	}
	reqs, _ := c.Inbox(msg.HumanOf("desk/claude.ok"), true)
	for _, m := range reqs {
		if strings.HasPrefix(m.Body, "[approval needed]") && strings.Contains(m.Body, "phone-check: sha256:") {
			return // the request carries the hash; the code went only to the notification
		}
	}
	t.Error("the request does not carry the code's hash")
}

// The worker's request goes to the person on the approver's machine, not to the approver entry
// (which may be an agent's session, even the sender's own); the sender is told only that the task
// awaits approval; an agent's "approve" is refused by the node; and the person's signed decision,
// which names no more than the request does, is stored and runs the task.
func TestWriteModeAsksThePersonAndTellsTheSenderOnlyThatItWaits(t *testing.T) {
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
	const approver = boss // the entry names the very session that sent the task
	akPub, akPriv, _ := ed25519.GenerateKey(rand.Reader)
	w := &worker.Worker{C: c, Me: wk, Allow: allow, Argv: []string{"sh", "-c", "echo made > made-by-task.txt"}, Env: os.Environ(),
		Limits: worker.Limits{Timeout: 20 * time.Second, MaxOutput: 4000}, MaxAge: time.Hour, Out: io.Discard, AllowUnsigned: true,
		Write: &worker.WriteMode{Repo: repo, Root: t.TempDir(), Approver: approver, ApproverKey: akPub, Within: 30 * time.Second}}
	task, err := c.Send(msg.Message{ID: msg.NewID(), From: boss, To: "lap", Kind: msg.KindTask, Prio: msg.PrioUrgent, Body: "write the file"})
	if err != nil {
		t.Fatal(err)
	}
	person := msg.HumanOf(approver)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			ms, _ := c.Inbox(person, false)
			for _, req := range ms {
				if !strings.HasPrefix(req.Body, "[approval needed]") {
					continue
				}
				if req.To != person || !strings.Contains(req.Body, msg.ApproverKeyLine(akPub)) {
					t.Errorf("the request is to %q and names the key: %v\n%s", req.To, strings.Contains(req.Body, msg.ApproverKeyLine(akPub)), req.Body)
				}
				if ags, _ := c.Inbox(approver, true); len(ags) > 0 {
					for _, m := range ags {
						if strings.HasPrefix(m.Body, "[approval needed]") {
							t.Errorf("the sender's own session was shown the approval request: %q", m.Body)
						}
					}
				}
				told := false
				tm, _ := c.Thread(task.ID)
				for _, m := range tm {
					if m.Kind == msg.KindClaim && strings.HasPrefix(m.Body, "awaiting-approval:") && !strings.Contains(m.Body, req.ID) {
						told = true
					}
				}
				if !told {
					t.Errorf("the sender is not told that the task awaits approval: %+v", tm)
				}
				// the agent writes "approve": the node refuses it, and says who decides
				_, err := c.Send(msg.Message{ID: msg.NewID(), From: approver, To: wk, Re: req.ID, Thread: req.Thread, Body: "approve"})
				var se *client.StatusError
				if !errors.As(err, &se) || se.Refused() != msg.RefusedApproval || !strings.Contains(err.Error(), "aivoicebus approve "+req.ID) {
					t.Errorf("an agent's approve: %v", err)
				}
				// the person's decision
				body := msg.ApprovalBody("approve", ed25519.Sign(akPriv, msg.ApprovalStatement(req.ID, "approve", wk)))
				if _, err := c.Send(msg.Message{ID: msg.NewID(), From: approver, To: wk, Re: req.ID, Thread: req.Thread, Body: body}); err != nil {
					t.Errorf("the person's signed decision: %v", err)
				}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("the request never reached the person's queue")
	}()
	if n, err := w.Pass(context.Background()); n != 1 || err != nil {
		t.Fatalf("pass: %d %v", n, err)
	}
	<-done
	if got := gitT(t, repo, "show", "aivoicebus/task-"+task.ID+":made-by-task.txt"); got != "made" {
		t.Errorf("not run: %q", got)
	}
	tm, _ := c.Thread(task.ID)
	approved := false
	for _, m := range tm {
		if m.Kind == msg.KindProgress && strings.Contains(m.Body, "approved by the person") {
			approved = true
		}
	}
	if !approved {
		t.Errorf("the sender is not told that it was approved: %+v", tm)
	}
}

// aivoicebus answer lists the approvals waiting for the person next to their questions, and points
// an approval's id at approve.
func TestAnswerListsApprovalsAndPointsThemAtApprove(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	req, _ := c.Send(msg.Message{ID: msg.NewID(), From: "lap/codex.worker", To: msg.HumanOf(boss), Prio: msg.PrioUrgent,
		Body: "[approval needed] the worker on lap would run this task:\nwrite the file"})
	t.Setenv("AIVOICEBUS_ME", boss)
	t.Setenv("AIVOICEBUS_AGENT", "")
	_, list, _ := run(t, "", "answer")
	for _, want := range []string{req.ID, "approval needed", "write the file", "aivoicebus approve " + req.ID, "aivoicebus deny " + req.ID} {
		if !strings.Contains(list, want) {
			t.Errorf("the list lacks %q:\n%s", want, list)
		}
	}
	if code, _, e := run(t, "", "answer", req.ID); code != exitUsage || !strings.Contains(e, "aivoicebus approve "+req.ID) {
		t.Errorf("answer on an approval: %d %s", code, e)
	}
}
