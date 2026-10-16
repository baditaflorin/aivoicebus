package hook

import (
	"net/url"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
)

// A hook that fails says nothing, which is right for a session but leaves a machine whose
// token was retired (or whose nodes are all down) deaf with nobody the wiser. So every
// inbox read leaves a note in ~/.config/aivoicebus/hook-status.json (read by `aivoicebus
// doctor`), and the one time the hook does speak is when that note shows a refused token
// or a run of failures: one fixed line, at most once a day.
const (
	// warnAfterFailures is how many failed runs in a row a hook puts up with in silence, and
	// warnAfterFailing how long the failing has to have lasted: a laptop that has just left
	// the wifi fails a dozen prompts in a minute and is nobody's problem. A refused token
	// needs no such patience: it will not fix itself.
	warnAfterFailures = 3
	warnAfterFailing  = 30 * time.Minute
	// warnEvery is how often the warning line may be printed.
	warnEvery = 24 * time.Hour
	// noteBudget is how long the hook waits for the note's file I/O, each time it touches it.
	// The note lives on the home directory, which may be on a stalled network mount: such a
	// disk costs the session this much, not its whole deadline, and not its acknowledgements.
	noteBudget = 100 * time.Millisecond
	// statusRefresh is how stale an unchanged note may get before it is rewritten: the
	// per-prompt hook runs on every prompt, so a healthy machine writes it only this often.
	statusRefresh = 10 * time.Minute
)

// The warning lines are fixed text, never built from anything a peer or a node supplied:
// they are injected into an agent's context next to (not as) the messages.
const (
	warnRejected    = "[aivoicebus] The mesh rejected this machine's token, so no messages reach this machine. Tell the user once; `aivoicebus doctor` says what to fix. Do not change tokens or settings without their OK."
	warnUnreachable = "[aivoicebus] The mesh has been unreachable from this machine for several runs in a row, so messages may be missing. Tell the user once; `aivoicebus doctor` says what to check."
)

// outcomeOf says what an inbox read's result means for the note.
func outcomeOf(err error) string {
	switch {
	case err == nil:
		return config.HookOK
	case client.IsRejected(err):
		return config.HookRejected
	}
	return config.HookUnreachable
}

// advance is the note after one more run ended in outcome. line is the warning that run
// must print ("" for none); save says whether the note differs enough from prev to be written.
func advance(prev config.HookStatus, outcome, node string, now time.Time) (next config.HookStatus, line string, save bool) {
	next = config.HookStatus{LastRun: now, Outcome: outcome, Node: node, LastOK: prev.LastOK, WarnedAt: prev.WarnedAt, FailingSince: prev.FailingSince}
	if outcome == config.HookOK {
		next.LastOK = now
		next.FailingSince = time.Time{}
	} else {
		next.Failures = prev.Failures + 1
		if prev.Failures == 0 || prev.FailingSince.IsZero() || prev.FailingSince.After(now) {
			next.FailingSince = now
		}
	}
	// A stamp in the future (the clock was set back) must not silence the warning for good.
	due := prev.WarnedAt.IsZero() || prev.WarnedAt.After(now) || now.Sub(prev.WarnedAt) >= warnEvery
	switch {
	case !due:
	case outcome == config.HookRejected:
		line = warnRejected
	case outcome == config.HookUnreachable && next.Failures >= warnAfterFailures && now.Sub(next.FailingSince) >= warnAfterFailing:
		line = warnUnreachable
	}
	if line != "" {
		next.WarnedAt = now
	}
	// A healthy machine asks on every prompt: write only when something besides the time moved.
	save = line != "" || next.Outcome != prev.Outcome || next.Failures != prev.Failures ||
		next.Node != prev.Node || now.Sub(prev.LastRun) >= statusRefresh || prev.LastRun.After(now)
	return next, line, save
}

// nodeHost is host:port of the first node, and nothing else of its URL (it may carry
// credentials or a path).
func nodeHost(urls []string) string {
	if len(urls) == 0 {
		return ""
	}
	u, err := url.Parse(urls[0])
	if err != nil {
		return ""
	}
	return u.Host
}

// meshNote follows one hook run's inbox read. The run asks, the hook's own output goes out,
// and only then is the note written (flush), so a slow disk never delays what the session
// is waiting for. The exception is the warning line: its stamp is written before the line is
// printed, so a home that cannot be written to never turns a daily line into one per prompt.
//
// The read ends in exactly one of two ways, and whichever claims it (answered, or giveUp at
// the deadline) owns the note from then on. Neither mu nor wmu is ever held across file I/O,
// and every touch of the file is bounded by noteBudget (see within), so a note on a stalled
// disk can keep neither the output nor the acknowledgements waiting.
type meshNote struct {
	// prompt marks the per-prompt hook. Its budget is too short to tell a slow node from a
	// dead one, so a read that failed to answer is not a failure it records: only an answer
	// (ok or a refusal) is, and a refused token is just as fast to see in 500 ms as in 2.5 s.
	prompt bool

	mu        sync.Mutex
	asking    bool // an inbox read is in flight
	abandoned bool // the hook gave up on it
	path      string
	node      string

	wmu  sync.Mutex        // guards next and save: a straggler of a stalled disk may set them late
	next config.HookStatus // what to write
	save bool
}

// within runs f for at most noteBudget and returns what it returned, or "" if the disk made
// it late. The late f goes on in the background, and is harmless: it only sets the note.
func within(f func() string) string {
	res := make(chan string, 1)
	go func() { res <- f() }()
	select {
	case s := <-res:
		return s
	case <-time.After(noteBudget):
		return ""
	}
}

// ask marks that the inbox is about to be read from c's nodes.
func (n *meshNote) ask(c *client.Client) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.abandoned {
		return
	}
	n.path, n.node, n.asking = config.HookStatusPath(), nodeHost(c.URLs), true
}

// claim takes the read for the caller, who owns the note if it returns true.
func (n *meshNote) claim(giveUp bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	was := n.asking
	n.asking = false
	n.abandoned = n.abandoned || giveUp // whatever the abandoned run does later is ignored
	return was
}

// answered takes the result of that read and returns the warning line to print, if one is due.
func (n *meshNote) answered(err error) string {
	if !n.claim(false) {
		return "" // not asked, or the hook already gave up on this read
	}
	outcome := outcomeOf(err)
	if n.prompt && outcome == config.HookUnreachable {
		return ""
	}
	now := clock() // read here, not in the closure: a straggler of a stalled disk reads nothing shared
	return within(func() string { return n.record(outcome, now) })
}

// giveUp is for a hook that ran out of time with the read still in flight: the nodes did
// not answer in time, which counts as unreachable (but see prompt). It writes at once (the
// process is about to end) and returns the warning line to print, if one is due.
func (n *meshNote) giveUp() string {
	if !n.claim(true) || n.prompt {
		return ""
	}
	now := clock()
	return within(func() string {
		line := n.record(config.HookUnreachable, now)
		_ = n.write()
		return line
	})
}

// flush writes the note, if it needs writing, once the hook's output is out. A failure is
// ignored: the note is a convenience, never a reason for a hook to fail or wait.
func (n *meshNote) flush() {
	within(func() string { _ = n.write(); return "" })
}

// record works out the note for this run and returns the warning line it calls for.
func (n *meshNote) record(outcome string, now time.Time) string {
	if n.path == "" {
		return "" // no home to write to: no note, and no warning that could not be rate-limited
	}
	prev, _ := config.LoadHookStatus(n.path)
	next, line, save := advance(prev, outcome, n.node, now)
	n.wmu.Lock()
	n.next, n.save = next, save
	n.wmu.Unlock()
	if line != "" && n.write() != nil {
		return "" // a stamp that cannot be kept cannot rate-limit the line: say nothing
	}
	return line
}

// write saves the note if it is marked for saving.
func (n *meshNote) write() error {
	n.wmu.Lock()
	next, save := n.next, n.save
	n.save = false
	n.wmu.Unlock()
	if !save {
		return nil
	}
	return next.Save(n.path)
}
