package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// The per-prompt hook is the answer to "an urgent message only reached an agent at
// its next session start". Every time the user sends a prompt it takes one very
// short look at the inbox and says something only if a message marked urgent has
// arrived since, or a reply to a message this very session sent (see isReplyCandidate): a
// session that asked something should hear the answer while it is still working on it.
// Nothing else is shown or acknowledged: other ask and fyi messages, broadcasts and
// selector messages wait for the next session start, so a busy session costs no extra
// tokens. That includes a plain retraction notice (ask priority, from the node); a
// leak notice is urgent and so arrives through the urgent path like any urgent message.
// What starts waiting for the person on this machine meanwhile (a [token hand], an approval) is
// urgent by nature: it gets the session start's one line, once per message per session (person.go).
var (
	// PromptDeadline bounds the whole check; past it the hook gives up without a word.
	PromptDeadline = 700 * time.Millisecond
	// promptEvery is how rarely a session is checked: a burst of prompts costs one look.
	promptEvery = 15 * time.Second
)

const maxPromptShown = 3

func runPromptEvent(args []string, stdin io.Reader, out io.Writer) {
	deadline, every := PromptDeadline, promptEvery // read once: the goroutine below may outlive this call
	var buf bytes.Buffer
	var pending *ackSet
	note := meshNote{prompt: true}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		pending = promptCheck(args, stdin, &buf, deadline, every, &note)
	}()
	select {
	case <-done:
		_, _ = out.Write(buf.Bytes())
		note.flush()                         // after the output: a slow disk never delays the prompt
		pending.send(300 * time.Millisecond) // only after the message has been handed over
	case <-time.After(deadline):
		if line := note.giveUp(); line != "" {
			fmt.Fprintln(out, line)
		}
	}
}

// promptCheck prints any urgent message and any reply to this session to out and returns
// the acknowledgements owed for them.
func promptCheck(args []string, stdin io.Reader, out io.Writer, deadline, every time.Duration, note *meshNote) *ackSet {
	start := time.Now()
	tool := toolArg(args)
	in := readInput(stdin)
	// The session-start hook exported the identity for the shell commands of this
	// session, not for hooks, so work it out the same way here.
	if os.Getenv("AIVOICEBUS_ME") == "" && os.Getenv("AIVOICEBUS_AGENT") == "" {
		os.Setenv("AIVOICEBUS_AGENT", sessionAgent(tool, in.SessionID))
		os.Setenv("AIVOICEBUS_AGENT_SRC", "hook")
	}
	me := identity.Me()
	if !promptDue(me, time.Now(), every) {
		return nil
	}
	c, err := client.FromEnv()
	if err != nil {
		return nil
	}
	c.Timeout = deadline - 150*time.Millisecond
	c.HedgeDelay = 120 * time.Millisecond
	c.Warn = io.Discard
	sessionModel(in, me, 256<<10)
	c.Labels = identity.Labels(in.CWD)
	c.Title = sessionTitle(in, me, 256<<10)
	note.ask(c)
	ms, err := c.Inbox(me, false)
	if line := note.answered(err); line != "" {
		fmt.Fprintln(out, line)
	}
	if err != nil {
		return nil
	}
	// Urgent first, as before; then a task the nodes offer to this session alone (msg/offer.go: the
	// session a person is working in is the one to ask now, not at its next start); then the replies
	// to this session, in arrival order.
	var urgent, offers, replies []msg.Message
	quiet := quietNow()
	var sent map[string]bool // read lazily: most prompts have no candidate
	for _, m := range ms {
		switch {
		case m.Priority() == msg.PrioUrgent:
			urgent = append(urgent, m)
		case m.Offer != nil && m.Priority() != msg.PrioFYI && !quiet:
			offers = append(offers, m)
		case isReplyCandidate(m, me) && m.Priority() != msg.PrioFYI && !quiet:
			if sent == nil {
				sent = outbox.SentBy(me)
			}
			if !sent[m.Re] {
				continue // not an answer to anything this session sent: it waits for the inbox
			}
			// fyi stays a count at session start, and quiet hours hold asks back, as there.
			replies = append(replies, m)
		}
	}
	listed := append(append(urgent, offers...), replies...)
	told := func(id string) bool { return config.Told(me, id) }
	ps := personWaiting(c, me, deadline-time.Since(start)-150*time.Millisecond, quiet, told)
	if len(listed) == 0 && len(ps) == 0 {
		return nil
	}
	acks := &ackSet{c: c, me: me, told: ps}
	if len(ps) > 0 {
		fmt.Fprintln(out, view.PersonWaiting(ps))
	}
	if len(listed) == 0 {
		return acks
	}
	shown := listed
	if len(shown) > maxPromptShown {
		shown = shown[:maxPromptShown]
	}
	switch {
	case len(offers) > 0:
		fmt.Fprintln(out, promptHeader(len(urgent), len(offers), len(replies)))
	case len(replies) == 0:
		fmt.Fprintf(out, "[aivoicebus] %d URGENT message(s) arrived while you were working. Tell the user now, then carry on with your task.\n", len(urgent))
	case len(urgent) == 0:
		fmt.Fprintf(out, "[aivoicebus] %d reply message(s) to this session arrived while you were working. Give the user a one-line summary, then carry on with your task.\n", len(replies))
	default:
		fmt.Fprintf(out, "[aivoicebus] %d URGENT message(s) and %d reply message(s) to this session arrived while you were working. Tell the user now, then carry on with your task.\n", len(urgent), len(replies))
	}
	view.ForAgent(out, shown, maxBody)
	acks.offered(out, shown)
	if more := len(listed) - len(shown); more > 0 {
		what := "urgent"
		if len(replies) > 0 || len(offers) > 0 {
			what = "unread"
		}
		fmt.Fprintf(out, "[aivoicebus] +%d more %s; `aivoicebus inbox` shows them when the user asks.\n", more, what)
	}
	for _, m := range shown {
		acks.add(m.ID, msg.ViaHook, injectedChars(m, nil)) // urgent, offers and replies to this session are never shown as a headline
	}
	return acks
}

// promptHeader is the prompt hook's first line when a task offered to this session is among what it
// shows (the lines for urgent messages and replies alone are written where they are used).
func promptHeader(urgent, offers, replies int) string {
	parts := []string{}
	if urgent > 0 {
		parts = append(parts, fmt.Sprintf("%d URGENT message(s)", urgent))
	}
	parts = append(parts, fmt.Sprintf("%d task(s) offered to this session alone", offers))
	if replies > 0 {
		parts = append(parts, fmt.Sprintf("%d reply message(s) to this session", replies))
	}
	advice := "Tell the user in one line, then carry on with your task."
	if urgent > 0 {
		advice = "Tell the user now, then carry on with your task."
	}
	return "[aivoicebus] " + strings.Join(parts, " and ") + " arrived while you were working. " + advice
}

// noticeFrom is the sender of the notices a node writes when a message was retracted (the
// msg.NoticeFrom). They wait for the session start unless they are urgent (a leak),
// whatever they refer to.
const noticeFrom = "aivoicebus/retraction"

// isReplyCandidate reports whether m could be a reply to this session: addressed to exactly
// this identity (never a broadcast or a selector, which wait for the next session start),
// naming the message it replies to (--re, which an answer must), and not a node's notice.
// It is only a candidate: the caller checks that this session really sent m.Re.
func isReplyCandidate(m msg.Message, me string) bool {
	return m.To == me && m.Re != "" && m.From != noticeFrom
}

const promptStateFile = "prompt-check.json"

// promptDue reports whether identity me is due a look (none in the last `every`), and records
// that one is being made. State is one small file; losing or corrupting it only means an extra look.
func promptDue(me string, now time.Time, every time.Duration) bool {
	dir := config.Dir()
	if dir == "" {
		return true
	}
	path := dir + "/" + promptStateFile
	state := map[string]int64{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &state)
	}
	if last, ok := state[me]; ok && now.Sub(time.Unix(0, last)) < every {
		return false
	}
	for k, v := range state { // forget sessions not seen for an hour
		if now.Sub(time.Unix(0, v)) > time.Hour {
			delete(state, k)
		}
	}
	state[me] = now.UnixNano()
	if b, err := json.Marshal(state); err == nil && os.MkdirAll(dir, 0o700) == nil {
		tmp := path + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, path)
		}
	}
	return true
}
