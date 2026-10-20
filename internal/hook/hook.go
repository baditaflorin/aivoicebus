// Package hook is the session-start hook for agent tools
// (`aivoicebus hook --tool claude`). It gives the session its own identity
// (host/tool.session), and prints unread messages as context.
//
// It also names the other sessions that recently worked in the same repository
// (the collision radar, radar.go), so two agents notice before they edit the same files.
//
// It costs the agent nothing when there is nothing to say: no unread messages,
// an opted-out project and a slow network all produce no output, and it never
// fails the session. A mesh that rejects this machine's token, or that has not
// answered for a few runs, is the one thing it says something about (one fixed
// line a day) and notes in ~/.config/aivoicebus/hook-status.json, because a
// machine that has quietly gone deaf is worse than a line of noise.
package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/discovery"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/version"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

const (
	maxShown = 5                     // messages printed in full; the rest wait in the inbox
	maxBody  = view.SessionStartBody // runes of each body printed

	// OptOutFile in the working directory switches the hook off for a project.
	OptOutFile = ".aivoicebus-off"
)

// clockFn, when set, is the time quiet hours and the mesh note are judged by; tests set it with
// setClock. It is atomic: a hook's goroutine can outlive the call that started it (and the test).
var clockFn atomic.Pointer[func() time.Time]

func clock() time.Time {
	if f := clockFn.Load(); f != nil {
		return (*f)()
	}
	return time.Now()
}

// setClock makes clock call f (nil: the real time) and returns what puts the previous one back.
func setClock(f func() time.Time) (restore func()) {
	var p *func() time.Time
	if f != nil {
		p = &f
	}
	old := clockFn.Swap(p)
	return func() { clockFn.Store(old) }
}

// quietNow reports whether the do-not-disturb setting (`aivoicebus quiet`) is active. Every
// hook output that is not urgent asks it, so there is one setting and one place that reads it.
func quietNow() bool { return config.LoadQuiet().Active(clock()) }

// Deadline bounds the whole hook. Past it the hook gives up without a word, apart from the
// daily warning when the inbox read it gave up on was the third failure in a row.
var Deadline = 2500 * time.Millisecond

// Run executes the hook. args may carry `--tool <name>`; stdin is the tool's
// hook JSON (nil when there is none); output goes to out.
func Run(args []string, stdin io.Reader, out io.Writer) {
	if os.Getenv("AIVOICEBUS_OFF") != "" {
		return
	}
	if _, err := os.Stat(OptOutFile); err == nil {
		return
	}
	cw := &countWriter{w: out}
	out = cw
	event := "hook:start"
	if hookEvent(args) == "prompt" {
		event = "hook:prompt"
	}
	defer func() { config.NoteUsage(identity.Me(), event, cw.n) }() // what this run put into the session's context
	if tool := toolArg(args); tool == "gemini" || tool == "codex" {
		// Gemini CLI reads a hook's stdout as JSON only: text goes into the conversation as
		// hookSpecificOutput.additionalContext (anything else would be shown as a system message).
		// Codex takes plain text too, but reads output that starts with '[' or '{' as JSON, and
		// "[aivoicebus] ..." then fails as "invalid session start JSON output" and is dropped: it
		// gets the same JSON, with the hookEventName its schema requires.
		event := "SessionStart"
		if hookEvent(args) == "prompt" {
			event = "UserPromptSubmit"
		}
		out = jsonWriter{out, tool, event} // each run writes once, before it acknowledges anything
	}
	if hookEvent(args) == "prompt" {
		runPromptEvent(args, stdin, out)
		return
	}
	deadline := Deadline // read once: the goroutine below may outlive this call
	var buf bytes.Buffer
	var pending *ackSet
	var note meshNote
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		pending = run(args, stdin, &buf, deadline, &note)
	}()
	select {
	case <-done:
		_, _ = out.Write(buf.Bytes())
		note.flush()            // after the output: a slow disk never delays the session
		pending.send(ackBudget) // after the output: only what was written is acknowledged
		refreshNodes()          // after the output: a session start never waits on it
	case <-time.After(deadline):
		if line := note.giveUp(); line != "" {
			fmt.Fprintln(out, line)
		}
	}
}

// personPeekBudget bounds the session start's look at what waits for the person, and
// personPeekHeadroom is what it leaves of the hook's deadline for the rest of the run.
const (
	personPeekBudget   = 800 * time.Millisecond
	personPeekHeadroom = 400 * time.Millisecond
)

// ackBudget bounds the acknowledgements sent after a hook has written its output. A message
// is acknowledged only once it has been handed over, so a slow node costs an
// acknowledgement (the message is shown again next time), never a message nobody saw.
const ackBudget = 600 * time.Millisecond

// maxAcksPerRun caps acknowledgements per run; a backlog is cleared over several runs.
const maxAcksPerRun = 100

type ackJob struct {
	id, via string
	chars   int
}

// ackSet is the acknowledgements a hook owes for what it just printed.
type ackSet struct {
	c    *client.Client
	me   string
	jobs []ackJob
	told []msg.Message // what waits for the person and the output named (see person.go)
}

func (a *ackSet) add(id, via string, chars int) { a.jobs = append(a.jobs, ackJob{id, via, chars}) }

// send acknowledges everything in parallel, giving up on whatever is not done within budget.
func (a *ackSet) send(budget time.Duration) {
	if a == nil {
		return
	}
	noteTold(a.me, a.told) // only now that the output naming them was handed over
	if len(a.jobs) == 0 {
		return
	}
	jobs := a.jobs
	if len(jobs) > maxAcksPerRun {
		jobs = jobs[:maxAcksPerRun]
	}
	a.c.Timeout = budget // the run that used the client has finished
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, j := range jobs {
			sem <- struct{}{}
			wg.Add(1)
			go func(j ackJob) {
				defer wg.Done()
				defer func() { <-sem }()
				_ = a.c.AckVia(j.id, a.me, j.via, j.chars)
			}(j)
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(budget):
	}
}

// run prints what the session should see to out and returns the acknowledgements owed for it
// (nil when nothing was printed).
func run(args []string, stdin io.Reader, out io.Writer, timeout time.Duration, note *meshNote) *ackSet {
	start := time.Now()
	tool := toolArg(args)
	in := readInput(stdin)

	if os.Getenv("AIVOICEBUS_ME") == "" && os.Getenv("AIVOICEBUS_AGENT") == "" {
		agent := sessionAgent(tool, in.SessionID)
		os.Setenv("AIVOICEBUS_AGENT", agent)
		os.Setenv("AIVOICEBUS_AGENT_SRC", "hook")
		// Later shell commands in this session (Claude Code) inherit it. The source marks it as the
		// hook's, not the user's, so a different tool started from one of those commands (Codex
		// from a Claude Bash command) can still be told apart instead of inheriting this identity.
		exportToSession("export AIVOICEBUS_AGENT=" + agent)
		exportToSession("export AIVOICEBUS_AGENT_SRC=hook")
	}
	me := identity.Me()

	// The session's labels (repo, branch, tool, machine) are derived once, from
	// the directory the tool works in, and exported like the identity, so later
	// commands in the session declare the same labels wherever they run.
	sessionModel(in, me, 4<<20) // saved first, so the labels below carry the model the transcript names now
	labels := identity.Labels(in.CWD)
	delete(labels, msg.CapsLabel) // capabilities are not pinned: they follow AIVOICEBUS_CAPS as it changes
	exported := labels.Overlay(nil)
	delete(exported, msg.ModelLabel) // nor the model: a session can switch models while it runs
	if os.Getenv("AIVOICEBUS_LABELS") == "" && len(exported) > 0 {
		exportToSession("export AIVOICEBUS_LABELS='" + exported.String() + "'")
	}

	c, err := client.FromEnv()
	if err != nil {
		return nil
	}
	c.Timeout = timeout
	c.Warn = io.Discard // a session start has no use for "only 1 node took the write"
	c.Labels = labels
	c.Title = sessionTitle(in, me, 4<<20)
	// The collision radar reads the roster beside the inbox read and speaks last, whatever
	// the inbox said (see radar.go).
	var rad *radar // nil prints nothing: quiet hours silence the radar too
	if !quietNow() {
		rad = startRadar(labels, timeout, time.Now())
	}
	inboxStart := time.Now()
	var inboxErr error
	var inboxTook time.Duration
	defer func() { rad.print(out, me, labels, timeout, inboxErr != nil, inboxTook) }()
	note.ask(c)
	ms, err := c.Inbox(me, false)
	inboxErr, inboxTook = err, time.Since(inboxStart)
	if line := note.answered(err); line != "" {
		fmt.Fprintln(out, line)
	}
	if err != nil {
		return nil
	}
	// Asked first: updateNotice records that it spoke, and a notice held back must be said later.
	if !quietNow() {
		if notice := updateNotice(c.Latest, time.Now()); notice != "" {
			fmt.Fprintln(out, notice)
		}
	}
	acks := &ackSet{c: c, me: me}
	// What waits for the person on this machine: one line, every session start, since nothing else
	// tells them (see person.go). It keeps clear of the deadline, which would cost all the output.
	if ps := personWaiting(c, me, min(personPeekBudget, timeout-time.Since(start)-personPeekHeadroom), quietNow(), nil); len(ps) > 0 {
		fmt.Fprintln(out, view.PersonWaiting(ps))
		acks.told = ps
	}
	if len(ms) == 0 {
		return acks
	}
	// Urgent first, then ask; fyi is only counted, so low-value traffic costs one
	// short line instead of a message body.
	var urgent, ask, fyi []msg.Message
	for _, m := range ms {
		switch m.Priority() {
		case msg.PrioUrgent:
			urgent = append(urgent, m)
		case msg.PrioFYI:
			fyi = append(fyi, m)
		default:
			ask = append(ask, m)
		}
	}
	// Quiet hours hold the asks back: they are counted, not shown, and stay unread.
	var held []msg.Message
	if quietNow() && len(ask) > 0 {
		held, ask = ask, nil
	}
	// Within each class the narrowest audience comes first and broadcasts are capped (see
	// rank and choose), so a message to this session is not buried under greetings to all.
	listed := append(rank(urgent, me), rank(ask, me)...)
	shown, more, waiting := choose(listed, me)
	// A long or old broadcast costs one line, not a body (view.Headlines). The whole read decides:
	// a message left out of shown can still be the one that shows another is over.
	headline := view.Headlines(ms, clock())
	if len(shown) > 0 {
		advice := "Give the user a one-line summary, then carry on with your current task."
		if n := countUrgent(shown); n > 0 { // the ones the agent can see, not the ones left waiting
			advice = fmt.Sprintf("%d marked urgent: tell the user now, then carry on.", n)
		}
		fmt.Fprintf(out, "[aivoicebus] You are %s. %d unread message(s) from other agents. %s\n", me, len(listed), advice)
		view.ForAgentWith(out, shown, maxBody, headline)
		if more > 0 {
			fmt.Fprintf(out, "[aivoicebus] +%d more unread; `aivoicebus inbox` shows them when the user asks.\n", more)
		}
		if waiting > 0 {
			fmt.Fprintf(out, "[aivoicebus] +%d broadcast(s) waiting: `aivoicebus inbox` shows them when the user asks.\n", waiting)
		}
	}
	if len(held) > 0 {
		fmt.Fprintf(out, "[aivoicebus] %d message(s) held during quiet hours (`aivoicebus quiet`); `aivoicebus inbox` lists them when the user asks.\n", len(held))
	}
	if len(fyi) > 0 {
		fmt.Fprintf(out, "[aivoicebus] %d low-priority (fyi) message(s) arrived and were not shown; `aivoicebus inbox --all` lists them.\n", len(fyi))
	}
	// The receipts say what each message cost: the characters shown at session
	// start, or none for fyi, which is only counted.
	for _, m := range shown {
		acks.add(m.ID, msg.ViaHook, injectedChars(m, headline))
	}
	for _, m := range fyi {
		acks.add(m.ID, msg.ViaHookCount, 0)
	}
	return acks
}

// injectedChars is how many characters showing m at session start puts into the
// agent's context: its header line and its body, cut as BriefWith cuts it.
func injectedChars(m msg.Message, headline view.Headliner) int {
	var b strings.Builder
	view.BriefWith(&b, []msg.Message{m}, maxBody, headline)
	return utf8.RuneCountInString(b.String())
}

// exportToSession appends a shell line to $CLAUDE_ENV_FILE, which Claude Code
// sources before each later command of the session. Without that file (other
// tools) it does nothing.
func exportToSession(line string) {
	f := os.Getenv("CLAUDE_ENV_FILE")
	if f == "" {
		return
	}
	if fh, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		fmt.Fprintln(fh, line)
		fh.Close()
	}
}

// shortSession turns a session id into 4 lowercase alphanumerics.
func shortSession(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			if b.Len() == 4 {
				break
			}
		}
	}
	return b.String()
}

// noticeEvery is how often a machine that stays behind is reminded of a release.
const noticeEvery = 24 * time.Hour

type noticeRecord struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
}

// shouldNotify reports whether to mention release latest now, given what was
// last mentioned: a release is announced once, then again after noticeEvery.
func shouldNotify(prev noticeRecord, latest string, now time.Time) bool {
	return prev.Version != latest || now.Sub(prev.At) >= noticeEvery
}

// updateNotice is the one-line heads-up that a newer release exists ("" when
// this machine is current, the node announced nothing, or it was mentioned
// recently). The line is for the user: installing is `aivoicebus update`, which
// verifies a signed manifest, and only with the user's OK.
func updateNotice(latest string, now time.Time) string {
	if !release.Newer(latest, version.Release) {
		return ""
	}
	dir := config.Dir()
	var prev noticeRecord
	path := ""
	if dir != "" {
		path = dir + "/update-notice"
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &prev)
		}
	}
	if !shouldNotify(prev, latest, now) {
		return ""
	}
	if path != "" {
		if b, err := json.Marshal(noticeRecord{Version: latest, At: now}); err == nil && os.MkdirAll(dir, 0o700) == nil {
			_ = os.WriteFile(path, b, 0o644)
		}
	}
	return fmt.Sprintf("[aivoicebus] Release %s is available (this machine runs %s). Mention it to the user; `aivoicebus update` installs it after verifying a signed manifest. Do not run it without their OK.", latest, version.Release)
}

// trustedKeys supplies the keys that sign the node list; a variable only so tests can use their own.
var trustedKeys = release.TrustedKeys

// refreshNodes learns the mesh's signed node list when it is due (once a day, and
// at most once in six hours after a failure), so a node added after this machine
// was set up reaches it without anyone touching it. It runs after the hook has
// written its output and with a short timeout, so it costs a session start at most
// a fraction of a second, once a day.
func refreshNodes() { discovery.RefreshIfDue(trustedKeys(), time.Now(), discovery.StartBudget) }

// countWriter counts what passes through it.
type countWriter struct {
	w io.Writer
	n int
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}

// jsonWriter turns each Write into one toolJSON object. A hook run writes its output in one call,
// and only then acknowledges what it showed, so the wrapping must happen in that call, not later.
type jsonWriter struct {
	w           io.Writer
	tool, event string
}

func (j jsonWriter) Write(p []byte) (int, error) {
	toolJSON(j.w, j.tool, j.event, string(p))
	return len(p), nil
}

// toolJSON writes text the way a Gemini CLI or Codex hook must: one JSON object, nothing when there
// is nothing to say. Codex's schema requires hookEventName (SessionStart or UserPromptSubmit).
func toolJSON(out io.Writer, tool, event, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	o := map[string]any{"additionalContext": text}
	if tool == "codex" {
		o["hookEventName"] = event
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": o})
	_, _ = out.Write(append(b, '\n'))
}

// hookInput is what the tool passes the hook on stdin. Claude Code, Codex and Gemini CLI share
// these snake_case fields; Codex also says which model the session runs. Antigravity passes
// camelCase protojson fields (conversationId, workspacePaths, transcriptPath, modelName).
type hookInput struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	Model     string `json:"model"`
	// TranscriptPath is the session's transcript file, where Claude Code records the session's title.
	TranscriptPath string `json:"transcript_path"`

	ConversationID      string   `json:"conversationId"`
	WorkspacePaths      []string `json:"workspacePaths"`
	CamelTranscriptPath string   `json:"transcriptPath"`
	ModelName           string   `json:"modelName"`
}

func readInput(stdin io.Reader) hookInput {
	var in hookInput
	if stdin != nil {
		b, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
		_ = json.Unmarshal(b, &in)
	}
	if in.SessionID == "" {
		in.SessionID = in.ConversationID
	}
	if in.CWD == "" && len(in.WorkspacePaths) > 0 {
		in.CWD = in.WorkspacePaths[0]
	}
	if in.TranscriptPath == "" {
		in.TranscriptPath = in.CamelTranscriptPath
	}
	if in.Model == "" && in.ModelName != "" && !strings.EqualFold(in.ModelName, "auto") {
		in.Model = in.ModelName
	}
	return in
}

// toolArg is the value of --tool ("agent" when absent).
func toolArg(args []string) string {
	tool := "agent"
	for i, a := range args {
		if a == "--tool" && i+1 < len(args) {
			tool = strings.ToLower(args[i+1])
		}
	}
	return tool
}

// hookEvent is the value of --event ("start" when absent).
func hookEvent(args []string) string {
	for i, a := range args {
		if a == "--event" && i+1 < len(args) {
			return strings.ToLower(args[i+1])
		}
	}
	return "start"
}

// sessionAgent is the agent part of this session's identity: the tool, then the
// first four letters or digits of the session id, so sessions of one tool on one
// machine stay apart.
func sessionAgent(tool, sessionID string) string {
	if name := identity.NamedAgent(tool); name != "" { // a registered long-lived name (AIVOICEBUS_NAME) is this session's, restart after restart
		return name
	}
	if sid := shortSession(sessionID); sid != "" {
		return tool + "." + sid
	}
	return tool
}
