package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Worker takes tasks for one identity (Me, usually machine/worker) and runs Argv for each.
type Worker struct {
	C      *client.Client
	Me     string
	Allow  Allowlist
	Argv   []string
	Format string   // what Argv prints (a preset's FormatCodexJSONL...): the answer and the token counts are read out of it
	Env    []string // the command's environment, before the AIVOICEBUS_TASK_* variables are added
	Limits Limits
	MaxAge time.Duration // a task older than this is left alone (a worker started after a week away runs nothing stale)
	Settle time.Duration // passed to tasks.Claim
	DryRun bool
	// AllowUnsigned takes a task that carries no signature (a sender on a client older than 0.7.0).
	// A bad signature, or a key other than the one pinned for the sender's machine, is never taken.
	AllowUnsigned bool
	Out           io.Writer   // one line per decision, for a person or a service log
	Log           func(Entry) // the local record of every decision; nil keeps none
	Now           func() time.Time
	// Replaced reports that the worker's own binary was replaced on disk (an update). Loop then
	// returns ErrReplaced between passes, never during a run, so a service restarts it on the
	// new binary. nil never reports it.
	Replaced func() bool
	// Write, when set, lets the agent change files: each task runs in a new git worktree of
	// Write.Repo on its own branch, only after Write.Approver approved it (see write.go).
	Write *WriteMode
	// Sandbox runs each command without access to this user's secrets (SecretPaths), when the
	// system has a way to (Sandboxed).
	Sandbox bool
	// Budget is how many tokens (in and out, as the agents report them) the worker's runs may use
	// in any 24 hours: over it, it takes no task until older runs fall out of the window. 0 is none.
	Budget int
	// Alert tells the person out of band (the notify URL); nil does nothing.
	Alert func(text string)
	// Tick runs at the start of every loop, between passes (the worker's --watch check). nil does nothing.
	Tick func()

	seen       map[string]bool
	overNote   bool // the budget was said to be used up, once
	sayMu      sync.Mutex
	pausedNote string // the pause last reported, so it is said once // tasks already reported as skipped, so a poll loop says so once
}

// Entry is one line of the worker's log (~/.config/aivoicebus/worker.log, JSON lines). It never
// holds the task text or the output, only their sizes and hashes, so the log itself carries
// nothing a peer sent or a run printed.
type Entry struct {
	At        time.Time `json:"at"`
	Task      string    `json:"task"`
	From      string    `json:"from"`
	Outcome   string    `json:"outcome"` // ran, withheld, lost, skipped, would-run, error
	Why       string    `json:"why,omitempty"`
	Command   string    `json:"command,omitempty"`
	Exit      *int      `json:"exit,omitempty"`
	TimedOut  bool      `json:"timed_out,omitempty"`
	Took      string    `json:"took,omitempty"`
	Usage     *Usage    `json:"usage,omitempty"` // tokens (and cost) the run reported
	TaskBytes int       `json:"task_bytes"`
	TaskSHA   string    `json:"task_sha256"`
	OutBytes  int       `json:"out_bytes,omitempty"`
	OutSHA    string    `json:"out_sha256,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
	Reply     string    `json:"reply,omitempty"` // the id of the answer sent
}

// Decide says whether the worker may take task m: "" when it may, else why not.
func (w *Worker) Decide(m msg.Message) (why, entry string) {
	if m.Kind != msg.KindTask {
		return "not a task", ""
	}
	entry, ok := w.Allow.Allows(m.From)
	if !ok {
		if w.Allow.Empty() {
			return "the allowlist is empty (aivoicebus worker allow add SENDER)", ""
		}
		return "the sender is not in the allowlist", ""
	}
	switch m.Verified {
	case msg.VerifiedSigned:
	case msg.VerifiedUnsigned:
		if !w.AllowUnsigned {
			return "it is not signed (a sender on a client older than 0.7.0): --allow-unsigned takes such tasks", entry
		}
	case msg.VerifiedChanged:
		return "it is signed by a key other than the one pinned for " + msg.HostOf(m.From) + " (KEY CHANGED): not from who it says, or that machine was reinstalled", entry
	default:
		return "its signature does not verify: not from who it says", entry
	}
	host := msg.HostOf(w.Me)
	tool, _, _ := strings.Cut(strings.TrimPrefix(w.Me, host+"/"), ".")
	if to := strings.ToLower(m.To); to != host && to != host+"/*" && to != w.Me && to != host+"/"+tool {
		return fmt.Sprintf("it was sent to %s, not to this machine or this worker by name", view.Name(m.To, msg.MaxIdentity)), entry
	}
	if why := msg.HoldReason(m); why != "" {
		return "held for the user: " + why + " (aivoicebus held)", entry
	}
	if m.Depth > msg.MaxTaskDepth {
		return fmt.Sprintf("it is %d tasks deep (sent from the run of a task sent from ...): more than %d is a chain reaction", m.Depth, msg.MaxTaskDepth), entry
	}
	if age := w.now().Sub(m.At); w.MaxAge > 0 && age > w.MaxAge {
		return fmt.Sprintf("it is %s old (more than --max-age %s)", age.Round(time.Minute), w.MaxAge), entry
	}
	return "", entry
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) say(format string, a ...any) {
	w.sayMu.Lock() // the cancel watch says things while a run goes on
	defer w.sayMu.Unlock()
	if w.Out != nil {
		fmt.Fprintf(w.Out, format+"\n", a...)
	}
}

func (w *Worker) log(e Entry) {
	if w.Log != nil {
		e.At = w.now().UTC()
		w.Log(e)
	}
	if !w.DryRun && e.Outcome != "skipped" { // a decision that did something, or refused to: in the audit log too
		detail := fmt.Sprintf("task %s from %s", e.Task, e.From)
		if e.Why != "" {
			detail += ": " + e.Why
		}
		if e.Exit != nil {
			detail += fmt.Sprintf(": exit %d after %s", *e.Exit, e.Took)
		}
		config.Audit(w.Me, "worker "+e.Outcome, detail)
	}
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// Pass reads this worker's queue once and handles what it may, one task after another (never two
// at once). It returns how many tasks it ran (or, dry, would run).
func (w *Worker) Pass(ctx context.Context) (int, error) {
	if w.seen == nil {
		w.seen = map[string]bool{}
	}
	ms, err := w.C.Inbox(w.Me, false)
	if err != nil {
		return 0, err
	}
	if p, until, on := msg.ActivePause(ms, w.now()); on && !w.DryRun {
		if note := p.ID + until.String(); note != w.pausedNote {
			w.pausedNote = note
			w.say("paused by %s until %s: taking no task (aivoicebus resume, on that machine, lifts it)", view.Name(p.From, msg.MaxIdentity), until.Local().Format("01-02 15:04"))
		}
		return 0, nil
	}
	w.pausedNote = ""
	if w.Budget > 0 && !w.DryRun {
		if spent := SpentTokens(w.now().Add(-24 * time.Hour)); spent >= w.Budget {
			if !w.overNote {
				w.overNote = true
				line := fmt.Sprintf("the worker %s used %d tokens in the last 24h, over its budget of %d: it takes no task until older runs fall out of the window (worker --budget)", w.Me, spent, w.Budget)
				w.say("%s", line)
				if w.Alert != nil {
					w.Alert("aivoicebus: " + line)
				}
			}
			return 0, nil
		}
		w.overNote = false
	}
	var queue []msg.Message
	for _, m := range ms {
		if m.Kind == msg.KindTask && m.Retracted == nil {
			queue = append(queue, m)
		}
	}
	sort.SliceStable(queue, func(i, j int) bool { return queue[i].At.Before(queue[j].At) })
	ran := 0
	for _, m := range queue {
		if ctx.Err() != nil {
			break
		}
		base := Entry{Task: m.ID, From: m.From, TaskBytes: len(m.Body), TaskSHA: sum(m.Body)}
		why, entry := w.Decide(m)
		if why != "" {
			if w.DryRun {
				w.say("would skip task %s from %s: %s", m.ID, view.Name(m.From, msg.MaxIdentity), why)
			} else if !w.seen[m.ID] {
				w.seen[m.ID] = true
				w.say("skipped task %s from %s: %s", m.ID, view.Name(m.From, msg.MaxIdentity), why)
				base.Outcome, base.Why = "skipped", why
				w.log(base)
			}
			continue
		}
		if w.DryRun {
			ran++
			cmd := QuoteCommand(w.Argv)
			if cmd == "" {
				cmd = "the --exec command"
			}
			w.say("would run task %s from %s (allowed by %q, %d bytes on stdin): %s", m.ID, view.Name(m.From, msg.MaxIdentity), entry, len(m.Body), cmd)
			w.say("  %s", view.OneLine(m.Body, 160))
			continue
		}
		if w.handle(ctx, m, entry, base) {
			ran++
		}
	}
	return ran, nil
}

// handle claims m and, when it is this worker's, runs the command and answers. It reports whether
// the command ran.
func (w *Worker) handle(ctx context.Context, m msg.Message, entry string, base Entry) bool {
	who := view.Name(m.From, msg.MaxIdentity)
	c, err := tasks.Claim(w.C, w.Me, m.ID, "the worker on "+msg.HostOf(w.Me)+" is running it now", w.Settle, io.Discard)
	switch {
	case err != nil:
		w.say("task %s from %s: claim failed: %v", m.ID, who, err)
		base.Outcome, base.Why = "error", "claim: "+err.Error()
		w.log(base)
		return false
	case !c.Mine:
		w.say("task %s from %s: %s took it first; leaving it", m.ID, who, view.Name(c.Owner.From, msg.MaxIdentity))
		base.Outcome, base.Why = "lost", "claimed first by "+c.Owner.From
		w.log(base)
		return false
	}
	limits := w.Limits
	// The sender can take the task back (`aivoicebus cancel ID`) while it waits for approval or
	// runs: the watch stops both, and a run is killed with everything it started.
	taskCtx, stopTask := context.WithCancel(ctx)
	defer stopTask()
	cancelled := &atomic.Bool{}
	stopWhy := &atomic.Value{}
	go w.watchCancel(taskCtx, m.ID, cancelled, stopWhy, stopTask)
	var wt *worktree
	if w.Write != nil {
		ok, why := w.approve(taskCtx, m)
		if cancelled.Load() {
			ok, why = false, "cancelled by its sender"
			if s, _ := stopWhy.Load().(string); s != "" {
				why = s
			}
		}
		if !ok {
			w.say("task %s from %s: not run: %s", m.ID, who, why)
			base.Outcome, base.Why = "not approved", why
			if id, err := w.reply(m, fmt.Sprintf("[worker on %s] not run: %s.", msg.HostOf(w.Me), why)); err == nil {
				base.Reply = id
			}
			w.log(base)
			return false
		}
		var err error
		if wt, err = newWorktree(ctx, w.Write.Repo, w.Write.Root, m.ID); err != nil {
			w.say("task %s: no worktree: %v", m.ID, err)
			base.Outcome, base.Why = "error", "worktree: "+err.Error()
			_, _ = w.reply(m, fmt.Sprintf("[worker on %s] not run: could not make a worktree (%v).", msg.HostOf(w.Me), err))
			w.log(base)
			return false
		}
		limits.Dir = wt.dir
	}
	w.say("task %s from %s (allowed by %q): running %s", m.ID, who, entry, QuoteCommand(w.Argv))
	env := append(withoutIdentity(w.Env), "AIVOICEBUS_TASK_ID="+m.ID, "AIVOICEBUS_TASK_FROM="+m.From, "AIVOICEBUS_TASK_THREAD="+m.Thread,
		"AIVOICEBUS_TASK_DEPTH="+strconv.Itoa(m.Depth+1), // what the run sends is a task deeper
		"AIVOICEBUS_OFF=1") // no session hooks in the run: the agent gets the task, not this machine's inbox (found live: it read and answered old peer messages)
	if w.Format != FormatText {
		limits.MaxOutput = max(limits.MaxOutput, 4<<20) // JSON events are bulky: keep them whole to read; the answer is cut below
	}
	argv := w.Argv
	if w.Sandbox {
		home, _ := os.UserHomeDir()
		argv, _, _ = Sandboxed(argv, home)
	}
	go w.heartbeat(taskCtx, m) // a long run says it is alive, so the sender does not give up on it
	r := Exec(taskCtx, argv, m.Body, env, limits)
	if w.Format != FormatText {
		r.Stdout, r.Usage = ParseOutput(w.Format, r.Stdout)
		if len(r.Stdout) > w.Limits.MaxOutput {
			r.Stdout, r.Truncated = r.Stdout[:w.Limits.MaxOutput], true
		}
	}
	r.Cancelled = cancelled.Load()
	r.StopWhy, _ = stopWhy.Load().(string)
	body, withheld := w.answer(r)
	if wt != nil && r.Cancelled {
		body += "\n" + wt.discard(ctx) // nothing half done is committed
	} else if wt != nil {
		body += "\n" + wt.finish(ctx, m)                                     // commit what it changed on the branch, and say where
		if kind, found := outbox.Credential(body); found && withheld == "" { // file names are the agent's words too
			withheld = kind
			body = fmt.Sprintf("[worker on %s] %s, but its answer is withheld: it looks like it holds a %s. The branch aivoicebus/task-%s is on this machine.", msg.HostOf(w.Me), r.Describe(w.Limits.Timeout), kind, m.ID)
		}
	}
	base.Command, base.Took, base.TimedOut = QuoteCommand(w.Argv), r.Took.Round(time.Millisecond).String(), r.TimedOut
	base.Usage = r.Usage
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
		w.say("task %s: %s, but the answer could not be sent: %v", m.ID, r.Describe(w.Limits.Timeout), err)
	} else {
		base.Reply = id
		w.say("task %s: %s; answered %s (%s)", m.ID, r.Describe(w.Limits.Timeout), who, id)
	}
	w.log(base)
	return true
}

// HeartbeatEvery is how often a running task says it is still running.
var HeartbeatEvery = 2 * time.Minute

// heartbeat sends a [progress] message to the task's sender every HeartbeatEvery until ctx ends.
func (w *Worker) heartbeat(ctx context.Context, m msg.Message) {
	start := w.now()
	t := time.NewTicker(HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		body := fmt.Sprintf("[worker on %s] still running, %s in", msg.HostOf(w.Me), w.now().Sub(start).Round(time.Second))
		if plan, err := outbox.Prepare(w.C, outbox.Request{To: m.From, Re: m.ID, Thread: m.Thread, Kind: msg.KindProgress, Body: body, TargetSaid: true}, io.Discard); err == nil {
			_, _ = plan.Send(w.C)
		}
	}
}

// watchCancel stops the task when its sender withdraws it: it looks every two seconds until ctx ends.
func (w *Worker) watchCancel(ctx context.Context, id string, cancelled *atomic.Bool, why *atomic.Value, stop context.CancelFunc) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if in, err := w.C.Inbox(w.Me, false); err == nil {
			if p, _, on := msg.ActivePause(in, w.now()); on {
				why.Store("stopped by a pause from " + msg.SafeName(p.From))
				cancelled.Store(true)
				w.say("task %s: paused by %s; stopping it", id, view.Name(p.From, msg.MaxIdentity))
				stop()
				return
			}
		}
		ms, err := w.C.Thread(id)
		if err != nil {
			continue
		}
		for _, x := range ms {
			if x.ID == id && x.Retracted != nil {
				cancelled.Store(true)
				w.say("task %s: its sender took it back; stopping it", id)
				stop()
				return
			}
		}
	}
}

// answer is the text sent back: what the run printed, under a line that says how it ended. It
// returns the kind of credential found instead when the text holds one; the answer then says only
// that the output was withheld.
func (w *Worker) answer(r Result) (body, withheld string) {
	host := msg.HostOf(w.Me)
	var b strings.Builder
	fmt.Fprintf(&b, "[worker on %s] %s.\n", host, r.Describe(w.Limits.Timeout))
	out := strings.TrimRight(r.Stdout, "\n")
	if out != "" {
		b.WriteString(out + "\n")
	}
	if errText := strings.TrimRight(r.Stderr, "\n"); errText != "" && (r.ExitCode != 0 || out == "") {
		b.WriteString("--- stderr ---\n" + tail(errText, 2000) + "\n")
	}
	if r.Truncated {
		fmt.Fprintf(&b, "[output cut at %d bytes]\n", w.Limits.MaxOutput)
	}
	body = strings.TrimRight(b.String(), "\n")
	if len(body) > msg.MaxBody-512 {
		body = body[:msg.MaxBody-512] + "\n[answer cut to fit a message]"
		body = strings.ToValidUTF8(body, "")
	}
	if kind, found := outbox.Credential(body); found {
		return fmt.Sprintf("[worker on %s] %s, but its output is withheld: it looks like it holds a %s. Nothing of it was sent or kept.", host, r.Describe(w.Limits.Timeout), kind), kind
	}
	return body, ""
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + strings.ToValidUTF8(s[len(s)-n:], "")
}

// reply sends the answer and ends the conversation in one message: the worker does not wait for
// more, so the sender's wait stops with it.
func (w *Worker) reply(m msg.Message, body string) (string, error) {
	plan, err := outbox.Prepare(w.C, outbox.Request{To: m.From, Re: m.ID, Thread: m.Thread, Kind: msg.KindEnd, Body: body, TargetSaid: true}, io.Discard)
	if err != nil {
		return "", err
	}
	out, err := plan.Send(w.C)
	return out.ID, err
}

// withoutIdentity drops the variables that would make the command speak as the worker: a run that
// uses aivoicebus itself is a session of its own.
func withoutIdentity(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "AIVOICEBUS_ME", "AIVOICEBUS_AGENT", "AIVOICEBUS_AGENT_SRC", "AIVOICEBUS_TASK_ID", "AIVOICEBUS_TASK_FROM", "AIVOICEBUS_TASK_THREAD":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Loop runs Pass until ctx is done. Between passes it holds a read open on a node that can
// (msg.FeatureInboxWait), so a task sent to it is taken within a second and the worker shows as
// listening; with an older node, or when a held read fails, it passes every `every`. A node that
// cannot be reached is reported and tried again.
func (w *Worker) Loop(ctx context.Context, every time.Duration) error {
	held := false
	if h, err := w.C.Health(); err == nil && h.Has(msg.FeatureInboxWait) {
		held = true
	}
	var gen uint64
	hasGen := false
	for {
		if w.Tick != nil {
			w.Tick()
		}
		if w.Replaced != nil && w.Replaced() {
			w.say("this binary was replaced on disk (an update): stopping between tasks, so the service starts the new one")
			return ErrReplaced
		}
		if held && !hasGen { // the change counter before the pass: whatever comes after it wakes the hold
			if _, g, err := w.C.InboxWait(ctx, w.Me, 0, false, 0); err == nil {
				gen, hasGen = g, true
			} else {
				held = false
			}
		}
		if _, err := w.Pass(ctx); err != nil {
			if !client.IsUnreachable(err) {
				return err
			}
			w.say("no node reachable (%v); trying again", err)
			hasGen = false
		}
		if held && hasGen {
			start := time.Now()
			_, g, err := w.C.InboxWait(ctx, w.Me, gen, true, msg.MaxInboxWait)
			switch {
			case ctx.Err() != nil:
				return nil
			case err != nil:
				hasGen = false // ask afresh, after the pause below
			default:
				gen = g
				if time.Since(start) >= time.Second {
					continue // it waited: something arrived, or the hold ran out; pass again now
				}
			}
		}
		// No held read, or one that answered at once (a busy board): pass again after `every` at
		// most, and at least a second, so a node that answers at once cannot make this spin.
		pause := every
		if held {
			pause = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pause):
		}
	}
}

// ErrReplaced is Loop's answer when the worker's binary was replaced: exit non-zero, so launchd
// (KeepAlive on failure) and systemd (Restart=on-failure) start the new binary.
var ErrReplaced = errors.New("the worker's binary was replaced; restart it to run the new one")

// Paused reports the pause in force for the worker now, if any (for what runs between passes).
func (w *Worker) Paused() (msg.Message, bool) {
	ms, err := w.C.Inbox(w.Me, false)
	if err != nil {
		return msg.Message{}, false
	}
	p, _, on := msg.ActivePause(ms, w.now())
	return p, on
}

// SpentTokens adds up the tokens the runs in worker.log since since reported (in and out).
func SpentTokens(since time.Time) int {
	f, err := os.Open(LogPath())
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Usage != nil && !e.At.Before(since) {
			n += e.Usage.Total()
		}
	}
	return n
}

// LogPath is where the worker's log lives.
func LogPath() string {
	if d := config.Dir(); d != "" {
		return filepath.Join(d, "worker.log")
	}
	return ""
}

// maxLog is the size at which the log is moved to worker.log.1 (one old file is kept).
const maxLog = 1 << 20

// FileLog appends entries to path as JSON lines (mode 0600). A symlink in its place is refused, so
// the log cannot be pointed at another file.
func FileLog(path string) (func(Entry), error) {
	if path == "" {
		return nil, errors.New("no home directory for the worker log")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	check := func() error {
		if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file (a symlink is refused)", path)
		}
		return nil
	}
	if err := check(); err != nil {
		return nil, err
	}
	return func(e Entry) {
		if check() != nil {
			return
		}
		if fi, err := os.Stat(path); err == nil && fi.Size() > maxLog {
			_ = os.Rename(path, path+".1")
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		b, _ := json.Marshal(e)
		_, _ = f.Write(append(b, '\n'))
	}, nil
}
