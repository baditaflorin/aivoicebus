package worker

import (
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
	"strings"
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

	seen map[string]bool // tasks already reported as skipped, so a poll loop says so once
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
	if w.Out != nil {
		fmt.Fprintf(w.Out, format+"\n", a...)
	}
}

func (w *Worker) log(e Entry) {
	if w.Log != nil {
		e.At = w.now().UTC()
		w.Log(e)
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
	w.say("task %s from %s (allowed by %q): running %s", m.ID, who, entry, QuoteCommand(w.Argv))
	env := append(withoutIdentity(w.Env), "AIVOICEBUS_TASK_ID="+m.ID, "AIVOICEBUS_TASK_FROM="+m.From, "AIVOICEBUS_TASK_THREAD="+m.Thread)
	r := Exec(ctx, w.Argv, m.Body, env, w.Limits)
	body, withheld := w.answer(r)
	base.Command, base.Took, base.TimedOut = QuoteCommand(w.Argv), r.Took.Round(time.Millisecond).String(), r.TimedOut
	if r.ExitCode >= 0 {
		code := r.ExitCode
		base.Exit = &code
	}
	base.OutBytes, base.OutSHA, base.Truncated = len(r.Stdout)+len(r.Stderr), sum(r.Stdout+r.Stderr), r.Truncated
	base.Outcome = "ran"
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

// Loop runs Pass every `every` until ctx is done. A node that cannot be reached is reported and
// tried again.
func (w *Worker) Loop(ctx context.Context, every time.Duration) error {
	for {
		if _, err := w.Pass(ctx); err != nil {
			if !client.IsUnreachable(err) {
				return err
			}
			w.say("no node reachable (%v); trying again", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
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
