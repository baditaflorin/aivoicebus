package hook

import (
	"fmt"
	"io"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/release"
)

// maxSettlesPerRun bounds what one hook run writes for messages whose condition came true.
const maxSettlesPerRun = 10

// nodeSettles reports whether a node that says it runs version v applies a settle (msg.FeatureSettle),
// from the release header of an answer just read, so knowing costs no request. A dev build is current.
func nodeSettles(v string) bool {
	return v == "dev" || (release.ValidVersion(v) && !release.Newer(msg.SettleSince, v))
}

// settleMet writes the settle of each message whose done-when is true on this machine, and tells its
// sender with one progress line, so no later session is shown the message and its sender stops waiting.
// The hook already left them out of its output; against a node that does not apply settles it writes
// nothing (each session start would tell the sender again) and the next session works it out again.
func (a *ackSet) settleMet() {
	if a == nil || len(a.met) == 0 || !nodeSettles(a.c.Server.Version) {
		return
	}
	human := msg.HumanOf(a.me)
	for i, w := range a.met {
		if i == maxSettlesPerRun {
			return
		}
		if err := a.c.Settle(w.Msg.ID, human); err != nil {
			continue
		}
		config.Audit(a.me, "done-when", fmt.Sprintf("%s %s", w.Msg.ID, w.Cond))
		if msg.IsSystemSender(w.Msg.From) {
			continue
		}
		plan, err := outbox.Prepare(a.c, outbox.Request{To: w.Msg.From, Re: w.Msg.ID, Thread: w.Msg.Thread, Kind: msg.KindProgress, Body: msg.DoneBody(msg.DoneMetTag, ""), TargetSaid: true, From: human}, io.Discard)
		if err == nil {
			_, _ = plan.Send(a.c)
		}
	}
}
