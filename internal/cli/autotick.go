package cli

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/version"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// The periodic auto-update check. The session-start hook starts an automatic update only when an
// agent session starts, so a machine where nobody opens one (a worker Mac, a build host, the Grok
// Bot box with its forwarder) never updated by itself. The long-running processes, `worker` and
// `forward --webhook`, now check too, about once an hour, and `update --auto tick` is the same check
// for cron or launchd on a machine that runs neither. It is the same automatic update (autoRun:
// signed, strictly newer, health-checked, rolled back) under the same rules (the setting, at most one
// try a day, never the release rolled back from), plus one: a machine waits its own share of a
// six-hour spread after it first sees a release, so the fleet does not install a new release at once.
// The run happens in the checking process; when it replaced the binary, the worker and the forwarder
// see that between passes and start the new one, as they already do after `aivoicebus update`.

const (
	autoTryEvery   = 24 * time.Hour   // as the hook's autoEvery: at most one try a day
	autoSpread     = 6 * time.Hour    // the fleet spread: each machine waits its own share of it
	autoCheckEvery = time.Hour        // how often a worker or a forwarder checks, give or take autoCheckSlack
	autoCheckSlack = 15 * time.Minute //
)

// autoHoldoff is how long this machine waits after it first sees a release: a fixed share of the
// spread, from its host name, so the machines of a fleet spread over the six hours and each one
// always waits the same.
func autoHoldoff(host string) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write([]byte(host))
	return time.Duration(h.Sum64()%uint64(autoSpread/time.Minute)) * time.Minute
}

// machineHoldoff is autoHoldoff for this machine; a variable so tests choose it.
var machineHoldoff = func() time.Duration { return autoHoldoff(identity.Host()) }

// autoTickDecide is whether a periodic check starts an automatic update now, and why not when it
// does not. latest is the release the nodes announce, here the one that runs here, st the record
// with this check's sighting already in it (Seen, SeenTo). It reads and writes nothing.
func autoTickDecide(latest, here string, a config.AutoUpdate, st config.AutoState, holdoff time.Duration, now time.Time) (bool, string) {
	switch {
	case !a.On:
		return false, "auto-update is off"
	case !release.ValidVersion(here):
		return false, "this is a build of its own (" + orDash(here) + "), not a release: auto-update leaves it alone"
	case latest == "":
		return false, "no node announced a release"
	case !release.Newer(latest, here):
		return false, "up to date: the nodes announce " + latest + ", this machine runs " + here
	case st.Skip != "" && !release.Newer(latest, st.Skip):
		return false, "release " + latest + " was rolled back here: `aivoicebus update` installs it by hand"
	case now.Sub(st.Tried) < autoTryEvery:
		return false, "a run was started " + view.When(st.Tried, now) + ": the next try is a day after it"
	case st.SeenTo == latest && now.Sub(st.Seen) < holdoff:
		return false, fmt.Sprintf("release %s seen %s: this machine waits until %s (its share of the fleet spread)", latest, view.When(st.Seen, now), st.Seen.Add(holdoff).Local().Format("15:04"))
	}
	return true, ""
}

// autoTick is one periodic check: it notes the sighting, decides under the record's lock (so a
// session start, a worker and a forwarder checking at once start one run), stamps the run it starts
// and runs it here. It returns whether it ran one and, when not, why.
func autoTick(latest, by string, now time.Time, s Streams) (bool, string, error) {
	a := config.LoadAutoUpdate()
	if !a.On {
		return false, "auto-update is off", nil
	}
	holdoff := machineHoldoff()
	start, why := false, ""
	err := config.EditAutoState(func(st *config.AutoState) bool {
		st.Checked, st.CheckedBy = now.UTC(), by
		if release.Newer(latest, version.Release) && st.SeenTo != latest {
			st.Seen, st.SeenTo = now.UTC(), latest
		}
		start, why = autoTickDecide(latest, version.Release, a, *st, holdoff, now)
		if start {
			st.Tried, st.From, st.To = now.UTC(), version.Release, latest
			st.Result, st.Error, st.Told = "", "", false
		}
		return true
	})
	if err != nil || !start {
		return false, why, err
	}
	fmt.Fprintf(s.Out, "%s auto-update (%s): the nodes announce release %s, this machine runs %s: installing it (signature checked first)\n",
		now.Format("2006-01-02 15:04:05"), by, latest, version.Release)
	err = autoRun(s)
	st := config.LoadAutoState()
	fmt.Fprintf(s.Out, "%s auto-update (%s): %s\n", time.Now().Format("2006-01-02 15:04:05"), by, autoResultText(st))
	return true, "", err
}

// announcedRelease is the release the nodes announce (every answer carries it): "" when no node answers.
func announcedRelease(c *client.Client) string {
	if c == nil {
		return ""
	}
	if _, err := c.Health(); err != nil {
		return ""
	}
	return c.Latest
}

// autoTicker is the periodic check for a long-running process's loop (its Tick): about once an hour
// (give or take a quarter of an hour, so machines started together drift apart), the first a few
// minutes after the process starts. While the setting is off it asks the nodes nothing.
func autoTicker(c *client.Client, by string, s Streams) func() {
	next := time.Now().Add(2*time.Minute + rand.N(13*time.Minute))
	return func() {
		now := time.Now()
		if now.Before(next) {
			return
		}
		next = now.Add(autoCheckEvery - autoCheckSlack + rand.N(2*autoCheckSlack))
		if !config.LoadAutoUpdate().On {
			return
		}
		latest := announcedRelease(c)
		if latest == "" {
			return
		}
		if ran, _, err := autoTick(latest, by, now, s); err != nil && !ran { // a run says its own result
			fmt.Fprintf(s.Out, "%s auto-update (%s): %s\n", now.Format("2006-01-02 15:04:05"), by, view.OneLine(err.Error(), 300))
		}
	}
}

// autoTickCmd is `update --auto tick`: one periodic check now, for cron or launchd on a machine that
// runs neither a worker nor a forwarder. It does nothing (and asks no node) while the setting is off.
func autoTickCmd(s Streams) error {
	if !config.LoadAutoUpdate().On {
		fmt.Fprintln(s.Out, "auto-update is off on this machine: nothing done (the person turns it on, in their own terminal: aivoicebus update --auto on)")
		return nil
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	latest := announcedRelease(c)
	if latest == "" {
		return fmt.Errorf("no node answered: nothing done (AIVOICEBUS_URL, or `aivoicebus status`)")
	}
	ran, why, err := autoTick(latest, "tick", time.Now(), s)
	if !ran && err == nil {
		fmt.Fprintln(s.Out, "nothing done: "+why)
	}
	return err
}
