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

// watch tells you when a task this machine sent has sat untaken: once per task, as a line and,
// when a notify URL is set, as a POST to it (ntfy and most webhook relays take that), so nobody
// has to keep asking "did anyone get it?".

const (
	notifyFile  = "notify-url" // ~/.config/aivoicebus/notify-url, mode 0600: the URL may be a secret
	alertedFile = "alerted"    // the tasks already alerted on, one id per line
	alertedKeep = 200
)

func notifyURL() string {
	if v := strings.TrimSpace(os.Getenv("AIVOICEBUS_NOTIFY_URL")); v != "" {
		return v
	}
	if b, err := os.ReadFile(filepath.Join(config.Dir(), notifyFile)); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

func loadAlerted() map[string]bool {
	out := map[string]bool{}
	b, _ := os.ReadFile(filepath.Join(config.Dir(), alertedFile))
	for _, l := range strings.Split(string(b), "\n") {
		if msg.ValidID(strings.TrimSpace(l)) {
			out[strings.TrimSpace(l)] = true
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

// watchOnce alerts on every task stuck longer than after that it has not alerted on before, and
// says how many it found.
func watchOnce(c *client.Client, after time.Duration, out io.Writer) (int, error) {
	now := time.Now()
	stuck, err := tasks.Unanswered(c, msg.HostOf(identity.Me()), after, now)
	seen := loadAlerted()
	nu := notifyURL()
	n := 0
	for _, st := range stuck {
		if seen[st.ID] {
			continue
		}
		n++
		line := tasks.AlertText(st, now)
		fmt.Fprintln(out, line)
		if nu != "" {
			if err := tasks.Notify(nu, line); err != nil {
				fmt.Fprintln(out, "  not notified: "+err.Error())
				continue // tried again next time
			}
		}
		noteAlerted(st.ID)
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
	once := fs.Bool("once", false, "look once and exit (exit 1 when a task is stuck)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 || *after < time.Minute || *every < 10*time.Second {
		return outbox.Usagef("usage: watch [--after 15m] [--every 1m] [--once]   |   watch notify set URL | clear | test   (--after at least 1m, --every at least 10s)")
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	if *once {
		n, err := watchOnce(c, *after, s.Out)
		if err != nil {
			return err
		}
		if n > 0 {
			return exitError{exitFailure}
		}
		fmt.Fprintf(s.Out, "nothing new: no task sent from this machine waits untaken past %s, other than any already alerted on\n", after.String())
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(s.Err, "watching the tasks this machine sends: an alert when one waits %s untaken%s\n", after.String(), map[bool]string{true: ", posted to the notify URL", false: ""}[notifyURL() != ""])
	for {
		if _, err := watchOnce(c, *after, s.Out); err != nil && !client.IsUnreachable(err) {
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
