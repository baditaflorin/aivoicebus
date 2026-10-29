package cli

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/updater"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Automatic update. Off by default. Turning it on is the person's, once per machine, as approving
// and resuming are: it refuses in an agent's session (personOnly) and asks for a word typed at a
// terminal, which an agent's tool shell does not have. Turning it off is anyone's, as pausing is:
// stopping is always safe. With it on, the session-start hook starts `update --auto run` at most
// once a day while the nodes hold a newer release (hook/release.go): the verified update path of
// `aivoicebus update`, never forced, rolled back when the new binary's setup fails, and never again
// to a release the person rolled back from.

const autoUsage = "usage: update --auto on|off|status|run   (on is the person's, typed at a terminal; run is what the session-start hook starts)"

// autoUpdateCmd is `update --auto WORD`.
func autoUpdateCmd(word string, s Streams) error {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "on":
		return autoOn(s)
	case "off":
		return autoOff(s)
	case "status":
		fmt.Fprintln(s.Out, autoStatusText(config.LoadAutoUpdate(), config.LoadAutoState(), time.Now()))
		return nil
	case "run":
		return autoRun(s)
	}
	return outbox.Usagef(autoUsage)
}

// autoOn turns it on: in the person's own terminal only.
func autoOn(s Streams) error {
	if err := personOnly("update --auto on"); err != nil {
		return err
	}
	if a := config.LoadAutoUpdate(); a.On {
		fmt.Fprintf(s.Out, "auto-update is already on (since %s): nothing changed\n", view.When(a.At, time.Now()))
		return nil
	}
	line, err := readPassphrase("Type auto to let this machine install newer signed releases by itself (aivoicebus update --auto off stops it): ")
	if err != nil {
		return fmt.Errorf("update --auto on needs a person at a terminal: %v", err)
	}
	if strings.TrimSpace(line) != "auto" {
		return errors.New("auto-update not turned on: run it again and type auto")
	}
	if err := config.SaveAutoUpdate(config.AutoUpdate{On: true, At: time.Now().UTC(), By: msg.HumanOf(identity.Me())}); err != nil {
		return err
	}
	config.Audit(identity.Me(), "update --auto on", "")
	fmt.Fprintln(s.Out, "auto-update is on: a session start installs a newer signed release by itself, at most once a day (the same checks as aivoicebus update, the previous binary kept, put back if the new one's setup fails). aivoicebus update --auto off stops it.")
	return nil
}

// autoOff turns it off, from anywhere.
func autoOff(s Streams) error {
	if !config.LoadAutoUpdate().On {
		fmt.Fprintln(s.Out, "auto-update is already off: nothing changed")
		return nil
	}
	if err := config.SaveAutoUpdate(config.AutoUpdate{At: time.Now().UTC(), By: identity.Me()}); err != nil {
		return err
	}
	config.Audit(identity.Me(), "update --auto off", "")
	fmt.Fprintln(s.Out, "auto-update is off: this machine installs nothing by itself; a session start says once a day when it is behind")
	return nil
}

// autoStatusText is the setting and the last run in one line.
func autoStatusText(a config.AutoUpdate, st config.AutoState, now time.Time) string {
	if !a.On {
		return "auto-update: off (the person turns it on, once, in their own terminal: aivoicebus update --auto on)"
	}
	s := "auto-update: on since " + view.When(a.At, now)
	if a.By != "" {
		s += " (" + view.Name(a.By, msg.MaxIdentity) + ")"
	}
	switch {
	case st.Tried.IsZero():
		s += "; no run yet"
	case st.Result == "":
		s += "; a run started " + view.When(st.Tried, now) + " has not finished (or was stopped)"
	default:
		s += "; last run " + view.When(st.Tried, now) + ": " + autoResultText(st)
	}
	if st.Skip != "" {
		s += "; leaves out release " + st.Skip + " (rolled back here)"
	}
	return s
}

// autoResultText says what a finished run did.
func autoResultText(st config.AutoState) string {
	switch st.Result {
	case config.AutoInstalled:
		return "installed " + st.To + " (over " + orDash(st.From) + ")"
	case config.AutoRolledBack:
		return "installed " + st.To + ", then put " + orDash(st.From) + " back: " + view.OneLine(st.Error, 160)
	case config.AutoFailed:
		return "did not install " + orDash(st.To) + ": " + view.OneLine(st.Error, 160)
	case config.AutoSkipped:
		return "left out " + orDash(st.To) + " (rolled back here)"
	}
	return st.Result
}

// autoRun is one automatic update: what the session-start hook starts, as a process of its own,
// when the setting is on. It does nothing when the setting is off or another run holds the lock,
// and records what it did for the next session start and for status.
func autoRun(s Streams) error {
	if !config.LoadAutoUpdate().On {
		fmt.Fprintln(s.Out, "auto-update is off on this machine: nothing done (the person turns it on, in their own terminal: aivoicebus update --auto on)")
		return nil
	}
	unlock, ok := config.Lock(config.AutoLockPath(), 0)
	if !ok {
		fmt.Fprintln(s.Out, "another automatic update is running: nothing done")
		return nil
	}
	defer unlock()
	bin, err := installedBinary()
	if err != nil {
		return autoRecord(updater.Result{}, err)
	}
	src, err := releaseSource("auto")
	if err != nil {
		return autoRecord(updater.Result{}, err)
	}
	res, err := updater.Auto(updater.Options{
		Source: src, Trusted: trustedKeys(), Bin: bin, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Skip: config.LoadAutoState().Skip, Out: s.Out,
	})
	return autoRecord(res, err)
}

// autoRecord writes what a run did to the record (and the audit log) and returns its error.
func autoRecord(res updater.Result, err error) error {
	announced := config.LoadAutoState().To // what the nodes announced when the hook started this run
	result := config.AutoFailed
	switch {
	case res.RolledBack:
		result = config.AutoRolledBack
	case err != nil:
	case res.Updated:
		result = config.AutoInstalled
	case res.Skipped:
		result = config.AutoSkipped
	case release.Newer(announced, res.Current):
		// A node announces a release the node read does not hold (a release put on some nodes only).
		// Taken as up to date, the machine would stay behind and nobody would hear of it.
		err = fmt.Errorf("the nodes announce release %s but the one read holds %s: `aivoicebus mesh` shows which node runs and holds which", announced, orDash(res.Latest))
		res.Latest = announced
	default:
		result = config.AutoCurrent
	}
	why := ""
	if err != nil {
		why = view.OneLine(err.Error(), 300)
	}
	_ = config.EditAutoState(func(st *config.AutoState) bool {
		if st.Tried.IsZero() { // a run nobody stamped (by hand): it counts as today's
			st.Tried = time.Now().UTC()
		}
		if res.Current != "" { // a run that failed before it learned these keeps what the hook stamped
			st.From = res.Current
		}
		if res.Latest != "" {
			st.To = res.Latest
		}
		st.Result, st.Error, st.Told = result, why, false
		if res.RolledBack {
			st.Skip = res.Latest
		}
		return true
	})
	detail := result
	if res.Latest != "" {
		detail += " " + res.Latest + " (ran " + orDash(res.Current) + ")"
	}
	if why != "" {
		detail += ": " + why
	}
	config.Audit(identity.Me(), "update --auto run", detail)
	if err != nil {
		return fmt.Errorf("auto-update: %w", err)
	}
	return nil
}
