package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/status"
	"github.com/baditaflorin/aivoicebus/internal/version"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// statusCmd with no id is the home screen (see package status). With an id it answers "did it get
// through?" for a message this machine sent (or any it can see): who read it and when, who took it
// if it is a task, and how many replies it has.
func statusCmd(args []string, s Streams) error {
	fs := newFlagSet("status")
	asJSON := fs.Bool("json", false, "print the screen (or the receipts) as JSON")
	budget := fs.Duration("budget", status.DefaultBudget, "how long to wait for the nodes; what has not answered by then is shown as unknown")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		if *budget < 100*time.Millisecond || *budget > time.Minute {
			return outbox.Usagef("--budget is 100ms to 1m")
		}
		r := homeStatus(*budget)
		if *asJSON {
			return writeJSON(s.Out, r)
		}
		status.Render(s.Out, r)
		return nil
	}
	if fs.NArg() != 1 || !reMsgID.MatchString(fs.Arg(0)) {
		return outbox.Usagef("usage: status [--json]   (where this session stands), or: status MESSAGE_ID [--json]   (the id `send` or `task` printed)")
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	h, err := c.Health()
	if err == nil && !h.Has(msg.FeatureReceipts) {
		return fmt.Errorf("the node runs %s, which cannot report receipts yet (it needs 0.4.0); `aivoicebus thread %s` shows the replies", view.OneLine(h.Version, 20), fs.Arg(0))
	}
	r, err := c.Receipts(fs.Arg(0))
	if err != nil {
		if client.IsUnreachable(err) {
			return err
		}
		return outbox.NotFoundf("message %s was not found on the nodes (status takes the full id): check it with `aivoicebus inbox --all`", fs.Arg(0))
	}
	if *asJSON {
		return writeJSON(s.Out, r)
	}
	view.Receipts(s.Out, r)
	return nil
}

// homeStatus gathers the home screen for this session.
func homeStatus(budget time.Duration) status.Report {
	start := time.Now() // working out the identity (the process chain) is part of the budget
	o := status.Options{
		Start:  start,
		Me:     identity.Me(),
		Why:    func() string { return identity.Printable(identity.Explain().Why()) },
		Here:   version.Release,
		Budget: budget,
		Quiet:  config.LoadQuiet,
		Held:   heldCount,
		Worker: localWorker,
	}
	if c, err := sessionClient(); err == nil {
		o.Client = c
	}
	return status.Gather(o)
}

// homeText is the home screen as text, for the MCP tool.
func homeText() (string, error) {
	var b strings.Builder
	status.Render(&b, homeStatus(status.DefaultBudget))
	return b.String(), nil
}

// heldCount is how many messages were held back from agents and not released by the person.
func heldCount() int {
	n := 0
	for _, h := range config.ReadHeld() {
		if !config.Released(h.ID) {
			n++
		}
	}
	return n
}

// localWorker says whether this machine's worker runs: as the service (launchd or systemd), or by
// hand, which the worker's lock shows (a worker holds it while it runs).
func localWorker() *status.Worker {
	w := &status.Worker{Service: "not installed"}
	if home, err := os.UserHomeDir(); err == nil {
		file, _ := svcPaths(home)
		if _, err := os.Stat(file); err == nil {
			up, detail := svcRunning()
			w.Running, w.Detail, w.Service = up, detail, "stopped"
			if up {
				w.Service = "running"
			}
		}
	}
	if dir := config.Dir(); dir != "" && !w.Running {
		lock := filepath.Join(dir, ".worker.lock")
		if _, err := os.Stat(lock); err == nil { // never create the lock just to look
			if unlock, ok := config.Lock(lock, 0); ok {
				unlock()
			} else {
				w.Running, w.Detail = true, "started by hand"
			}
		}
	}
	return w
}
