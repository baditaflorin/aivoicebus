package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/version"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// What a session start says about releases, and the automatic update it may start.
//
// The nodes announce the release they hold on every answer, so a machine that is behind needs no
// task on the mesh: the session start says so in one line for the person (the agent relays it), at
// most once a day, and the line stops by itself once the machine runs that release. Where the
// person turned on `aivoicebus update --auto` (once, at a terminal; see cli/autoupdate.go), the
// session start instead starts that update, at most once a day, as a process of its own, and says
// once what it did.

// noticeEvery is how often a machine that stays behind is reminded, whatever the release.
const noticeEvery = 24 * time.Hour

// autoEvery is how often an automatic update is tried while the machine stays behind.
const autoEvery = 24 * time.Hour

type noticeRecord struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
}

// shouldNotify reports whether the line about being behind may be said now, given when it was last
// said: at most once a day, a newer release included.
func shouldNotify(prev noticeRecord, now time.Time) bool {
	return now.Sub(prev.At) >= noticeEvery
}

// news is what kind of line releaseNews chose.
type news int

const (
	newsNone      news = iota
	newsBehind         // this machine is behind: counts against the once-a-day budget
	newsFailed         // the last automatic run did not install: counts too, and is about the run
	newsInstalled      // the last automatic run installed what runs here: said once, about the run
)

// releaseNews decides what a session start says about releases and whether it starts an automatic
// update, from the release the nodes announce (latest), the one that runs here, the setting, the
// last automatic run and when the line about being behind was last said. It reads and writes
// nothing (releaseNotice does).
func releaseNews(latest, here string, a config.AutoUpdate, st config.AutoState, prev noticeRecord, now time.Time) (line string, kind news, start bool) {
	if st.Result == config.AutoInstalled && !st.Told && st.To == here {
		return fmt.Sprintf("[aivoicebus] This machine installed release %s by itself (auto-update, which the person turned on; it ran %s, which `aivoicebus update --rollback` puts back). Mention it to the user in one line.", here, orUnknown(st.From)), newsInstalled, false
	}
	if !release.Newer(latest, here) {
		return "", newsNone, false
	}
	if a.On && release.ValidVersion(here) { // a dev build is never replaced by itself
		switch {
		case st.Failed() && !st.Told: // a failure is said once at least, a rollback included
			return failedLine(st), newsFailed, false
		case st.Skip != "" && !release.Newer(latest, st.Skip):
			return "", newsNone, false // rolled back here: the person's to install again
		case now.Sub(st.Tried) >= autoEvery:
			return "", newsNone, true
		case st.Failed() && shouldNotify(prev, now):
			return failedLine(st), newsFailed, false
		}
		return "", newsNone, false // tried today: a run is under way, or the next one is tomorrow
	}
	if !shouldNotify(prev, now) {
		return "", newsNone, false
	}
	return fmt.Sprintf("[aivoicebus] For the person, not the agent: the nodes have release %s, this machine runs %s. In your own terminal: aivoicebus update (it checks the signature first), or once: aivoicebus update --auto on, to have this machine install signed releases by itself. Agent: tell the user this line; run neither yourself.", latest, here), newsBehind, false
}

// failedLine tells the person that the last automatic run did not leave the release it went for.
func failedLine(st config.AutoState) string {
	what := fmt.Sprintf("auto-update did not install release %s (%s)", orUnknown(st.To), view.OneLine(st.Error, 160))
	if st.Result == config.AutoRolledBack {
		what = fmt.Sprintf("auto-update installed release %s, its setup failed, and it put %s back (%s); it leaves %s out from now on", orUnknown(st.To), orUnknown(st.From), view.OneLine(st.Error, 160), orUnknown(st.To))
	}
	return "[aivoicebus] For the person, not the agent: " + what + ". In your own terminal: aivoicebus update, which says why. Agent: tell the user this line; do not run it yourself."
}

func orUnknown(v string) string {
	if v == "" {
		return "an unknown release"
	}
	return v
}

// releaseNotice is releaseNews on this machine's records: it returns the line to say ("" for none,
// or during quiet hours, when nothing is said and so nothing is recorded as said) and whether this
// session start is the one that starts the automatic update, which it has then stamped as tried.
func releaseNotice(latest string, now time.Time, quiet bool) (string, bool) {
	dir := config.Dir()
	path := ""
	var prev noticeRecord
	if dir != "" {
		path = dir + "/update-notice"
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &prev)
		}
	}
	line, kind, start := releaseNews(latest, version.Release, config.LoadAutoUpdate(), config.LoadAutoState(), prev, now)
	if start {
		// Under the record's lock, so of several sessions starting at once only one starts a run.
		beforeStamp()
		start = false
		_ = config.EditAutoState(func(s *config.AutoState) bool {
			if now.Sub(s.Tried) < autoEvery {
				return false
			}
			skip := s.Skip
			*s = config.AutoState{Tried: now, From: version.Release, To: latest, Skip: skip}
			start = true
			return true
		})
	}
	if quiet || line == "" {
		return "", start
	}
	if kind == newsFailed || kind == newsInstalled {
		_ = config.EditAutoState(func(s *config.AutoState) bool { s.Told = true; return true })
	}
	if (kind == newsBehind || kind == newsFailed) && path != "" {
		if b, err := json.Marshal(noticeRecord{Version: latest, At: now}); err == nil && os.MkdirAll(dir, 0o700) == nil {
			_ = os.WriteFile(path, b, 0o644)
		}
	}
	return line, start
}

// beforeStamp runs between reading the record and stamping it: a test is the other session there.
var beforeStamp = func() {}

// startAuto starts `aivoicebus update --auto run` as a process of its own: in a session of its own
// (detach), with no standard streams, so it outlives the hook and the tool waits for none of it.
// It is the binary the hook runs, which setup puts at ~/.local/bin/aivoicebus. A variable so tests
// can see what would be started.
var startAuto = func() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "update", "--auto", "run")
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
