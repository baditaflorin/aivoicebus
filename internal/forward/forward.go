// Package forward wakes an agent that cannot run hooks: it reads the messages and tasks addressed to
// one identity on this machine (host/grokbot) and POSTs each, once, to a webhook that starts the
// agent (a Grok Bot routine's). It is the session-start hook's job done from outside the session, with
// the hook's rules: payloads are labelled untrusted, no credential goes into one, a message that is
// held for the person is not forwarded, fyi is only counted, quiet hours and pauses hold the rest,
// and two brakes (wakes per hour, forwards per thread per hour) stop a conversation that loops.
package forward

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Defaults of the two brakes and of one wake.
const (
	DefaultMaxWakes     = 20 // POSTs an hour: each one starts an agent run
	DefaultMaxPerThread = 6  // messages of one conversation an hour
	DefaultBatch        = 5  // messages in one POST (the rest go in the next)
	DefaultBodyRunes    = 1500
	DefaultMinGap       = 5 * time.Second
	Window              = time.Hour
)

// Bus is what the forwarder needs of the nodes (a *client.Client).
type Bus interface {
	Inbox(me string, all bool) ([]msg.Message, error)
	AckVia(id, by, via string, chars int) error
}

// Forwarder forwards what arrives for Me to Post.
type Forwarder struct {
	Bus  Bus
	Me   string
	Post Poster
	Out  io.Writer // one line per decision, for a service log (never a body, a URL or a key)
	Log  func(Entry)
	Now  func() time.Time

	Release      string
	MaxWakes     int
	MaxPerThread int
	Batch        int
	BodyRunes    int
	MinGap       time.Duration
	Broadcasts   bool                 // also wake for a broadcast that is not a task or a reply
	FYI          bool                 // also wake for fyi messages
	Quiet        func(time.Time) bool // do-not-disturb: hold what is not urgent
	Replaced     func() bool          // the binary was replaced: Loop returns ErrReplaced between passes
	Retries      int                  // delivery attempts in one pass (default 3)
	RetryBase    time.Duration        // first wait between them (default 2s)
	Sleep        func(context.Context, time.Duration) bool

	mu       sync.Mutex
	noted    map[string]bool // decisions already said, so a poll loop says each once
	lastWake time.Time
	fails    int
	permFail bool
	retry    time.Duration // set by Pass: messages are left that a later pass may forward, so Loop must not wait for news
}

// ErrReplaced is Loop's answer when the binary was replaced on disk.
var ErrReplaced = errors.New("the forwarder's binary was replaced; start it again to run the new one")

// Entry is one line of ~/.config/aivoicebus/forward.log (JSON lines). It never holds a message body,
// a URL or a key: ids, who, sizes and a hash.
type Entry struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"` // sent, failed, skipped, held, braked
	IDs     []string  `json:"ids,omitempty"`
	From    []string  `json:"from,omitempty"`
	Why     string    `json:"why,omitempty"`
	Status  int       `json:"status,omitempty"`
	Bytes   int       `json:"bytes,omitempty"`
	SHA     string    `json:"sha256,omitempty"`
}

func (f *Forwarder) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Forwarder) say(format string, a ...any) {
	if f.Out != nil {
		fmt.Fprintf(f.Out, "%s "+format+"\n", append([]any{f.now().Format("2006-01-02 15:04:05")}, a...)...)
	}
}

// once says line (and logs e) the first time key comes up.
func (f *Forwarder) once(key string, e Entry, format string, a ...any) {
	f.mu.Lock()
	if f.noted == nil {
		f.noted = map[string]bool{}
	}
	if f.noted[key] {
		f.mu.Unlock()
		return
	}
	f.noted[key] = true
	if len(f.noted) > 2000 {
		f.noted = map[string]bool{key: true}
	}
	f.mu.Unlock()
	f.say(format, a...)
	f.log(e)
}

func (f *Forwarder) log(e Entry) {
	if f.Log != nil {
		e.At = f.now().UTC()
		f.Log(e)
	}
}

func (f *Forwarder) toldAs() string { return "forward:" + f.Me }

func (f *Forwarder) maxWakes() int     { return pos(f.MaxWakes, DefaultMaxWakes) }
func (f *Forwarder) maxPerThread() int { return pos(f.MaxPerThread, DefaultMaxPerThread) }
func (f *Forwarder) batch() int        { return pos(f.Batch, DefaultBatch) }
func (f *Forwarder) bodyRunes() int    { return pos(f.BodyRunes, DefaultBodyRunes) }
func pos(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// verdict says what the forwarder does with m: "" to forward it, else why not and whether it is
// acknowledged (dropped for good) or left unread.
type decision struct {
	why   string
	ack   string // "" leave unread; else the via to acknowledge it with
	quiet bool   // say nothing (a message of this machine's own, a tombstone)
}

func (f *Forwarder) judge(m msg.Message, now time.Time) decision {
	switch {
	case m.Retracted != nil:
		return decision{why: "withdrawn by its sender", ack: msg.ViaHookCount, quiet: true}
	case m.From == f.Me:
		return decision{why: "written by this forwarder's own identity", ack: msg.ViaHookCount, quiet: true}
	case config.Told(f.toldAs(), m.ID):
		return decision{why: "already forwarded", ack: msg.ViaHook, quiet: true}
	case msg.PersonOnly(m):
		return decision{why: "it is meant for the person on this machine (" + msg.PersonStep(m) + "), not for an agent"}
	}
	if why := msg.HoldReason(m); why != "" && !config.Released(m.ID) {
		config.NoteHeld(m.ID, why)
		return decision{why: "held for the user: " + why + " (aivoicebus held)"}
	}
	switch {
	case m.Kind == msg.KindTask && m.Depth > msg.MaxTaskDepth:
		return decision{why: fmt.Sprintf("it is a task %d deep (sent from the run of a task sent from ...): more than %d is a chain reaction", m.Depth, msg.MaxTaskDepth)}
	case m.Priority() == msg.PrioFYI && !f.FYI:
		return decision{why: "low priority (fyi): counted, not forwarded", ack: msg.ViaHookCount, quiet: true}
	case !f.Broadcasts && msg.IsBroadcast(m.To) && m.Kind != msg.KindTask && m.Re == "":
		return decision{why: "a broadcast that is neither a task nor a reply", quiet: true}
	case f.Quiet != nil && f.Quiet(now) && m.Priority() != msg.PrioUrgent:
		return decision{why: "quiet hours (aivoicebus quiet): held until they end"}
	}
	return decision{}
}

// Pass reads the inbox once and forwards what it may in one POST. It returns how many messages it
// forwarded. An error is a node or the webhook not answering: nothing was acknowledged, so the next
// pass tries again.
func (f *Forwarder) Pass(ctx context.Context) (int, error) {
	f.retry = 0
	ms, err := f.Bus.Inbox(f.Me, false)
	if err != nil {
		return 0, err
	}
	now := f.now()
	if p, until, on := msg.ActivePause(ms, now); on {
		f.retry = time.Minute
		f.once("pause:"+p.ID+until.String(), Entry{Outcome: "held", Why: "paused"}, "paused by %s until %s: forwarding nothing (aivoicebus resume, on that machine, lifts it)", view.Name(p.From, msg.MaxIdentity), view.When(until, now))
		return 0, nil
	}
	var cand []msg.Message
	for _, m := range ms {
		d := f.judge(m, now)
		if d.why == "" {
			cand = append(cand, m)
			continue
		}
		if d.ack != "" {
			_ = f.Bus.AckVia(m.ID, f.Me, d.ack, 0)
		}
		if !d.quiet {
			f.once("skip:"+m.ID+d.why, Entry{Outcome: "skipped", IDs: []string{m.ID}, From: []string{msg.SafeName(m.From)}, Why: d.why},
				"not forwarding %s from %s: %s", m.ID, view.Name(m.From, msg.MaxIdentity), d.why)
		}
	}
	if len(cand) == 0 {
		if f.Quiet != nil && f.Quiet(now) {
			f.retry = time.Minute // what quiet hours held is looked at again when they end
		}
		return 0, nil
	}
	sort.SliceStable(cand, func(i, j int) bool {
		if ui, uj := cand[i].Priority() == msg.PrioUrgent, cand[j].Priority() == msg.PrioUrgent; ui != uj {
			return ui
		}
		return cand[i].At.Before(cand[j].At)
	})
	wakes := loadWakes(now)
	if f.MinGap > 0 && now.Sub(f.lastWake) < f.MinGap {
		f.retry = f.MinGap - now.Sub(f.lastWake)
		return 0, nil
	}
	if len(wakes) >= f.maxWakes() {
		f.retry = time.Minute
		f.brake(cand, fmt.Sprintf("%d wakes in the last hour is the limit (--max-wakes)", f.maxWakes()), wakes, now)
		return 0, nil
	}
	var batch []msg.Message
	perThread := map[string]int{}
	for _, w := range wakes {
		perThread[w.thread]++
	}
	var over []msg.Message
	for _, m := range cand {
		if perThread[m.Thread] >= f.maxPerThread() {
			over = append(over, m)
			continue
		}
		if len(batch) < f.batch() {
			batch = append(batch, m)
			perThread[m.Thread]++
		}
	}
	if len(over) > 0 {
		f.retry = time.Minute
		f.brake(over, fmt.Sprintf("%d messages of one conversation in the last hour is the limit (--max-per-thread): is this a loop?", f.maxPerThread()), wakes, now)
	}
	if len(batch) == 0 {
		return 0, nil
	}
	if len(batch)+len(over) < len(cand) {
		f.retry = max(f.MinGap, time.Second) // more than one POST's worth waits
	}
	p, key := Build(f.Me, f.Release, batch, f.bodyRunes())
	body, err := Encode(p)
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256(body)
	e := Entry{IDs: ids(batch), From: froms(batch), Bytes: len(body), SHA: hex.EncodeToString(sum[:6])}
	if err := f.deliver(ctx, body, key); err != nil {
		e.Outcome, e.Why = "failed", err.Error()
		var pe *PostError
		if errors.As(err, &pe) {
			e.Status = pe.Status
		}
		f.log(e)
		f.say("not delivered (%d message(s)): %v", len(batch), err)
		return 0, err
	}
	f.fails, f.permFail, f.lastWake = 0, false, f.now()
	e.Outcome = "sent"
	f.log(e)
	f.say("woke %s with %d message(s): %s", f.Me, len(batch), strings.Join(e.IDs, " "))
	noteWake(now, batch)
	for _, m := range batch { // only now: a message is acknowledged once it was handed over
		config.NoteTold(f.toldAs(), m.ID)
		_ = f.Bus.AckVia(m.ID, f.Me, msg.ViaHook, len([]rune(m.Body)))
	}
	return len(batch), nil
}

func (f *Forwarder) brake(ms []msg.Message, why string, wakes []wake, now time.Time) {
	until := now.Add(time.Minute)
	if len(wakes) > 0 {
		until = wakes[0].at.Add(Window)
	}
	f.once("brake:"+why+":"+ms[0].ID, Entry{Outcome: "braked", IDs: ids(ms), Why: why},
		"holding %d message(s), first %s: %s; it goes on after %s", len(ms), ms[0].ID, why, view.When(until, now))
}

// deliver posts body, trying a few times (a 5xx, 429 or a timeout) with a growing wait.
func (f *Forwarder) deliver(ctx context.Context, body []byte, key string) error {
	tries, wait := pos(f.Retries, 3), f.RetryBase
	if wait <= 0 {
		wait = 2 * time.Second
	}
	var err error
	for i := 0; i < tries; i++ {
		if err = f.Post.Post(ctx, body, key); err == nil {
			return nil
		}
		var pe *PostError
		if ctx.Err() != nil || (errors.As(err, &pe) && pe.Permanent) || i == tries-1 {
			break
		}
		if errors.As(err, &pe) && pe.RetryAfter > 0 {
			wait = max(wait, min(pe.RetryAfter, time.Minute))
		}
		if !f.sleep(ctx, wait) {
			break
		}
		wait *= 2
	}
	f.fails++
	var pe *PostError
	f.permFail = errors.As(err, &pe) && pe.Permanent
	return err
}

func (f *Forwarder) sleep(ctx context.Context, d time.Duration) bool {
	if f.Sleep != nil {
		return f.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// Backoff is how long Loop waits after a failed pass: 5s doubling to 5 minutes, and at least
// 2 minutes after a refusal that waiting will not fix (a wrong key), so a bad setting is not hammered.
func (f *Forwarder) Backoff() time.Duration {
	d := 5 * time.Second
	for i := 1; i < f.fails && d < 5*time.Minute; i++ {
		d *= 2
	}
	d = min(d, 5*time.Minute)
	if f.permFail {
		d = max(d, 2*time.Minute)
	}
	return d
}

// Loop passes until ctx is done. Between passes it holds a read open on a node that can
// (msg.FeatureInboxWait), so the identity shows in who and ping as listening and a message is
// forwarded within a second; otherwise it passes every `every`.
func (f *Forwarder) Loop(ctx context.Context, every time.Duration) error {
	c, _ := f.Bus.(*client.Client)
	held := false
	if c != nil {
		if h, err := c.Health(); err == nil && h.Has(msg.FeatureInboxWait) {
			held = true
		}
	}
	var gen uint64
	hasGen := false
	for {
		if f.Replaced != nil && f.Replaced() {
			f.say("this binary was replaced on disk (an update): stopping, so the new one is started")
			return ErrReplaced
		}
		if held && !hasGen {
			if _, g, err := c.InboxWait(ctx, f.Me, 0, false, 0); err == nil {
				gen, hasGen = g, true
			} else {
				held = false
			}
		}
		pause := every
		if _, err := f.Pass(ctx); err != nil {
			hasGen = false
			switch {
			case ctx.Err() != nil:
				return nil
			case client.IsUnreachable(err):
				f.say("no node reachable (%v); trying again", err)
			}
			pause = f.Backoff()
		} else if f.retry > 0 {
			pause = f.retry
		} else if held && hasGen {
			start := time.Now()
			_, g, err := c.InboxWait(ctx, f.Me, gen, true, msg.MaxInboxWait)
			switch {
			case ctx.Err() != nil:
				return nil
			case err != nil:
				hasGen = false
			default:
				gen = g
				if time.Since(start) >= time.Second {
					continue
				}
			}
			pause = time.Second
		}
		if !f.sleep(ctx, pause) {
			return nil
		}
	}
}

func ids(ms []msg.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func froms(ms []msg.Message) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range ms {
		if n := msg.SafeName(m.From); !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// The wakes of the last hour are kept in a file, so a restart does not forget the brake.

const wakesFile = "forward-wakes"

type wake struct {
	at     time.Time
	thread string
}

var wakesMu sync.Mutex

func wakesPath() string {
	if d := config.Dir(); d != "" {
		return filepath.Join(d, wakesFile)
	}
	return ""
}

// loadWakes lists the messages forwarded within Window of now, oldest first: one line each,
// "unix-seconds thread".
func loadWakes(now time.Time) []wake {
	wakesMu.Lock()
	defer wakesMu.Unlock()
	return readWakes(now)
}

func readWakes(now time.Time) []wake {
	p := wakesPath()
	if p == "" {
		return nil
	}
	b, _ := os.ReadFile(p)
	var out []wake
	for _, l := range strings.Split(string(b), "\n") {
		secs, thread, ok := strings.Cut(strings.TrimSpace(l), " ")
		n, err := strconv.ParseInt(secs, 10, 64)
		if !ok || err != nil {
			continue
		}
		if at := time.Unix(n, 0); now.Sub(at) < Window && !at.After(now.Add(time.Minute)) {
			out = append(out, wake{at, thread})
		}
	}
	return out
}

// WakesIn is how many POSTs and messages went out within the last hour (for `forward status`).
func WakesIn(now time.Time) (posts, messages int) {
	ws := loadWakes(now)
	seen := map[int64]bool{}
	for _, w := range ws {
		seen[w.at.Unix()] = true
	}
	return len(seen), len(ws)
}

func noteWake(now time.Time, ms []msg.Message) {
	wakesMu.Lock()
	defer wakesMu.Unlock()
	p := wakesPath()
	if p == "" || os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	var b strings.Builder
	for _, w := range readWakes(now) {
		fmt.Fprintf(&b, "%d %s\n", w.at.Unix(), w.thread)
	}
	for _, m := range ms {
		fmt.Fprintf(&b, "%d %s\n", now.Unix(), m.Thread)
	}
	_ = os.WriteFile(p, []byte(b.String()), 0o600)
}

// LogPath is the forwarder's decision log.
func LogPath() string {
	if d := config.Dir(); d != "" {
		return filepath.Join(d, "forward.log")
	}
	return ""
}

const maxLog = 1 << 20

// FileLog appends entries to path as JSON lines (mode 0600), moving it to path.1 at 1 MiB.
func FileLog(path string) (func(Entry), error) {
	if path == "" {
		return nil, errors.New("no home directory for the forwarder log")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return func(e Entry) {
		if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
			return // a symlink in its place is refused
		} else if err == nil && fi.Size() > maxLog {
			_ = os.Rename(path, path+".1")
		}
		fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return
		}
		defer fh.Close()
		b, _ := json.Marshal(e)
		_, _ = fh.Write(append(b, '\n'))
	}, nil
}
