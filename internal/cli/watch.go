package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
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
	"github.com/baditaflorin/aivoicebus/internal/tasks"
)

// watch tells you when a task this machine sent has sat untaken, or was taken and then went silent
// (no progress, answer or end for --quiet-after), or when the deadline of a message it sent
// (send --reply-by, task --until) passed with nobody having read it, a reply in, or nobody having
// taken the offer: once per task and kind of alert, as a line and, when a notify URL is set, as a POST
// to it (ntfy and most webhook relays take that), so nobody has to keep asking "did anyone get it?"
// or "is anyone still on it?". A deadline that came out well (read, nobody replied; taken) is marked
// reported without an alert: `status ID` shows it.

const (
	notifyFile  = config.NotifyFile
	alertedFile = "alerted" // the tasks already alerted on, one id per line ("quiet:ID" for a silent one, "due:ID" for a deadline)
	quietKey    = "quiet:"
	dueKey      = "due:"
	alertedKeep = 200
)

func notifyURL() string { return config.NotifyURL() }

func loadAlerted() map[string]bool {
	out := map[string]bool{}
	b, _ := os.ReadFile(filepath.Join(config.Dir(), alertedFile))
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if msg.ValidID(strings.TrimPrefix(strings.TrimPrefix(l, quietKey), dueKey)) {
			out[l] = true
		}
	}
	return out
}

func noteAlerted(id string) {
	p := filepath.Join(config.Dir(), alertedFile)
	b, _ := os.ReadFile(p)
	lines := append(strings.Fields(string(b)), id)
	if len(lines) > alertedKeep {
		lines = lines[len(lines)-alertedKeep:]
	}
	_ = os.MkdirAll(config.Dir(), 0o700)
	_ = os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// watchOnce alerts on every task stuck longer than after (untaken) or silent for quiet (taken,
// then nothing; 0: not looked for), and on every deadline that passed unreported with an outcome a
// person should know, that it has not alerted on before, and says how many it found.
func watchOnce(c *client.Client, after, quiet time.Duration, out io.Writer) (int, error) {
	now := time.Now()
	host := msg.HostOf(identity.Me())
	type alert struct{ key, title, line string }
	var alerts []alert
	stuck, err := tasks.Unanswered(c, host, after, now)
	for _, st := range stuck {
		if _, ok := outbox.DeadlineFor(host, st.ID); ok {
			continue // an offer with its own deadline is reported at that deadline, not at --after
		}
		alerts = append(alerts, alert{st.ID, "aivoicebus: a task is waiting", tasks.AlertText(st, now)})
	}
	var due []tasks.DueReport
	if err == nil {
		due, err = tasks.Due(c, host, now)
		for _, r := range due {
			if tasks.Alarming(r.Judged) {
				alerts = append(alerts, alert{dueKey + r.Deadline.ID, "aivoicebus: a deadline passed", r.Line})
			}
		}
	}
	if quiet > 0 && err == nil {
		var silent []tasks.Silent
		silent, err = tasks.SilentTasks(c, host, quiet, now)
		for _, si := range silent {
			alerts = append(alerts, alert{quietKey + si.ID, "aivoicebus: a task went silent", tasks.SilentText(si, now)})
		}
	}
	seen := loadAlerted()
	nu := notifyURL()
	n := 0
	for _, r := range due {
		if !tasks.Alarming(r.Judged) { // read and nobody replied, or taken: good news needs no alert
			outbox.MarkReported(host, r.Deadline.ID, now)
		}
	}
	settled := func(key string) { // a deadline that was told about is not looked at again
		if id, ok := strings.CutPrefix(key, dueKey); ok {
			outbox.MarkReported(host, id, now)
		}
	}
	for _, a := range alerts {
		if seen[a.key] {
			settled(a.key)
			continue
		}
		n++
		fmt.Fprintln(out, a.line)
		if nu != "" {
			if err := tasks.NotifyActions(nu, a.title, a.line, ""); err != nil {
				fmt.Fprintln(out, "  not notified: "+err.Error())
				continue // tried again next time
			}
		}
		noteAlerted(a.key)
		settled(a.key)
	}
	return n, err
}

func watchCmd(args []string, s Streams) error {
	if len(args) > 0 && args[0] == "notify" {
		return watchNotifyCmd(args[1:], s)
	}
	fs := newFlagSet("watch")
	after := fs.Duration("after", 15*time.Minute, "alert on a task this machine sent that nobody took after this long")
	every := fs.Duration("every", time.Minute, "how often to look")
	quiet := fs.Duration("quiet-after", tasks.QuietAfter, "alert on a task this machine sent that someone took and then said nothing about (no progress, answer or end) this long; 0: never")
	once := fs.Bool("once", false, "look once and exit (exit 1 when a task is stuck or a deadline passed unread, objected to or untaken)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 || *after < time.Minute || *every < 10*time.Second || (*quiet != 0 && *quiet < time.Minute) {
		return outbox.Usagef("usage: watch [--after 15m] [--quiet-after 10m] [--every 1m] [--once]   |   watch notify set URL | clear | test   (--after and --quiet-after at least 1m, --every at least 10s)")
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	if *once {
		n, err := watchOnce(c, *after, *quiet, s.Out)
		if err != nil {
			return err
		}
		if n > 0 {
			return exitError{exitFailure}
		}
		fmt.Fprintf(s.Out, "nothing new: no task sent from this machine waits untaken past %s or taken and silent past %s, and no deadline passed with nobody having read it or taken it, other than any already alerted on\n", after.String(), quiet.String())
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(s.Err, "watching the tasks and deadlines this machine sends: an alert when a task waits %s untaken or %s silent after it was taken, or a --reply-by or --until deadline passes unread or untaken%s\n", after.String(), quiet.String(), map[bool]string{true: ", posted to the notify URL", false: ""}[notifyURL() != ""])
	for {
		if _, err := watchOnce(c, *after, *quiet, s.Out); err != nil && !client.IsUnreachable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*every):
		}
	}
}

// watchNotifyCmd sets, clears or tries the notify URL. The URL is kept in a file of mode 0600
// and never printed, since a notification URL (an ntfy topic, a webhook) is often a secret.
func watchNotifyCmd(args []string, s Streams) error {
	usage := outbox.Usagef("usage: watch notify set URL | clear | test")
	if len(args) == 0 {
		return usage
	}
	p := filepath.Join(config.Dir(), notifyFile)
	switch args[0] {
	case "set":
		if len(args) != 2 {
			return usage
		}
		u, err := url.Parse(args[1])
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return outbox.Usagef("the notify URL is an http(s) URL, e.g. https://ntfy.sh/YOUR-TOPIC")
		}
		if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(args[1]+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(s.Out, "notify URL saved in %s (mode 0600; host %s). Try it: aivoicebus watch notify test\n", p, u.Host)
		return nil
	case "clear":
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fmt.Fprintln(s.Out, "notify URL removed: alerts are printed only")
		return nil
	case "test":
		nu := notifyURL()
		if nu == "" {
			return errors.New("no notify URL: aivoicebus watch notify set URL")
		}
		if err := tasks.Notify(nu, "aivoicebus: this is a test alert from "+msg.SafeName(identity.Me())); err != nil {
			return err
		}
		fmt.Fprintln(s.Out, "sent a test alert to the notify URL")
		return nil
	}
	return usage
}
