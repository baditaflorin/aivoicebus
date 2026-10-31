package worker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// handleRun answers a [run] task: it resolves the request against the named commands, runs the one
// argv that gives (no shell, nothing on stdin), and sends back how it ended and what it printed. The
// task is already claimed. It reports whether a command ran.
func (w *Worker) handleRun(ctx context.Context, m msg.Message, base Entry, cancelled *atomic.Bool, stopWhy *atomic.Value) bool {
	who := view.Name(m.From, msg.MaxIdentity)
	host := msg.HostOf(w.Me)
	refuse := func(why string) bool {
		w.say("task %s from %s: run refused: %s", m.ID, who, why)
		base.Outcome, base.Why = "refused", why
		if id, err := w.reply(m, fmt.Sprintf("[worker on %s] not run: %s.", host, why)); err == nil {
			base.Reply = id
		}
		w.log(base)
		return false
	}
	req, err := ParseRun(m.Body)
	if err != nil {
		return refuse(err.Error())
	}
	argv, sp, err := w.Run.Resolve(req)
	if err != nil {
		return refuse(err.Error())
	}
	limits := w.Limits
	limits.Timeout = sp.Limit()
	if sp.UseSandbox() {
		home, _ := os.UserHomeDir()
		argv, _, _ = Sandboxed(argv, home)
	}
	shown := QuoteCommand(append([]string{req.Cmd}, req.Args...))
	w.say("task %s from %s: running named command %s", m.ID, who, shown)
	env := append(withoutIdentity(w.Env), "AIVOICEBUS_TASK_ID="+m.ID, "AIVOICEBUS_TASK_FROM="+m.From,
		"AIVOICEBUS_TASK_DEPTH="+strconv.Itoa(m.Depth+1), "AIVOICEBUS_OFF=1")
	r := Exec(ctx, argv, "", env, limits)
	r.Cancelled = cancelled.Load()
	r.StopWhy, _ = stopWhy.Load().(string)
	body, withheld := w.answerWithin(r, limits.Timeout)
	base.Command, base.Took, base.TimedOut = "run "+shown, r.Took.Round(time.Millisecond).String(), r.TimedOut
	if r.ExitCode >= 0 {
		code := r.ExitCode
		base.Exit = &code
	}
	base.OutBytes, base.OutSHA, base.Truncated = len(r.Stdout)+len(r.Stderr), sum(r.Stdout+r.Stderr), r.Truncated
	base.Outcome = "ran"
	if r.Cancelled {
		base.Outcome, base.Why = "cancelled", r.StopWhy
	}
	if withheld != "" {
		base.Outcome, base.Why = "withheld", "the output looks like it holds a "+withheld
	}
	id, err := w.reply(m, body)
	if err != nil {
		base.Outcome, base.Why = "error", "reply: "+err.Error()
		w.say("task %s: %s, but the answer could not be sent: %v", m.ID, r.Describe(limits.Timeout), err)
	} else {
		base.Reply = id
		w.say("task %s: %s; answered %s (%s)", m.ID, r.Describe(limits.Timeout), who, id)
	}
	w.log(base)
	return true
}
