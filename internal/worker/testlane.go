package worker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// The test lane for a worker (msg/testlane.go, `aivoicebus selftest --worker`). A worker that
// opted in (--test-lane) also reads test messages; a test task goes through every gate a task does
// (allowlist, signature, holds, depth, age, the claim) and is then answered by a built-in step
// instead of the worker's command: nothing is run, no agent is started and no tokens are spent, so
// the lane cannot be used to run anything and a test costs nothing. The steps:
//
//	calc A+B | A-B | A*B | A/B   two whole numbers: the answer is computed here
//	echo TEXT                    the text comes back
//	sleep N                      waits N seconds (at most MaxTestSleep), or until cancelled or paused
//	status                       how the worker is set up: write mode, sandbox
//	write                        what write mode would do with a task; never asks for approval, never makes a worktree
const (
	TestTag      = "[aivoicebus selftest] "
	MaxTestSleep = 5 * time.Minute
)

// TestBody is the body of a test task: the step and its argument.
func TestBody(step, arg string) string { return strings.TrimSpace(TestTag + step + " " + arg) }

// ParseTest reads a test task's body: the step and its argument, and whether it is one.
func ParseTest(body string) (step, arg string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(body), strings.TrimSpace(TestTag))
	if !found {
		return "", "", false
	}
	step, arg, _ = strings.Cut(strings.TrimSpace(rest), " ")
	return step, strings.TrimSpace(arg), step != ""
}

// Calc is what the calc step computes: "17*23" is 391. One operation on two whole numbers of at most
// nine digits, so there is nothing to evaluate that could be anything else.
func Calc(expr string) (int64, error) {
	expr = strings.ReplaceAll(expr, " ", "")
	for i := 1; i < len(expr); i++ {
		if !strings.ContainsRune("+-*/", rune(expr[i])) {
			continue
		}
		a, errA := strconv.ParseInt(expr[:i], 10, 64)
		b, errB := strconv.ParseInt(expr[i+1:], 10, 64)
		if errA != nil || errB != nil || len(expr[:i]) > 9 || len(expr[i+1:]) > 9 || strings.HasPrefix(expr[i+1:], "+") || strings.HasPrefix(expr[i+1:], "-") {
			break
		}
		switch expr[i] {
		case '+':
			return a + b, nil
		case '-':
			return a - b, nil
		case '*':
			return a * b, nil
		default:
			if b == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			return a / b, nil
		}
	}
	return 0, fmt.Errorf("calc takes two whole numbers and one of + - * /, like 17*23")
}

// handleTest answers task m, a test task this worker claimed, by its built-in step.
func (w *Worker) handleTest(ctx context.Context, m msg.Message, base Entry, cancelled *atomic.Bool, stopWhy *atomic.Value) bool {
	host := msg.HostOf(w.Me)
	who := view.Name(m.From, msg.MaxIdentity)
	step, arg, ok := ParseTest(m.Body)
	start := w.now()
	var text string
	switch {
	case !ok:
		text = "not a test step: a test task starts with " + strings.TrimSpace(TestTag) + " and a step (calc, echo, sleep, status, write)"
	case step == "calc":
		if n, err := Calc(arg); err != nil {
			text = "calc failed: " + err.Error()
		} else {
			text = fmt.Sprintf("calc %s = %d", strings.ReplaceAll(arg, " ", ""), n)
		}
	case step == "echo":
		text = "echo " + view.OneLine(arg, 200)
	case step == "sleep":
		secs, err := strconv.Atoi(arg)
		if err != nil || secs < 0 || time.Duration(secs)*time.Second > MaxTestSleep {
			text = fmt.Sprintf("sleep takes whole seconds, 0 to %d", int(MaxTestSleep/time.Second))
			break
		}
		t := time.NewTimer(time.Duration(secs) * time.Second)
		select {
		case <-t.C:
			text = fmt.Sprintf("slept %d s", secs)
		case <-ctx.Done():
			t.Stop()
			why, _ := stopWhy.Load().(string)
			if why == "" {
				why = "stopped"
			}
			text = fmt.Sprintf("sleep stopped after %s: %s", w.now().Sub(start).Round(100*time.Millisecond), why)
		}
	case step == "status":
		text = w.testStatus()
	case step == "write":
		text = w.testWrite()
	default:
		text = "unknown test step " + view.OneLine(step, 40) + " (calc, echo, sleep, status, write)"
	}
	body := fmt.Sprintf("[worker on %s] test: %s", host, text)
	base.Command, base.Took, base.Outcome, base.Why = "", w.now().Sub(start).Round(time.Millisecond).String(), "test", "built-in step "+view.OneLine(step, 20)
	if cancelled.Load() {
		base.Outcome = "cancelled"
		base.Why, _ = stopWhy.Load().(string)
	}
	id, err := w.reply(m, body)
	if err != nil {
		base.Outcome, base.Why = "error", "reply: "+err.Error()
		w.say("test task %s from %s: answered by a built-in step, but the answer could not be sent: %v", m.ID, who, err)
	} else {
		base.Reply = id
		w.say("test task %s from %s: answered by a built-in step (%s); nothing was run", m.ID, who, view.OneLine(step, 20))
	}
	w.log(base)
	return true
}

// testStatus is the status step's answer.
func (w *Worker) testStatus() string {
	mode := "read-only"
	if w.Write != nil {
		mode = "write mode (approver " + msg.SafeName(w.Write.Approver) + ", waits " + w.Write.Within.String() + ")"
	}
	sandbox := "off"
	if w.Sandbox {
		sandbox = "on"
	}
	return "status: " + mode + "; sandbox " + sandbox + "; test lane on"
}

// testWrite is the write step's answer: write mode and what it would do with a task. A test task never
// asks for approval (that is a person at a terminal, by design) and never makes a worktree.
func (w *Worker) testWrite() string {
	if w.Write == nil {
		return "write: write mode is off; a task would run read-only. Nothing was run and nothing was changed."
	}
	return fmt.Sprintf("write: write mode is on; a task would wait up to %s for %s to approve it (signed with the approver key) before it got a worktree of its own. A test task asks nobody and makes no worktree: nothing was run and nothing was changed.",
		w.Write.Within, msg.SafeName(w.Write.Approver))
}
