package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/forward"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/version"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

const (
	forwardLock = ".forward.lock"
	forwardOut  = "forward.out"
)

// forwardCmd wakes an agent that cannot run hooks (Grok Bot) through a webhook: see internal/forward.
//
//	forward --webhook          run in the foreground (what ensure starts)
//	forward ensure             start it in the background unless it runs; quiet; for a shell rc file
//	forward status | stop
func forwardCmd(args []string, s Streams) error {
	if len(args) > 0 {
		switch args[0] {
		case "ensure":
			return forwardEnsure(args[1:], s)
		case "status":
			return forwardStatus(args[1:], s)
		case "stop":
			return forwardStop(args[1:], s)
		}
	}
	fs := newFlagSet("forward")
	hook := fs.Bool("webhook", false, "post new messages and tasks for this machine's grokbot identity to the webhook (AIVOICEBUS_WEBHOOK_URL and _KEY, or the files webhook-url and webhook-key in ~/.config/aivoicebus)")
	agent := fs.String("as", "grokbot", "the agent part of the identity it listens as (host/grokbot); a woken agent replies as it with AIVOICEBUS_ME")
	every := fs.Duration("every", 15*time.Second, "how often the inbox is read when a node cannot hold a read open")
	maxWakes := fs.Int("max-wakes", forward.DefaultMaxWakes, "most POSTs (each starts an agent run) in any hour; the rest wait")
	perThread := fs.Int("max-per-thread", forward.DefaultMaxPerThread, "most messages of one conversation forwarded in any hour: the loop brake")
	batch := fs.Int("batch", forward.DefaultBatch, "most messages in one POST")
	body := fs.Int("body", forward.DefaultBodyRunes, "runes of each message body put in a POST (the rest: aivoicebus thread ID)")
	gap := fs.Duration("min-gap", forward.DefaultMinGap, "least time between two POSTs")
	broadcasts := fs.Bool("broadcasts", false, "also wake for a broadcast that is neither a task nor a reply")
	fyi := fs.Bool("fyi", false, "also wake for low-priority (fyi) messages, which are otherwise only counted")
	lockWait := fs.Duration("lock-wait", time.Second, "how long to wait for a forwarder that is stopping")
	dry := fs.Bool("check", false, "check the settings and that a node answers, post nothing, and exit")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 || !*hook || *maxWakes < 1 || *perThread < 1 || *batch < 1 || *body < 100 || *gap < 0 || *every < time.Second {
		return outbox.Usagef("usage: forward --webhook [--as grokbot] [--max-wakes 20] [--max-per-thread 6] [--batch 5] [--min-gap 5s]   |   forward ensure | status | stop")
	}
	if !validAgentName(*agent) {
		return outbox.Usagef("--as is a name of letters, digits, - and _ (e.g. grokbot)")
	}
	w, err := config.LoadWebhook()
	if err != nil {
		return err
	}
	if w.URL == "" {
		return errors.New("no webhook: set AIVOICEBUS_WEBHOOK_URL (and AIVOICEBUS_WEBHOOK_KEY), or put them in ~/.config/aivoicebus/webhook-url and webhook-key (mode 0600). The key goes in the Authorization header as `Bearer KEY`; AIVOICEBUS_WEBHOOK_HEADER and AIVOICEBUS_WEBHOOK_PREFIX change that")
	}
	if os.Getenv("AIVOICEBUS_ME") == "" { // its own identity, never one of the agent sessions on this machine
		os.Setenv("AIVOICEBUS_AGENT", *agent)
		os.Unsetenv("AIVOICEBUS_AGENT_SRC")
	}
	view.Screen = screenForAgent
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	c.Title = "aivoicebus webhook forwarder"
	c.Labels = identity.Labels("")
	me := identity.Me()
	if *dry {
		h, err := c.Health()
		if err != nil {
			return err
		}
		fmt.Fprintf(s.Out, "ok: %s would post to %s (key in %s header: %v) from node %s\n", me, w.Host(), w.Header, w.Key != "", h.Node)
		return nil
	}
	dir := config.Dir()
	if dir == "" {
		return errors.New("no home directory: ask the user to set HOME, then run it again")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, ok := config.Lock(filepath.Join(dir, forwardLock), *lockWait)
	if !ok {
		return errors.New("another aivoicebus forwarder is running for this user: one at a time, or a message would wake the agent twice")
	}
	defer unlock()
	_ = os.WriteFile(filepath.Join(dir, forwardLock), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)
	logf, err := forward.FileLog(forward.LogPath())
	if err != nil {
		return err
	}
	f := &forward.Forwarder{Bus: c, Me: me, Post: forward.NewHTTPPoster(w), Out: s.Err, Log: logf, Release: version.Release,
		MaxWakes: *maxWakes, MaxPerThread: *perThread, Batch: *batch, BodyRunes: *body, MinGap: *gap, Broadcasts: *broadcasts, FYI: *fyi,
		Quiet: func(t time.Time) bool { return config.LoadQuiet().Active(t) }, Replaced: binaryReplaced()}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(s.Err, "aivoicebus forwarder %s: new messages and tasks for it go to the webhook at %s (%s header), at most %d POSTs and %d messages per conversation an hour. Payloads are labelled untrusted and hold no credential. Log: %s\n",
		me, w.Host(), w.Header, *maxWakes, *perThread, forward.LogPath())
	err = f.Loop(ctx, *every)
	if errors.Is(err, forward.ErrReplaced) {
		// start the new binary as soon as this process lets go of the lock
		if exe, e := os.Executable(); e == nil {
			cmd := exec.Command(exe, append([]string{"forward"}, replaceArgs(args)...)...)
			cmd.Env = os.Environ()
			startDetached(cmd)
		}
		return nil
	}
	return err
}

// replaceArgs are the flags a forwarder was started with, with a longer lock wait for its successor.
func replaceArgs(args []string) []string {
	return append(append([]string{}, args...), "--lock-wait=30s")
}

func validAgentName(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// startDetached starts cmd in its own session with its output appended to forward.out.
func startDetached(cmd *exec.Cmd) error {
	dir := config.Dir()
	out := filepath.Join(dir, forwardOut)
	if fi, err := os.Stat(out); err == nil && fi.Size() > 1<<20 {
		_ = os.Rename(out, out+".1")
	}
	lf, err := os.OpenFile(out, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, lf, lf
	detachForwarder(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func forwardRunning() bool {
	dir := config.Dir()
	if dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, forwardLock)); err != nil {
		return false
	}
	unlock, ok := config.Lock(filepath.Join(dir, forwardLock), 0)
	if ok {
		unlock()
	}
	return !ok
}

// forwardEnsure starts the forwarder in the background when a webhook is set and none runs. It is
// meant for a shell rc file, so it prints nothing unless it started one (or --verbose).
func forwardEnsure(args []string, s Streams) error {
	fs := newFlagSet("forward ensure")
	verbose := fs.Bool("verbose", false, "say why nothing was started")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	say := func(format string, a ...any) {
		if *verbose {
			fmt.Fprintf(s.Out, format+"\n", a...)
		}
	}
	w, err := config.LoadWebhook()
	switch {
	case err != nil:
		say("not started: %v", err)
		return nil
	case w.URL == "":
		say("not started: no webhook is set (AIVOICEBUS_WEBHOOK_URL / AIVOICEBUS_GROKBOT_WEBHOOK_URL, or ~/.config/aivoicebus/webhook-url)")
		return nil
	case forwardRunning():
		say("already running")
		return nil
	}
	if len(config.URLs()) == 0 {
		say("not started: no node URL (AIVOICEBUS_URL)")
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return err
	}
	cmd := exec.Command(exe, "forward", "--webhook")
	cmd.Env = os.Environ()
	if err := startDetached(cmd); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "started the aivoicebus forwarder (log: %s)\n", filepath.Join(config.Dir(), forwardOut))
	return nil
}

func forwardStop(args []string, s Streams) error {
	if len(args) != 0 {
		return outbox.Usagef("usage: forward stop")
	}
	pid := forwardPID()
	if pid == 0 || !forwardRunning() {
		fmt.Fprintln(s.Out, "no forwarder is running")
		return nil
	}
	p, err := os.FindProcess(pid)
	if err == nil {
		err = p.Signal(syscall.SIGTERM)
	}
	if err != nil {
		return fmt.Errorf("could not stop process %d: %v", pid, err)
	}
	fmt.Fprintf(s.Out, "stopped the forwarder (process %d); `forward ensure` starts it again\n", pid)
	return nil
}

// forwardPID is the process the running forwarder wrote into its lock file (0 when unknown).
func forwardPID() int {
	b, _ := os.ReadFile(filepath.Join(config.Dir(), forwardLock))
	var pid int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid)
	return pid
}

// forwardStatus says whether the forwarder runs, who it speaks as, where it posts (host only) and what it did lately.
func forwardStatus(args []string, s Streams) error {
	if len(args) != 0 {
		return outbox.Usagef("usage: forward status")
	}
	w, werr := config.LoadWebhook()
	switch {
	case werr != nil:
		fmt.Fprintf(s.Out, "webhook: not usable: %v\n", werr)
	case w.URL == "":
		fmt.Fprintln(s.Out, "webhook: not set (AIVOICEBUS_WEBHOOK_URL and _KEY, or ~/.config/aivoicebus/webhook-url and webhook-key): nothing is forwarded")
	default:
		fmt.Fprintf(s.Out, "webhook: %s, key %s in the %s header\n", w.Host(), map[bool]string{true: "set", false: "not set"}[w.Key != ""], w.Header)
	}
	if forwardRunning() {
		fmt.Fprintf(s.Out, "forwarder: running (process %d)\n", forwardPID())
	} else {
		fmt.Fprintln(s.Out, "forwarder: not running (`aivoicebus forward ensure` starts it)")
	}
	now := time.Now()
	posts, msgs := forward.WakesIn(now)
	fmt.Fprintf(s.Out, "last hour: %d POST(s), %d message(s)\n", posts, msgs)
	f, err := os.Open(forward.LogPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var last []forward.Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e forward.Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			last = append(last, e)
			if len(last) > 5 {
				last = last[1:]
			}
		}
	}
	for _, e := range last {
		line := fmt.Sprintf("  %s %s %s", e.At.Local().Format("01-02 15:04:05"), e.Outcome, strings.Join(e.IDs, " "))
		if e.Why != "" {
			line += ": " + view.OneLine(e.Why, 160)
		}
		fmt.Fprintln(s.Out, line)
	}
	return nil
}
