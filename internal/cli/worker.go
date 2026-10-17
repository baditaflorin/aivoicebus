package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/worker"
)

// workerCmd runs a command for tasks sent to this machine while no session is open. It runs an
// agent on input from peers, so it does nothing until the user allows senders by name.
func workerCmd(args []string, s Streams) error {
	if len(args) > 0 && args[0] == "allow" {
		return workerAllowCmd(args[1:], s)
	}
	fs := newFlagSet("worker")
	execT := fs.String("exec", "", "the command to run for each task, e.g. 'claude -p'; the task text goes on its stdin, never on its command line (split like a shell would, but no shell runs it)")
	once := fs.Bool("once", false, "look at the queue once, run what may be run, and exit")
	dry := fs.Bool("dry-run", false, "say which waiting tasks the worker would run and which it would skip, and change nothing (no claim, no run)")
	timeout := fs.Duration("timeout", 10*time.Minute, "kill a run (and everything it started) after this long")
	maxOut := fs.Int("max-output", 16000, "bytes of the command's output (stdout, and stderr) kept for the answer")
	maxAge := fs.Duration("max-age", time.Hour, "leave alone a task older than this")
	every := fs.Duration("every", 15*time.Second, "how often the queue is read")
	settle := fs.Duration("settle", 2*time.Second, "after claiming, how long to wait for a claim made elsewhere at the same moment")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return outbox.Usagef("usage: worker --exec 'COMMAND' [--once] [--dry-run] [--timeout 10m] [--max-output 16000] [--max-age 1h]   |   worker allow add|rm|list [SENDER]")
	}
	var argv []string
	if *execT != "" || !*dry {
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
		os.Setenv("AIVOICEBUS_AGENT", "worker")
		os.Unsetenv("AIVOICEBUS_AGENT_SRC")
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	c.Title = "aivoicebus worker"
	w := &worker.Worker{
		C: c, Me: identity.Me(), Allow: allow, Argv: argv, Env: env,
		Limits: worker.Limits{Timeout: *timeout, MaxOutput: *maxOut},
		MaxAge: *maxAge, Settle: *settle, DryRun: *dry, Out: s.Out,
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
	return w.Loop(ctx, *every)
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
		fmt.Fprintf(s.Out, "removed %s\n", strings.ToLower(args[1]))
		return nil
	}
	return usage
}
