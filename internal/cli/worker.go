package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
	"github.com/baditaflorin/aivoicebus/internal/worker"
)

// binaryReplaced reports whether the running binary's file changed since now (size, mtime or
// inode: `install` and `update` write a new file). An unknown path never reports it.
func binaryReplaced() func() bool {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	first, err := os.Stat(exe)
	if err != nil {
		return nil
	}
	return func() bool {
		now, err := os.Stat(exe)
		if err != nil {
			return false // mid-replace, or gone: look again next time
		}
		return !os.SameFile(first, now) || now.Size() != first.Size() || !now.ModTime().Equal(first.ModTime())
	}
}

// workerCmd runs a command for tasks sent to this machine while no session is open. It runs an
// agent on input from peers, so it does nothing until the user allows senders by name.
func workerCmd(args []string, s Streams) error {
	if len(args) > 0 && args[0] == "allow" {
		return workerAllowCmd(args[1:], s)
	}
	if len(args) > 0 && (args[0] == "install" || args[0] == "uninstall" || args[0] == "status") {
		return workerSvcCmd(args[0], args[1:], s)
	}
	fs := newFlagSet("worker")
	execT := fs.String("exec", "", "the command to run for each task, e.g. 'claude -p'; the task text goes on its stdin, never on its command line (split like a shell would, but no shell runs it)")
	agent := fs.String("agent", "", "instead of --exec, a read-only headless preset of an agent CLI: "+strings.Join(worker.PresetNames(), " | ")+"; the worker then also takes tasks sent to this machine's sessions of that tool (host/TOOL)")
	once := fs.Bool("once", false, "look at the queue once, run what may be run, and exit")
	dry := fs.Bool("dry-run", false, "say which waiting tasks the worker would run and which it would skip, and change nothing (no claim, no run)")
	timeout := fs.Duration("timeout", 10*time.Minute, "kill a run (and everything it started) after this long")
	maxOut := fs.Int("max-output", 16000, "bytes of the command's output (stdout, and stderr) kept for the answer")
	maxAge := fs.Duration("max-age", time.Hour, "leave alone a task older than this")
	every := fs.Duration("every", 15*time.Second, "how often the queue is read")
	allowUnsigned := fs.Bool("allow-unsigned", false, "also take tasks that carry no signature (senders on clients older than 0.7.0); a bad signature or a changed key is never taken")
	settle := fs.Duration("settle", 2*time.Second, "after claiming, how long to wait for a claim made elsewhere at the same moment")
	write := fs.Bool("write", false, "let the agent change files: each task runs in a new git worktree of --repo on branch aivoicebus/task-ID (not pushed), only after --approver approved it")
	repo := fs.String("repo", "", "with --write: the git repository a task's worktree is made from (its HEAD)")
	approver := fs.String("approver", "", "with --write: who approves each task with `aivoicebus approve ID` (a machine, machine/tool or one session; signed)")
	approveWithin := fs.Duration("approve-within", 30*time.Minute, "with --write: how long to wait for the approval before answering that it did not run")
	phoneURL := fs.String("phone-url", "", "with --write and a notify URL: the node the phone's Approve/Deny buttons call (default: the first node; the phone must reach it, e.g. on the tailnet)")
	approverKey := fs.String("approver-key", "", "with --write: the approver key's public half (`aivoicebus approver show` where the approver is); only decisions signed with it count")
	sandbox := fs.Bool("sandbox", true, "run each command without access to this user's secrets (~/.ssh, cloud and git credentials, keychains, browser profiles, the aivoicebus token and keys): sandbox-exec on macOS, bubblewrap on Linux; --sandbox=false runs it as it is")
	budget := fs.Int("budget", 0, "tokens (in and out, as the agents report them) the runs may use in any 24 hours; over it the worker takes no task and alerts (0: no budget)")
	watch := fs.Duration("watch", 0, "also alert (see `aivoicebus watch`) when a task this machine sent waits untaken this long, e.g. 15m, or was taken and then silent for 10m")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return outbox.Usagef("usage: worker --exec 'COMMAND' [--once] [--dry-run] [--timeout 10m] [--max-output 16000] [--max-age 1h]   |   worker allow add|rm|list [SENDER]")
	}
	var argv []string
	format := worker.FormatText // what the command prints: a preset's JSON carries the token counts
	if *agent != "" {
		if *execT != "" {
			return outbox.Usagef("--agent and --exec both name the command; give one")
		}
		a, ok := worker.PresetArgv(strings.ToLower(*agent))
		if !ok {
			return outbox.Usagef("--agent is one of %s", strings.Join(worker.PresetNames(), ", "))
		}
		note := worker.Presets[strings.ToLower(*agent)].Note
		format = worker.Presets[strings.ToLower(*agent)].Format
		if *write {
			wp, ok := worker.WritePresets[strings.ToLower(*agent)]
			if !ok {
				return outbox.Usagef("--write has presets for claude, codex and gemini; for another agent name the command with --exec")
			}
			a, note, format = slices.Clone(wp.Argv), wp.Note, wp.Format
		}
		argv = a
		fmt.Fprintf(s.Err, "preset %s: %s (%s)\n", *agent, worker.QuoteCommand(argv), note)
	} else if *execT != "" || !*dry {
		var err error
		if argv, err = worker.SplitCommand(*execT); err != nil {
			return outbox.Usagef("%v", err)
		}
	}
	if *timeout < time.Second || *maxOut < 1 || *maxOut > msg.MaxBody-1024 || *every < time.Second {
		return outbox.Usagef("--timeout and --every are at least 1s, and --max-output is 1 to %d bytes", msg.MaxBody-1024)
	}
	allow, err := worker.LoadAllow()
	if err != nil {
		return err
	}
	if allow.Empty() && !*dry {
		return fmt.Errorf("no sender is allowed, so the worker would run nothing: allow one with `aivoicebus worker allow add MACHINE` (or MACHINE/SESSION). The worker runs %s on what they send", worker.QuoteCommand(argv))
	}
	// The worker is a session of its own (machine/worker), never one of the agent sessions on
	// this machine, unless AIVOICEBUS_ME names it on purpose.
	env := os.Environ()
	if os.Getenv("AIVOICEBUS_ME") == "" {
		name := "worker"
		if *agent != "" { // host/codex.worker: reached by tasks sent to host/codex as well as to host
			name = strings.ToLower(*agent) + ".worker"
		}
		os.Setenv("AIVOICEBUS_AGENT", name)
		os.Unsetenv("AIVOICEBUS_AGENT_SRC")
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	c.Title = "aivoicebus worker"
	c.Labels = identity.Labels("") // tasks routed by selector (@tool=codex...) reach it too
	w := &worker.Worker{Format: format,
		C: c, Me: identity.Me(), Allow: allow, Argv: argv, Env: env,
		Limits: worker.Limits{Timeout: *timeout, MaxOutput: *maxOut},
		MaxAge: *maxAge, Settle: *settle, DryRun: *dry, Out: s.Out, AllowUnsigned: *allowUnsigned,
	}
	w.Budget = *budget
	if *sandbox {
		home, _ := os.UserHomeDir()
		if _, how, ok := worker.Sandboxed(argv, home); ok {
			w.Sandbox = true
			fmt.Fprintf(s.Err, "sandbox: %s\n", how)
		} else {
			fmt.Fprintln(s.Err, "[aivoicebus] warning: no sandbox on this system (no sandbox-exec, no bwrap): runs can read this user's secrets. Install bubblewrap, or say --sandbox=false to accept it.")
		}
	}
	if nu := notifyURL(); nu != "" {
		w.Alert = func(text string) { _ = tasks.Notify(nu, text) }
	}

	if *write {
		if *repo == "" || *approver == "" || *approverKey == "" {
			return outbox.Usagef("--write needs --repo PATH (a git repository), --approver WHO and --approver-key KEY (`aivoicebus approver init` there, then `approver show`): no task changes files without a person saying yes")
		}
		ak, err := base64.StdEncoding.DecodeString(*approverKey)
		if err != nil || len(ak) != ed25519.PublicKeySize {
			return outbox.Usagef("--approver-key is the base64 public key `aivoicebus approver show` prints")
		}
		if err := worker.CheckEntry(*approver); err != nil {
			return outbox.Usagef("--approver: %v", err)
		}
		abs, err := filepath.Abs(*repo)
		if err != nil {
			return err
		}
		if out, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output(); err != nil {
			return outbox.Usagef("--repo %s is not a git repository", abs)
		} else {
			abs = strings.TrimSpace(string(out))
		}
		if *approveWithin < time.Minute {
			return outbox.Usagef("--approve-within is at least 1m")
		}
		root, _ := os.Getwd()
		wm := &worker.WriteMode{Repo: abs, Root: root, Approver: *approver, ApproverKey: ak, Within: *approveWithin}
		if nu := notifyURL(); nu != "" {
			wm.Notify = func(text, actions string) { _ = tasks.NotifyActions(nu, "aivoicebus: approval needed", text, actions) }
			wm.PhoneURL = *phoneURL
			if wm.PhoneURL == "" && len(c.URLs) > 0 {
				wm.PhoneURL = c.URLs[0] // a node on the tailnet: the phone reaches it when it is on the tailnet too
			}
		}
		w.Write = wm
		fmt.Fprintf(s.Err, "[aivoicebus] write mode: tasks change files in worktrees of %s, each only after %s approves it.\n", abs, *approver)
	}
	if *dry {
		fmt.Fprintf(s.Out, "worker %s, dry run: nothing is claimed or run. Allowed senders: %s\n", w.Me, allowedText(allow))
		n, err := w.Pass(context.Background())
		if err == nil && n == 0 {
			fmt.Fprintln(s.Out, "(no waiting task would run)")
		}
		return err
	}
	dir := config.Dir()
	if dir == "" {
		return errors.New("no home directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, ok := config.Lock(filepath.Join(dir, ".worker.lock"), 0)
	if !ok {
		return errors.New("another aivoicebus worker is running for this user: one at a time, so two runs never overlap")
	}
	defer unlock()
	if w.Log, err = worker.FileLog(worker.LogPath()); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(s.Err, "aivoicebus worker %s: runs %s (task text on stdin, at most %s, %d bytes of output kept) for tasks from %s.\n", w.Me, worker.QuoteCommand(argv), *timeout, *maxOut, allowedText(allow))
	fmt.Fprintf(s.Err, "[aivoicebus] This runs an agent on input from peers. Every run is logged in %s.\n", worker.LogPath())
	if *once {
		n, err := w.Pass(ctx)
		if err == nil && n == 0 {
			fmt.Fprintln(s.Out, "(no task to run)")
		}
		return err
	}
	w.Replaced = binaryReplaced()
	{
		wc, err := client.FromEnv() // the machine's own sends, not the worker's identity
		if err != nil {
			return err
		}
		var last time.Time
		w.Tick = func() { // once a minute: scheduled tasks that are due, and (--watch) stuck ones
			if time.Since(last) < time.Minute {
				return
			}
			last = time.Now()
			if _, paused := w.Paused(); !paused { // a pause holds schedules back too
				_, _ = runSchedules(wc, last, s.Out)
			}
			if *watch > 0 {
				_, _ = watchOnce(wc, *watch, tasks.QuietAfter, s.Out)
			}
		}
	}
	err = w.Loop(ctx, *every)
	if errors.Is(err, worker.ErrReplaced) {
		fmt.Fprintln(s.Err, "aivoicebus worker: "+err.Error()+" (a service does this by itself)")
		return exitError{exitReplaced}
	}
	return err
}

func allowedText(a worker.Allowlist) string {
	if a.Empty() {
		return "nobody (the allowlist is empty)"
	}
	return strings.Join(a.Entries(), ", ")
}

// workerAllowCmd edits the worker's allowlist: `worker allow add|rm SENDER`, `worker allow list`.
func workerAllowCmd(args []string, s Streams) error {
	usage := outbox.Usagef("usage: worker allow add SENDER | worker allow rm SENDER | worker allow list   (SENDER: a machine, e.g. laptop-b, or one session or tool there, e.g. laptop-b/claude)")
	if len(args) == 0 {
		return usage
	}
	switch {
	case args[0] == "list" && len(args) == 1:
		a, err := worker.LoadAllow()
		if err != nil {
			return err
		}
		if a.Empty() {
			fmt.Fprintf(s.Out, "(nobody: the worker runs nothing; add a sender with: aivoicebus worker allow add MACHINE)\n")
			return nil
		}
		for _, e := range a.Entries() {
			fmt.Fprintln(s.Out, e)
		}
		return nil
	case args[0] == "add" && len(args) == 2:
		if err := worker.AddAllow(args[1]); err != nil {
			return outbox.Usagef("%v", err)
		}
		config.Audit(identity.Me(), "worker allow add", strings.ToLower(args[1]))
		fmt.Fprintf(s.Out, "allowed %s: a worker on this machine runs its command on the tasks it sends (%s)\n", strings.ToLower(args[1]), worker.AllowPath())
		fmt.Fprintln(s.Err, "[aivoicebus] Sender names are self-declared: whoever holds the shared token can send as any name, so on such a mesh the allowlist stops mistakes, not token holders. Machine tokens (deploy/machine-token.sh) bind a sender to its machine.")
		return nil
	case args[0] == "rm" && len(args) == 2:
		found, err := worker.RemoveAllow(args[1])
		if err != nil {
			return err
		}
		if !found {
			return outbox.NotFoundf("%s is not in the allowlist", args[1])
		}
		config.Audit(identity.Me(), "worker allow rm", strings.ToLower(args[1]))
		fmt.Fprintf(s.Out, "removed %s\n", strings.ToLower(args[1]))
		return nil
	}
	return usage
}

// approveCmd and denyCmd answer a write-mode worker's "[approval needed]" message with a decision
// signed by the approver key, which needs its passphrase typed on a terminal: a person's step that
// an agent on the same machine cannot take, though it can sign anything with the machine key.
func approveCmd(args []string, s Streams) error { return answerApproval("approve", args, s) }
func denyCmd(args []string, s Streams) error    { return answerApproval("deny", args, s) }

// readPassphrase asks on the terminal with echo off (a variable so tests can answer it).
var readPassphrase = func(prompt string) (string, error) {
	line, err := readHidden(prompt, "-")
	if err != nil {
		return "", errors.New("no terminal here (this is typed by a person, at a terminal, never piped)")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func answerApproval(word string, args []string, s Streams) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return outbox.Usagef("usage: %s ID   (the id of the worker's [approval needed] message)", word)
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	ids := outbox.NewIDResolver(c, identity.Me())
	ids.Say = s.Err
	id, err := ids.Resolve(args[0])
	if err != nil {
		return err
	}
	ms, err := c.Thread(id)
	if err != nil {
		return err
	}
	var req msg.Message
	for _, m := range ms {
		if m.ID == id {
			req = m
		}
	}
	if req.ID == "" || !strings.HasPrefix(req.Body, "[approval needed]") {
		return outbox.Usagef("%s is not a worker's [approval needed] message", id)
	}
	fmt.Fprintln(s.Err, view.UntrustedBanner)
	fmt.Fprintf(s.Err, "%s asks, about this task:\n%s\n\n", view.Name(req.From, msg.MaxIdentity), view.OneLine(req.Body, 900))
	pass, err := readPassphrase(fmt.Sprintf("Approver passphrase, to %s it: ", word))
	if err != nil {
		return fmt.Errorf("%s needs a person at a terminal: %v", word, err)
	}
	key, err := config.OpenApprover(pass)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(key, msg.ApprovalStatement(req.ID, word, req.From))
	if err := sendWith([]string{"--re", req.ID, "--ack", msg.ApprovalBody(word, sig)}, s, true); err != nil {
		return err
	}
	config.Audit(identity.Me(), word, "approval request "+req.ID+" from "+req.From)
	return nil
}

// approverCmd makes and shows the approver key.
func approverCmd(args []string, s Streams) error {
	usage := outbox.Usagef("usage: approver init | show")
	if len(args) != 1 {
		return usage
	}
	switch args[0] {
	case "show":
		pub, err := config.ApproverPublic()
		if err != nil {
			return err
		}
		fmt.Fprintln(s.Out, base64.StdEncoding.EncodeToString(pub))
		return nil
	case "init":
		p1, err := readPassphrase(fmt.Sprintf("New approver passphrase (at least %d characters; only you should know it): ", config.MinPassphrase))
		if err != nil {
			return fmt.Errorf("approver init needs a person at a terminal: %v", err)
		}
		p2, err := readPassphrase("Again: ")
		if err != nil {
			return err
		}
		if p1 != p2 {
			return errors.New("the two passphrases differ; nothing was made")
		}
		pub, err := config.CreateApprover(p1)
		if err != nil {
			return err
		}
		config.Audit(identity.Me(), "approver init", "approver key "+base64.StdEncoding.EncodeToString(pub))
		b := base64.StdEncoding.EncodeToString(pub)
		fmt.Fprintf(s.Out, "approver key made (%s, sealed with your passphrase). Give a write-mode worker its public half:\n  aivoicebus worker install --agent codex --write --repo PATH --approver %s --approver-key %s\n", config.ApproverPath(), identity.Me(), b)
		return nil
	}
	return usage
}
