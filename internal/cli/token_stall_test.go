package cli

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// handTask sends air a [token hand] task from deploy/claude.9, as hand does.
func (n *tokenNode) handTask(t *testing.T, words string) msg.Message {
	t.Helper()
	task, err := n.c.Send(msg.Message{ID: msg.NewID(), From: "deploy/claude.9", To: "air", Kind: msg.KindTask, Prio: msg.PrioUrgent, Body: tokenHandTag + " air\n" + words})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// inThread waits up to within for a message in task's thread from air/claude.1 whose body starts with tag.
func (n *tokenNode) inThread(t *testing.T, task msg.Message, tag string, within time.Duration) msg.Message {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		ms, _ := n.c.Thread(task.ID)
		for _, m := range ms {
			if m.From == "air/claude.1" && strings.HasPrefix(m.Body, tag) {
				return m
			}
		}
		if time.Now().After(deadline) {
			return msg.Message{}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func noTempTokens(t *testing.T) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(config.Dir(), ".token-*.tmp"))
	if len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
}

// No terminal, no approver key, no notify URL: nobody can approve, so install says so at once, in
// the thread too, with the fix, instead of waiting half an hour for a decision nobody can make.
func TestTokenInstallWithNobodyToAskRefusesAtOnceAndAnswersWhy(t *testing.T) {
	n := tokenMachine(t, nil)
	task := n.handTask(t, "hands air its token, with nobody there to approve")
	start := time.Now()
	code, _, e := run(t, "", "token", "install", task.ID)
	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("took %s to refuse", took)
	}
	for _, want := range []string{"nobody can be asked", "no approver key", "aivoicebus token install " + task.ID + "` in a terminal on air", "aivoicebus approver init", "nothing was changed"} {
		if code == 0 || !strings.Contains(e, want) {
			t.Errorf("no %q: %d %q", want, code, e)
		}
	}
	failed := n.inThread(t, task, tokenFailedTag+" air", 5*time.Second)
	if failed.ID == "" || failed.To != "deploy/claude.9" || !strings.Contains(failed.Body, "aivoicebus token install "+task.ID) || !strings.Contains(failed.Body, "approver init") {
		t.Errorf("the answer in the thread: %+v", failed)
	}
	if why := msg.HoldReason(failed); why != "" {
		t.Errorf("the hand side's agents would not see the answer: %s", why)
	}
	ms, _ := n.c.Inbox("air/human", true)
	for _, m := range ms {
		if strings.HasPrefix(m.Body, "[approval needed]") {
			t.Errorf("asked a person who cannot answer: %q", m.Body)
		}
	}
	if m := n.inThread(t, task, tokenWaitingTag, 0); m.ID != "" {
		t.Error("said it waits for an approval it cannot get")
	}
	if tokenNow(t) != thShared {
		t.Error("changed")
	}
}

// A withdrawn task, or one the nodes no longer have, is refused with what to do, not as "neither a task nor a link".
func TestTokenInstallOfAWithdrawnOrExpiredTaskAsksForAFreshOne(t *testing.T) {
	n := tokenMachine(t, approved)
	task := n.handTask(t, "hands air its token, then thinks better of it")
	t.Setenv("AIVOICEBUS_ME", "deploy/claude.9")
	if code, _, e := run(t, "", "retract", task.ID); code != 0 {
		t.Fatalf("retract: %d %q", code, e)
	}
	t.Setenv("AIVOICEBUS_ME", "air/claude.1")
	code, _, e := run(t, "", "token", "install", task.ID)
	if code == 0 || !strings.Contains(e, "withdrawn") || !strings.Contains(e, "aivoicebus token hand air") || strings.Contains(e, "neither") {
		t.Errorf("a retracted task: %d %q", code, e)
	}
	// taken by a session standing by: the person learns how to get it to their own terminal
	taken := n.handTask(t, "hands air its token; a standing-by session grabs it")
	if _, err := n.c.Send(msg.Message{ID: msg.NewID(), From: "air/claude.2", To: "deploy/claude.9", Re: taken.ID, Thread: taken.Thread, Kind: msg.KindClaim, Body: "taking this"}); err != nil {
		t.Fatal(err)
	}
	code, _, e = run(t, "", "token", "install", taken.ID)
	if code == 0 || !strings.Contains(e, "taken by air/claude.2") || !strings.Contains(e, "aivoicebus token hand air --to air/claude.1") {
		t.Errorf("a task another session took: %d %q", code, e)
	}
	gone := msg.NewID() // expired and dropped by the nodes, or never sent
	code, _, e = run(t, "", "token", "install", gone)
	if code == 0 || !strings.Contains(e, "not found") || !strings.Contains(e, "aivoicebus token hand") {
		t.Errorf("a task the nodes no longer have: %d %q", code, e)
	}
	if tokenNow(t) != thShared {
		t.Error("changed")
	}
}

// handFor runs hand for air in this process with a short wait, while respond plays the session on air.
func handFor(t *testing.T, n *tokenNode, respond func(task msg.Message)) (handResult, string) {
	t.Helper()
	t.Setenv("AIVOICEBUS_ME", "deploy/claude.9")
	dir := filepath.Join(config.Dir(), "machine-tokens")
	writeSecret(t, filepath.Join(dir, "air"), thAir)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			ms, _ := n.c.Inbox("air/claude.1", true)
			for _, m := range ms {
				if strings.HasPrefix(m.Body, tokenHandTag+" air") && m.Kind == msg.KindTask {
					if why := msg.HoldReason(m); why != "" {
						t.Errorf("the task is held back from the agents on air: %s", why)
					}
					if respond != nil {
						respond(m)
					}
					return
				}
			}
		}
	}()
	_, out, e := run(t, "", "token", "hand", "air", "--via", "node", "--wait", "5s", "--json")
	close(stop)
	<-done
	var res handResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%q %q: %v", out, e, err)
	}
	return res, e
}

func (n *tokenNode) answer(t *testing.T, task msg.Message, kind, body string) msg.Message {
	t.Helper()
	m, err := n.c.Send(msg.Message{ID: msg.NewID(), From: "air/claude.1", To: task.From, Re: task.ID, Thread: task.Thread, Kind: kind, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// When hand's wait runs out it says how far the exchange got, and while it waits it says what for.
func TestTokenHandSaysWhereItStalled(t *testing.T) {
	if testing.Short() {
		t.Skip("seven waits of five seconds")
	}
	t.Run("nobody took it", func(t *testing.T) {
		n := tokenMachine(t, approved)
		res, _ := handFor(t, n, nil)
		if res.Stalled != "untaken" || !strings.Contains(res.Error, "nobody took it") || res.Session != "" {
			t.Errorf("%+v", res)
		}
	})
	t.Run("run again, a new task", func(t *testing.T) {
		n := tokenMachine(t, approved)
		first, _ := handFor(t, n, nil)
		again, _ := handFor(t, n, nil)
		if first.Task == "" || first.Task == again.Task || again.Stalled != "untaken" {
			t.Errorf("a hand run again within ten minutes got task %s back (first %s): %+v", again.Task, first.Task, again)
		}
	})
	t.Run("taken, then nothing", func(t *testing.T) {
		n := tokenMachine(t, approved)
		res, e := handFor(t, n, func(task msg.Message) { n.answer(t, task, msg.KindClaim, "installing the token") })
		if res.Stalled != "approval" || !strings.Contains(res.Error, "taken by air/claude.1, but it neither asked the person") || res.Session != "air/claude.1" || !strings.Contains(e, "took it") {
			t.Errorf("%+v %q", res, e)
		}
	})
	t.Run("waiting for the person", func(t *testing.T) {
		n := tokenMachine(t, approved)
		req := msg.NewID()
		res, e := handFor(t, n, func(task msg.Message) {
			n.answer(t, task, msg.KindClaim, "installing the token")
			n.answer(t, task, msg.KindProgress, tokenWaitingTag+" air\nair/claude.1 took it and asked the person on air to approve (request "+req+"): in a terminal on this machine: aivoicebus approve "+req+". It waits up to 30m0s.")
		})
		if !strings.Contains(e, "taken by air/claude.1, waiting for the person's approval on air (approve "+req+" there: aivoicebus approve "+req+" in a terminal on air)") {
			t.Errorf("hand did not show the wait: %q", e)
		}
		if res.Stalled != "approval" || res.Approval != req || !strings.Contains(res.Error, "nobody approved it on air") || !strings.Contains(res.Error, "aivoicebus approve "+req) {
			t.Errorf("%+v", res)
		}
	})
	t.Run("waiting for the person's phone", func(t *testing.T) {
		n := tokenMachine(t, approved)
		req := msg.NewID()
		_, e := handFor(t, n, func(task msg.Message) {
			n.answer(t, task, msg.KindProgress, tokenWaitingTag+" air\nair/claude.1 took it and asked the person on air to approve (request "+req+"): on the phone (there is no approver key here). It waits up to 30m0s.")
		})
		if !strings.Contains(e, "(approve "+req+" there: on the phone)") || strings.Contains(e, "aivoicebus approve") {
			t.Errorf("hand sent the person to a terminal that cannot approve it: %q", e)
		}
	})
	t.Run("approved, but no install", func(t *testing.T) {
		n := tokenMachine(t, approved)
		res, e := handFor(t, n, func(task msg.Message) {
			n.answer(t, task, msg.KindClaim, "installing the token")
			n.answer(t, task, "", tokenReadyTag+" air\napproved by the person (terminal); waiting for the link, up to 30m0s")
		})
		// a ready that does not say it opens sealed keys is an install of 0.10.4 or older
		if res.Sealed || !strings.Contains(e, "runs a release that cannot open a sealed key: the link carries its key") {
			t.Errorf("an older install's link: sealed=%v %q", res.Sealed, e)
		}
		if res.Stalled != "install" || !strings.Contains(res.Error, "approved by the person on air, but air/claude.1 did not answer") || res.Approved != "terminal" {
			t.Errorf("%+v", res)
		}
	})
}

// install runs in an agent's tool call, which can end while it waits: a SIGTERM still answers the
// hand side, and neither that nor a SIGKILL after the approval touches the token file.
func TestAStoppedOrKilledInstallLeavesTheTokenFileAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("runs install as its own process")
	}
	n := tokenMachine(t, nil)
	home := os.Getenv("HOME")
	const pass = "the person on air, not the agent"
	if _, err := config.CreateApprover(pass); err != nil {
		t.Fatal(err)
	}
	priv, err := config.OpenApprover(pass)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("SIGTERM while it waits for the person", func(t *testing.T) {
		task := n.handTask(t, "hands air its token; the install is stopped by its tool call")
		cmd := asCLI(t, home, "air/claude.1", n.url, "token", "install", task.ID, "--wait", "2m")
		var o strings.Builder
		cmd.Stdout, cmd.Stderr = &o, &o
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		m := n.inThread(t, task, tokenWaitingTag+" air", 30*time.Second)
		if m.ID == "" || m.Kind != msg.KindProgress || m.To != "deploy/claude.9" || !strings.Contains(m.Body, "(request ") {
			t.Fatalf("no progress before the wait: %+v\n%s", m, o.String())
		}
		if why := msg.HoldReason(m); why != "" {
			t.Errorf("the progress is held back from the hand side's agents: %s", why)
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		select {
		case err := <-exited:
			if err == nil {
				t.Error("a stopped install exited 0")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("install did not stop on SIGTERM")
		}
		if !strings.Contains(o.String(), "background command") {
			t.Errorf("the agent was not told to run it in the background: %q", o.String())
		}
		if m := n.inThread(t, task, tokenFailedTag+" air", 10*time.Second); m.ID == "" || !strings.Contains(m.Body, "stopped") || msg.HoldReason(m) != "" {
			t.Errorf("the hand side was not told: %+v %q\n%s", m, msg.HoldReason(m), o.String())
		}
		if tokenNow(t) != thShared {
			t.Error("the token file changed")
		}
		noTempTokens(t)
	})

	t.Run("SIGKILL after the approval, waiting for the link", func(t *testing.T) {
		task := n.handTask(t, "hands air its token; the install is killed outright after the person said yes")
		cmd := asCLI(t, home, "air/claude.1", n.url, "token", "install", task.ID, "--wait", "2m")
		var o strings.Builder
		cmd.Stdout, cmd.Stderr = &o, &o
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		var req msg.Message
		for deadline := time.Now().Add(30 * time.Second); req.ID == "" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			ms, _ := n.c.Inbox("air/human", false)
			for _, m := range ms {
				if strings.HasPrefix(m.Body, "[approval needed]") && strings.Contains(m.Body, "deploy/claude.9") {
					req = m
				}
			}
		}
		if req.ID == "" {
			t.Fatalf("no approval request\n%s", o.String())
		}
		sig := ed25519.Sign(priv, msg.ApprovalStatement(req.ID, "approve", req.From))
		if _, err := n.c.Send(msg.Message{ID: msg.NewID(), From: "air/agent", To: req.From, Re: req.ID, Thread: req.Thread, Body: msg.ApprovalBody("approve", sig)}); err != nil {
			t.Fatal(err)
		}
		if m := n.inThread(t, task, tokenReadyTag+" air", 30*time.Second); m.ID == "" {
			t.Fatalf("no ready after the approval\n%s", o.String())
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if tokenNow(t) != thShared {
			t.Error("the token file changed")
		}
		noTempTokens(t)
	})
}
