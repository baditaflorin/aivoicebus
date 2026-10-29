package hook

import (
	"sort"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/version"
)

// What waits for the person on this machine (<machine>/human: a [token hand], an approval, a
// question, a [for the person] task from `fleet update`) is no agent's to take, and nobody reads
// as the person, so without a word from the hooks it waits unseen until the person happens to run
// `aivoicebus status`. Both hooks peek at it (never acknowledge it: it is the person's, not the
// session's) and print one line for the agent to relay; the session start every time, the prompt
// hook once per message per session (config.Told, which standby shares).

// peekRelease is the first node release that reads an inbox on someone's behalf without making
// that identity a session on its roster (msg.FeatureInboxPeek). An older node would list the
// person as present, so it is not asked.
const peekRelease = "0.11.0"

// nodePeeks reports whether a node that says it runs version v can be peeked: the release header
// of the answer just read, so knowing costs no extra request. A dev build is taken as current.
func nodePeeks(v string) bool {
	return v == "dev" || (release.ValidVersion(v) && !release.Newer(peekRelease, v))
}

// minPeek is the least time worth giving the peek; with less left the run would rather say nothing
// about the person than lose all it has to say to its deadline.
const minPeek = 150 * time.Millisecond

// personWaiting is what waits unread for the person on me's machine, addressed to them
// (<machine>/human: a message to the whole machine or to all reaches the agents' own inboxes, where
// the hook and standby already say it is the person's), urgent first, then oldest first. It is read
// right after c answered an inbox read (that answer's release header says whether the node can be
// peeked), within left. During quiet hours only urgent ones count, as with the asks; skip leaves
// out what this session already told the user about.
func personWaiting(c *client.Client, me string, left time.Duration, quiet bool, skip func(id string) bool) []msg.Message {
	if msg.IsHuman(me) || left < minPeek || !nodePeeks(c.Server.Version) {
		return nil
	}
	saved := c.Timeout
	c.Timeout = left
	ms, err := c.Peek(msg.HumanOf(me))
	c.Timeout = saved
	if err != nil {
		return nil
	}
	var out []msg.Message
	for _, m := range ms {
		if m.From == me || m.Retracted != nil || !msg.IsHuman(m.To) || (quiet && m.Priority() != msg.PrioUrgent) || (skip != nil && skip(m.ID)) || updateDone(m) {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ui, uj := out[i].Priority() == msg.PrioUrgent, out[j].Priority() == msg.PrioUrgent
		if ui != uj {
			return ui
		}
		return out[i].At.Before(out[j].At)
	})
	return out
}

// noteTold records that session me told its user about each of ms: after the hook's output was
// handed over, as acknowledgements are, so a run cut off by its deadline tells again next time.
func noteTold(me string, ms []msg.Message) {
	for _, m := range ms {
		config.NoteTold(me, m.ID)
	}
}

// updateDone reports whether m is a `fleet update` task (msg.UpdateTaskRelease) for a release this
// machine already runs: it asked for what is done, so it is not the person's any more.
func updateDone(m msg.Message) bool {
	v := msg.UpdateTaskRelease(m)
	return v != "" && release.ValidVersion(version.Release) && !release.Newer(v, version.Release)
}
