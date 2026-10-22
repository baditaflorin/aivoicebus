// Package outbox turns "send this to that" into a checked message: it expands
// targets, refuses credentials, counts who the message would reach, estimates what
// it costs the readers, and stops obvious mistakes. The CLI and the MCP server
// both send through it, so every way of sending gets the same checks.
package outbox

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/secrets"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// How recently a session must have been seen to count as someone a message would reach.
const (
	ReachWindow     = 24 * time.Hour
	ReachWindowText = "24h"
)

// Guardrails: an urgent message to this many sessions or more interrupts them all,
// and a long body to this many readers floods their contexts. Force overrides both.
const (
	UrgentFanout = 5
	BulkReaders  = 3
	BulkChars    = 2000
)

// Request is what a sender asks for. Strings are as typed; Prepare validates them.
type Request struct {
	To, Body, Re, Thread, Prio, TTL, Kind string
	Queue                                 bool // allow a selector that matches nobody yet
	Force                                 bool // override the guardrails
	File                                  bool // the body points at a one-time file in the pipe: it lives minutes, so the reader must be around
	Keep                                  bool // a file that several sessions may read (not burned on the first read)
	Attach                                bool // the body carries a log link (a file sets File)
	TrustRole                             bool // use an unpinned role alias with a single claimant (--trust-role, AIVOICEBUS_TRUST_ROLES=1)
	// Authored: the sender wrote this message by hand (send, reply, task, the MCP send tool), so it is
	// also checked for an exact-session target nobody has seen lately and for a priority its own words
	// contradict (checks.go). The tool's own messages (claims, progress, worker results) leave it off.
	Authored   bool
	TargetSaid bool // the caller already told the sender whom a reply with no --to goes to
	// MaxTurns starts the thread with a turn limit (msg.Message.MaxTurns); 0 is none.
	MaxTurns int
	// Deadline starts the thread with a deadline (msg.Message.Deadline); 0 is none.
	Deadline time.Duration
	// BudgetChars starts the thread with a character budget (msg.Message.BudgetChars); 0 is none.
	BudgetChars int
	// From sends as another identity of this same machine (the scheduler's), signed with the
	// machine's key like any send; "" is this session. Another machine's name is refused.
	From string
	// Amending says the message replaces one already sent to this audience at this priority (Amend):
	// the urgent fan-out guard, which is about choosing to interrupt that many sessions, is not asked
	// again. The long-body guard is.
	Amending bool
	// Test sends the message in the test lane (msg/testlane.go): it reaches only sessions that opted in,
	// lives at most msg.TestLife and is purged. A reply to a test message is one without asking.
	Test bool
}

// Plan is a checked message and what is known about it before it goes out.
type Plan struct {
	Msg     msg.Message
	Reach   int  // sessions it would reach (valid when Known)
	Known   bool // the nodes could say who is around
	Readers int  // = Reach for bulletins, 1 for a message to one session
	Context int  // characters each reader's context receives at session start
	Notes   []string
	Alias   string              // how a role alias (@build) was resolved, "" when the target was not one
	Role    *msg.RoleResolution // the same, for output that is read by programs
	Expiry  time.Duration       // default lifetime that will apply; 0 when none
	Lowered bool                // the message said it needs nothing and no priority was chosen, so it goes as fyi (Authored)

	// Recipients names the sessions a message routed by capability (can=...) would reach, and
	// CapNote warns when only one or two of them declared it: capabilities are claimed by the
	// sessions themselves, so the sender checks who was resolved before sending anything sensitive.
	Recipients []string
	CapNote    string
	ByCaps     bool // the target routes by capability: callers that cannot show the recipients first ask for a confirmation

	// Suspect names what the body looks like that is not certain enough to refuse it (a weak
	// credential shape): the message is sent anyway, so the sender is also told how to say so.
	Suspect []string
}

// Prepare checks r and returns the plan to send. warn receives non-fatal remarks
// (a weak credential shape, an older node). It sends nothing.
func Prepare(c *client.Client, r Request, warn io.Writer) (Plan, error) {
	var p Plan
	me := identity.Me()
	if r.From != "" {
		if !msg.ValidIdentity(r.From) || msg.HostOf(r.From) != msg.HostOf(me) {
			return p, Usagef("a message can be sent as another identity of this machine only, not %s: leave --as out, or name one of this machine (`aivoicebus whoami`)", view.Name(r.From, msg.MaxIdentity))
		}
		me = r.From
	}
	body := strings.TrimSpace(r.Body)
	to := strings.ToLower(strings.TrimSpace(r.To))
	named := to != "" // the sender chose the target; a reply's target is inferred from the message it answers
	urgent := strings.EqualFold(r.Prio, msg.PrioUrgent)
	if r.Re != "" && hexPrefix(strings.ToLower(r.Re)) { // --re 3fa9 names the message by its first characters
		if len(r.Re) < 10 && (urgent || r.File || r.Attach) { // a peer can plant a message whose id starts with the same characters
			return p, Usagef("--re %s is an abbreviation: an urgent message, a file or a log link goes only to a message named by its full id (a peer can plant an id with the same first characters): run it again with the full id (`aivoicebus inbox --all` lists ids)", view.Name(r.Re, 12))
		}
		var err error
		ids := NewIDResolver(c, me)
		ids.Say = warn
		if r.Re, err = ids.Resolve(r.Re); err != nil {
			return p, fmt.Errorf("--re: %w", err)
		}
	}
	var approval msg.Message // the approval request this is a reply in, when it is one
	inApproval := false
	test := r.Test || msg.IsTestID(r.Re) // a reply to a test message stays in the lane
	if r.Re != "" {
		var orig msg.Message
		var found bool
		var err error
		if msg.ValidID(r.Re) {
			orig, found, err = lookup(c, r.Re)
		}
		if found && msg.DecisionShaped(body) {
			approval, inApproval = approvalRequestOf(c, orig)
		}
		switch {
		case to == "" && !found: // a reply goes back to whoever wrote the message
			if err != nil {
				return p, err
			}
			return p, NotFoundf("message %s was not found on the nodes: run it again with --to WHO, or check the id with `aivoicebus inbox --all`", r.Re)
		case to == "":
			to = replyTarget(orig, me)
			if !r.TargetSaid { // nothing is sent to a recipient the sender was not shown
				fmt.Fprintf(warn, "no --to: answering %s, the other side of message %s\n", view.Name(to, msg.MaxIdentity), r.Re)
			}
		}
		if found && orig.Retracted != nil {
			fmt.Fprintf(warn, "warning: message %s was withdrawn by %s: the sender took it back, so this replies to something that is no longer shown\n", r.Re, msg.TombstoneWho(orig.Retracted.By))
		}
	}
	if to == "" || body == "" {
		return p, Usagef("need --to and a body (args or stdin), like `aivoicebus send --to laptop-b \"text\"`")
	}
	// A name that only looks like another (a Unicode hyphen, a Cyrillic letter, an invisible
	// character) would route the message to, or pass as, someone else: the nodes refuse it too.
	if why := msg.SenderProblem(me); why != "" {
		return p, fmt.Errorf("refused: this session's identity %s: its machine name %q looks like something else (pin a plain ASCII one with AIVOICEBUS_HOST), or AIVOICEBUS_ME / AIVOICEBUS_AGENT does: `aivoicebus whoami` shows it", why, view.OneLine(msg.HostOf(me), 60))
	}
	if why := msg.TargetProblem(to); why != "" {
		return p, fmt.Errorf("refused: the target %s: name it with plain letters (`aivoicebus who` lists the sessions)", why)
	}
	if inApproval { // the word alone never decides: only the person's signed decision is stored (msg.DecisionProblem)
		if why := msg.DecisionProblem(approval, body); why != "" {
			return p, errors.New(msg.ApprovalRefusal(approval.ID, why)) // nextstep: msg.ApprovalRefusal says it
		}
	}
	if f, found := credentialIn(body); found {
		return p, errors.New("refused: " + secrets.Refusal(f) + " (nothing was sent)") // nextstep: secrets.Refusal says it
	}
	if f, found := secrets.Authority(body, inApproval); found { // an approver signature, a pipe link's key: not in a message's text
		return p, errors.New("refused: " + secrets.Refusal(f) + " (nothing was sent)") // nextstep: secrets.Refusal says it
	}
	for _, f := range secrets.Find(body) {
		fmt.Fprintf(warn, "warning: this message contains something that looks like a %s; if it is one, resend without it\n", f.Kind)
		p.Suspect = append(p.Suspect, f.Kind)
	}

	var err error
	if to, err = ExpandTarget(to); err != nil {
		return p, err
	}
	if subject, ok := msg.OwnerSubjectOf(to); ok {
		// owner:SUBJECT is turned into a machine, a session or a person before a message is made (tasks.RouteOwner,
		// called by send, task, ask and the MCP tools); a message built on it would go to a machine that does not exist.
		return p, Usagef("owner:%s is resolved before a message is made, and this way of sending did not: send, task and ask do it (`aivoicebus owner get %s` says who it reaches), or send to the machine it names", view.Name(subject, 80), view.Name(subject, 80))
	}
	if role, ok := msg.IsRoleAlias(to); ok { // @build: the one live session holding the role, shown
		res, err := ResolveRole(c, role, me, msg.RoleOpts{
			Trust:  r.TrustRole || os.Getenv("AIVOICEBUS_TRUST_ROLES") == "1",
			Urgent: strings.EqualFold(r.Prio, msg.PrioUrgent) || r.File, // a file is shown at the next prompt, urgently
			Attach: r.File || r.Attach,
		})
		if err != nil {
			return p, err
		}
		p.Alias, p.Role = res.Describe(), &res
		p.Notes = append(p.Notes, p.Alias)
		to = res.Target
	}
	var sel msg.Selector
	if msg.IsSelector(to) {
		if sel, err = msg.ParseSelector(to); err != nil {
			return p, Usagef("bad target %q: %v", r.To, err)
		}
	}

	p.ByCaps = usesCaps(sel)
	if p.ByCaps && (urgent || r.File || r.Attach) {
		// capabilities are claimed by the sessions themselves, so whoever claims one gets the message:
		// nothing urgent, no file and no log link goes to them (a pinned role or an explicit --to does)
		return p, Usagef("refused: %s routes by capability, which any session can claim; an urgent message, a file or a log link never goes to such a route (name the session with --to, or use a pinned role; `aivoicebus who` lists them)", view.Name(to, 80))
	}
	m := msg.Message{ID: msg.NewID(), From: me, To: to, Re: r.Re, Thread: r.Thread, Body: body,
		Prio: strings.ToLower(r.Prio), Kind: strings.ToLower(r.Kind), MaxTurns: r.MaxTurns, Deadline: int64(r.Deadline / time.Second), BudgetChars: r.BudgetChars}
	if test {
		switch why := msg.TestRefusal(m); {
		case why == "":
			m.ID = msg.NewTestID()
		case r.Test:
			return p, fmt.Errorf("refused: %s: try again without --test", why)
		default: // only a reply to a test message: one that cannot be a test (an approval request) goes out as it is
			test = false
		}
	}
	if d, err := strconv.Atoi(os.Getenv("AIVOICEBUS_TASK_DEPTH")); err == nil && d > 0 {
		// Sent from a run a worker started for a task: what this sends is one task deeper.
		m.Depth = min(d, msg.MaxDepthField)
		if m.Kind == msg.KindTask && d > msg.MaxTaskDepth {
			return p, fmt.Errorf("refused: this would be a task %d tasks deep (a task sent from the run of a task sent from ...); more than %d is a chain reaction, not work: ask the user instead", d, msg.MaxTaskDepth)
		}
	}
	if (r.MaxTurns != 0 || r.Deadline != 0 || r.BudgetChars != 0) && (r.Re != "" || r.Thread != "") {
		return p, Usagef("--max-turns, --deadline and --budget-chars go on the message that starts a conversation, not on a reply: run it again without them")
	}
	if r.MaxTurns != 0 || r.Deadline != 0 {
		if old := nodeTooOldFor(c, msg.TurnsSince); old != "" {
			return p, fmt.Errorf("refused: %s runs a release older than %s, which does not enforce --max-turns or --deadline: ask the user to update the nodes first", old, msg.TurnsSince)
		}
	}
	if r.BudgetChars != 0 {
		if r.BudgetChars < 0 || r.BudgetChars > msg.MaxBudgetChars {
			return p, Usagef("--budget-chars is 1 to %d characters: run it again with a number in that range", msg.MaxBudgetChars)
		}
		if n := utf8.RuneCountInString(body); n > r.BudgetChars {
			return p, Usagef("this message is %d characters, more than its own --budget-chars %d: run it again with a larger budget or a shorter message", n, r.BudgetChars)
		}
		if old := nodeTooOldFor(c, msg.BudgetSince); old != "" {
			return p, fmt.Errorf("refused: %s runs a release older than %s, which does not enforce --budget-chars: ask the user to update the nodes first, or send it without --budget-chars", old, msg.BudgetSince)
		}
	}
	if r.File {
		// The file lives five minutes, and an "ask" message is only shown at session start, so a
		// file sent that way would be gone before anyone saw it. Show it on the next prompt and
		// stop delivering it once the file is gone.
		if m.Prio == "" {
			m.Prio = msg.PrioUrgent
		}
		if r.TTL == "" {
			r.TTL = "5m"
		}
	}
	if m.Kind == msg.KindTask && m.Prio == "" {
		// A task is shown on the next prompt of each session it reaches, and only until one of
		// them takes it: an "ask" would wait for a session to start, which may be hours.
		m.Prio = msg.PrioUrgent
	}
	if r.TTL != "" {
		d, err := msg.ParseTTL(r.TTL)
		if err != nil {
			return p, Usagef("--ttl: %v", err)
		}
		m.TTL = int64(d / time.Second)
	}
	if test {
		if m.TTL > int64(msg.TestLife/time.Second) {
			return p, Usagef("--ttl: a test message lives at most %s (it is purged after that): run it again with a shorter --ttl, or without --test", SpanShort(msg.TestLife))
		}
		if m.TTL == 0 {
			m.TTL = int64(msg.TestLife / time.Second)
		}
		if !m.IsTest() { // cannot happen once the checks above passed; a message that is not a test must never go out as one
			return p, errors.New("refused: this message could not be made a test message: try again without --test")
		}
		old, err := c.TestLane()
		switch {
		case err != nil:
			return p, fmt.Errorf("refused: could not ask the nodes whether they have the test lane: %v", err)
		case old != "":
			return p, fmt.Errorf("refused: %s predates the test lane, so it would show a test message to everyone it is addressed to and never purge it: ask the user to update the nodes first (`aivoicebus nodes` lists them)", old)
		}
	}
	if err := m.ValidateOptions(); err != nil {
		switch {
		case strings.HasPrefix(err.Error(), "priority"):
			return p, Usagef("--prio: %v", err)
		case strings.HasPrefix(err.Error(), "kind"), strings.HasPrefix(err.Error(), "an answer"), strings.HasPrefix(err.Error(), "a claim"), strings.HasPrefix(err.Error(), "a lease"):
			return p, Usagef("--kind: %v", err)
		}
		return p, err
	}
	if r.Authored && !r.File && !r.Attach && !test {
		l := LintPriority(body, r.Prio, m.Kind)
		if l.Lower {
			m.Prio, p.Lowered = msg.PrioFYI, true
		}
		if l.Warning != "" {
			fmt.Fprintln(warn, l.Warning)
		}
	}
	if (r.Authored || r.Amending) && !r.File && !r.Attach {
		if w := LintRelativeTimes(body, time.Now()); w != "" {
			fmt.Fprintln(warn, w)
		}
	}

	if m.Kind == msg.KindTask || m.Kind == msg.KindClaim || m.Kind == msg.KindEnd {
		// A node older than this quarantines the record instead of relaying it.
		if old := nodeTooOldFor(c, msg.TaskSince); old != "" {
			return p, fmt.Errorf("refused: %s runs a release older than %s, which cannot hold a %s: ask the user to update the nodes first", old, msg.TaskSince, m.Kind)
		}
	}
	// Sign only when every node keeps the signature (see msg.SignSince); the client signs as it sends.
	if !c.NoSign && nodeTooOldFor(c, msg.SignSince) != "" {
		c.NoSign = true
	}
	if m.Kind == msg.KindLease {
		if old := nodeTooOldFor(c, msg.LeaseSince); old != "" {
			return p, fmt.Errorf("refused: %s runs a release older than %s, which cannot hold a lease: ask the user to update the nodes first", old, msg.LeaseSince)
		}
	}
	if m.Kind == msg.KindProgress {
		if m.Re == "" {
			return p, Usagef("a progress message says which task it is about: run it again with --re TASK_ID (`aivoicebus queue` lists tasks)")
		}
		if old := nodeTooOldFor(c, msg.ProgressSince); old != "" {
			return p, fmt.Errorf("refused: %s runs a release older than %s, which cannot hold a progress message: ask the user to update the nodes first", old, msg.ProgressSince)
		}
	}

	switch {
	case sel.Advanced():
		if old := nodeTooOldFor(c, msg.SelectorSince); old != "" {
			fmt.Fprintf(warn, "warning: %s runs a release that cannot read alternatives (a|b) or exclusions (!=): sessions reading from it will not get this message until it is updated\n", old)
		}
	case m.Kind != "":
		if old := nodeTooOldFor(c, msg.SelectorSince); old != "" {
			fmt.Fprintf(warn, "warning: %s runs a release that drops the message kind: where it relays this message it is an ordinary one, and an answer will not close a question there until it is updated\n", old)
		}
	}

	roster, rosterErr := c.RosterAll()                              // a decision, not a display: probes are counted
	if now := time.Now(); r.Authored && !test && rosterErr == nil { // a test message has its own, stricter check below
		if st, stale := StaleTarget(roster, to, me, now, LiveWithin); stale {
			switch {
			case !named: // the answer goes where its asker is, whatever happened since: say so and send it
				fmt.Fprintln(warn, st.Warning(now, LiveWithin))
			case !r.Queue && !r.Force:
				return p, errors.New(st.Refusal(now, LiveWithin))
			}
		}
	}
	if test { // only a session that declared the opt-in, and is around for the ten minutes the message lives, can be reached
		now := time.Now()
		roster = slices.DeleteFunc(slices.Clone(roster), func(e msg.RosterEntry) bool {
			return !msg.OptedIn(e.Labels) || (!e.Listening && now.Sub(e.Seen) > LiveWithin)
		})
	}
	names, known := reachAmong(roster, rosterErr, to, me)
	n := len(names)
	// A reply goes to whoever wrote the test message, opted in or not: it is in the lane because of what it answers,
	// and its reader reads the thread through the lane. Only a message the sender chose to send as a test needs a
	// recipient that opted in, since nobody else would ever see it.
	if test && r.Test && known && n == 0 {
		return p, fmt.Errorf("refused: no session addressed by %s has opted in to the test lane and been seen in the last %s, and a test message lives only %s: a session opts in with AIVOICEBUS_TEST=1 (a worker with `worker --test-lane`); `aivoicebus who %s` shows who is there", to, SpanShort(LiveWithin), SpanShort(msg.TestLife), to)
	}
	if w := ListenerWarning(roster, to, me, time.Now()); w != "" {
		fmt.Fprintln(warn, w)
	}
	p.Reach, p.Known = n, known
	if known && usesCaps(sel) {
		p.Recipients = names
		if n >= 1 && n <= 2 {
			p.CapNote = fmt.Sprintf("only %d session(s) advertise this capability, and anyone can: confirm they are who you mean before sending a secret or a command", n)
		}
	}
	if known && n > 0 {
		m.Reach = &n
	}
	if msg.IsSelector(to) && known && n == 0 && !r.Queue {
		return p, fmt.Errorf("no session seen in the last %s matches %s (check the spelling with `aivoicebus who %s`); add --queue to leave it for one that starts later", ReachWindowText, to, to)
	}

	if r.File {
		switch {
		case known && n == 0:
			return p, fmt.Errorf("no session seen in the last %s matches %s, and a file can be read for only five minutes, so it cannot wait for one to start: check the target with `aivoicebus who %s`", ReachWindowText, to, to)
		case known && n > 1 && !r.Keep && !strings.EqualFold(r.Kind, msg.KindTask): // a task has one reader: whoever takes it
			return p, fmt.Errorf("%s reaches %d sessions but a file is burned by the first one to read it; run it again to one session (`aivoicebus who %s` lists them), or with --keep to let each of them read it for five minutes", to, n, to)
		}
	}

	p.Readers = 1
	if msg.IsBulletin(to) && known {
		p.Readers = n
	}
	p.Context = contextChars(m)
	if test {
		p.Notes = append(p.Notes, "test message: reaches opted-in sessions only, is never put into a hook's context, and is purged about "+SpanShort(msg.TestLife)+" after it was sent")
	}
	if !r.Force {
		// A task interrupts the sessions it reaches only until one of them takes it, so a task to a
		// machine with many sessions is not refused; a task to everyone still is.
		if m.Priority() == msg.PrioUrgent && known && n >= UrgentFanout && !r.Amending && (m.Kind != msg.KindTask || to == "all") {
			return p, fmt.Errorf("an urgent message to %s would interrupt %d sessions; run it again with --prio ask, or a narrower target (`aivoicebus who` lists them), or --force if that is what you want", to, n)
		}
		if known && n >= BulkReaders && utf8.RuneCountInString(body) > BulkChars {
			return p, fmt.Errorf("a %d-character message to %d sessions puts about %d characters into their contexts; send it again shorter, or as a pointer, or with --force", utf8.RuneCountInString(body), n, p.Context*n)
		}
	}

	window := ReachWindowText
	if test {
		window = SpanShort(LiveWithin) // the sessions counted above are the opted-in ones seen this recently
	}
	switch {
	case known && n > 0:
		p.Notes = append(p.Notes, fmt.Sprintf("reaches %d session(s) seen in the last %s", n, window))
	case known:
		p.Notes = append(p.Notes, "no matching session seen in the last "+window+"; it waits for one to start")
	}
	if m.TTL == 0 && msg.IsBulletin(to) {
		p.Expiry = msg.DefaultLife(m.Priority())
		p.Notes = append(p.Notes, "expires in "+SpanText(p.Expiry)+" unless --ttl says otherwise")
	}
	if p.Lowered {
		p.Notes = append(p.Notes, LoweredNote)
	}
	p.Msg = m
	return p, nil
}

// credentialIn finds a credential that must never be sent: a secret this machine holds (its
// token, the files in ~/.config/aivoicebus), matched exactly, else anything shaped like one.
// The files are read at every send, so a token rotated a minute ago is covered too.
// CheckContent refuses data that contains a credential, so a file cannot carry what a message
// may not: this machine's own secrets matched exactly, and anything shaped like a credential.
func CheckContent(data []byte) error {
	if f, found := credentialIn(string(data)); found {
		return errors.New("refused: the file contains what looks like a " + f.Kind + "; say where the secret lives instead, and send it again")
	}
	return nil
}

// LineFilter returns a function that replaces a line holding a credential with a note, for
// text that is streamed rather than sent as one message. The machine's own secrets are read
// once, when the filter is made.
func LineFilter() func(line string) (string, bool) {
	local := secrets.NewCanaries(secrets.KindLocalSecret)
	for _, v := range config.SecretValues() {
		local.Add(v)
	}
	return func(line string) (string, bool) {
		f, found := local.Find(line)
		if !found {
			f, found = secrets.Strong(line)
		}
		if found {
			return "[line withheld: it looks like a " + f.Kind + "]", true
		}
		return line, false
	}
}

// Credential reports the kind of credential body holds, exactly as a send would refuse it: this
// machine's own secrets matched exactly, else anything shaped like a credential, an approver
// signature or a pipe link with its key.
func Credential(body string) (kind string, found bool) {
	f, found := credentialIn(body)
	if !found {
		f, found = secrets.Authority(body, false)
	}
	return f.Kind, found
}

func credentialIn(body string) (secrets.Finding, bool) {
	if f, found := localSecrets().Find(body); found {
		return f, true
	}
	return secrets.Strong(body)
}

// localSecrets is the set of secrets this machine holds, read afresh.
func localSecrets() *secrets.Canaries {
	local := secrets.NewCanaries(secrets.KindLocalSecret)
	for _, v := range config.SecretValues() {
		local.Add(v)
	}
	return local
}

// ExpandTarget resolves the sender-side shorthands of a target: a saved group (@+name),
// @here and a capability term (can=run). It is what `send` does, and what `who` must do too,
// so a preview of a target shows the sessions the send would reach.
func ExpandTarget(to string) (string, error) {
	to = strings.ToLower(strings.TrimSpace(to))
	to, err := msg.ExpandGroup(to, msg.ParseGroups(config.GroupsText()))
	if err != nil {
		return "", err
	}
	if to, err = msg.ExpandHere(to, identity.Labels("")); err != nil {
		return "", err
	}
	if to, err = msg.ExpandCaps(to); err != nil {
		return "", err
	}
	return msg.ExpandRooms(to)
}

// Send writes the planned message to the mesh and remembers it as the last one sent from this
// machine, so `retract` can take it back without being given the id.
func (p Plan) Send(c *client.Client) (msg.Message, error) {
	out, err := c.Send(p.Msg)
	if err != nil {
		var se *client.StatusError
		if errors.As(err, &se) && se.Code == http.StatusUnprocessableEntity && strings.Contains(se.Text, "looks like it contains") {
			err = fmt.Errorf("%w (nothing was sent)", err) // the node's own credential backstop
		}
		return out, err
	}
	rememberSent(out)
	return out, nil
}

// Describe is the dry-run report: what would be sent and what it would cost.
func (p Plan) Describe(w io.Writer) {
	m := p.Msg
	fmt.Fprintln(w, "dry run: nothing was sent")
	fmt.Fprintf(w, "  from      %s\n", view.Name(m.From, msg.MaxIdentity))
	fmt.Fprintf(w, "  to        %s\n", view.Name(m.To, msg.MaxIdentity)) // a reply's target came from a peer
	if m.Re != "" {
		fmt.Fprintf(w, "  re        %s\n", view.OneLine(m.Re, msg.MaxRef))
	}
	if p.Alias != "" {
		fmt.Fprintf(w, "  resolved  %s\n", p.Alias)
	}
	prio := m.Priority()
	if m.Kind != "" {
		prio += ", " + m.Kind
	}
	fmt.Fprintf(w, "  priority  %s\n", prio)
	if p.Lowered {
		fmt.Fprintln(w, "            lowered to fyi because the words say nothing is needed; --prio ask to interrupt")
	}
	switch {
	case m.TTL > 0:
		fmt.Fprintf(w, "  expires   in %s\n", SpanText(time.Duration(m.TTL)*time.Second))
	case p.Expiry > 0:
		fmt.Fprintf(w, "  expires   in %s (the default for a message that can reach several sessions)\n", SpanText(p.Expiry))
	default:
		fmt.Fprintln(w, "  expires   never")
	}
	switch {
	case p.Known && p.Reach > 0:
		fmt.Fprintf(w, "  reaches   %d session(s) seen in the last %s\n", p.Reach, ReachWindowText)
	case p.Known:
		fmt.Fprintf(w, "  reaches   nobody seen in the last %s; it would wait for one to start\n", ReachWindowText)
	default:
		fmt.Fprintln(w, "  reaches   unknown (the nodes could not say who is around)")
	}
	if p.Context == 0 {
		fmt.Fprintln(w, "  context   nothing: fyi messages are only counted at session start")
	} else {
		fmt.Fprintf(w, "  context   about %d characters in each reader's session start (%d in all)\n", p.Context, p.Context*max(p.Readers, 1))
	}
	for _, l := range p.CapLines() {
		fmt.Fprintln(w, "  "+l)
	}
	fmt.Fprintf(w, "  body      %d characters\n", utf8.RuneCountInString(m.Body))
}

// contextChars is how many characters showing m puts into a reader's context at
// session start: its header line and its body, cut as the hooks cut it (a long broadcast as its
// headline, view.Headlines; it is new, so the age and thread rules do not apply yet).
func contextChars(m msg.Message) int {
	if m.Priority() == msg.PrioFYI {
		return 0 // only counted, never shown
	}
	var b strings.Builder
	m.At = time.Now()
	view.BriefWith(&b, []msg.Message{m}, view.SessionStartBody, view.Headlines(nil, m.At))
	return utf8.RuneCountInString(b.String())
}

// Reach counts the sessions seen within ReachWindow, other than me, that a
// message to target would reach. known is false when the nodes cannot say (none
// has a roster, or none reachable), and then nothing should be claimed.
func Reach(c *client.Client, to, me string) (n int, known bool) {
	names, known := reachList(c, to, me)
	return len(names), known
}

// reachList is Reach that says which sessions.
func reachList(c *client.Client, to, me string) (names []string, known bool) {
	roster, err := c.RosterAll() // a decision, not a display: probes are counted
	return reachAmong(roster, err, to, me)
}

func reachAmong(roster []msg.RosterEntry, err error, to, me string) (names []string, known bool) {
	if err != nil {
		return nil, false
	}
	cutoff := time.Now().Add(-ReachWindow)
	for _, e := range roster {
		if e.Me != me && !e.Seen.Before(cutoff) && msg.MatchesLabeled(to, e.Me, e.Labels) {
			names = append(names, e.Me)
		}
	}
	return names, true // c.Roster is already sorted by identity
}

// usesCaps reports whether a selector routes by the capabilities label.
func usesCaps(sel msg.Selector) bool {
	for _, t := range sel {
		if t.Key == msg.CapsLabel {
			return true
		}
	}
	return false
}

// maxRecipientNames is how many recipients a report names before it counts the rest.
const maxRecipientNames = 8

// CapLines is what a send routed by capability tells its sender: who it resolved to (the names,
// not a count) and the warning when that is one or two sessions. Nil for any other send.
func (p Plan) CapLines() []string {
	if len(p.Recipients) == 0 {
		return nil
	}
	shown := slices.Clone(p.Recipients[:min(len(p.Recipients), maxRecipientNames)])
	for i := range shown {
		shown[i] = view.OneLine(shown[i], 48)
	}
	names := strings.Join(shown, ", ")
	if more := len(p.Recipients) - len(shown); more > 0 {
		names += fmt.Sprintf(", +%d more", more)
	}
	lines := []string{"routed by capability (self-declared, not authenticated) to: " + names}
	if p.CapNote != "" {
		lines = append(lines, p.CapNote)
	}
	return lines
}

// lookup finds message id among the messages of its thread on the nodes. The ordinary read asks
// the nodes in order and takes the first answer, and a node that has not yet received a message
// answers that there is nothing, so when it is not there every node is asked by itself, all at
// once: one that holds it is enough. A node that cannot be reached is skipped, and the error of
// the ordinary read is what is returned when none could answer.
func lookup(c *client.Client, id string) (msg.Message, bool, error) {
	if msg.IsTestID(id) { // a test message is in a thread read only when the read asks for the lane
		c = c.WithLane()
	}
	find := func(ms []msg.Message) (msg.Message, bool) {
		for _, m := range ms {
			if m.ID == id {
				return m, true
			}
		}
		return msg.Message{}, false
	}
	ms, err := c.Thread(id)
	if m, ok := find(ms); ok {
		return m, true, nil
	}
	if len(c.URLs) < 2 {
		return msg.Message{}, false, err
	}
	type answer struct {
		m             msg.Message
		held, replied bool
	}
	answers := make([]answer, len(c.URLs))
	var wg sync.WaitGroup
	for i, u := range c.URLs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			one := &client.Client{URLs: []string{u}, Token: c.Token, Timeout: lookupTimeout}
			if ms, err := one.Thread(id); err == nil {
				m, held := find(ms)
				answers[i] = answer{m, held, true}
			}
		}()
	}
	wg.Wait()
	for _, a := range answers {
		if a.held {
			return a.m, true, nil
		}
		if a.replied {
			err = nil // some node answered: the message is simply not there
		}
	}
	return msg.Message{}, false, err
}

// lookupTimeout bounds each node asked by itself, so a hung one does not hold up the answer.
const lookupTimeout = 3 * time.Second

// replyTarget is who a reply to m should go to: its sender, or if that is the replier, the
// recipient of the message.
func replyTarget(m msg.Message, me string) string {
	if m.From == me {
		return m.To
	}
	return m.From
}

// nodeRelease is a node and the release it runs, as the node that answers reports it.
type nodeRelease struct{ label, version string }

// nodeReleases lists the answering node and each peer it reaches whose release is known: a node that
// does not say, or cannot be reached, is not listed, since nothing can be said of it.
func nodeReleases(c *client.Client) []nodeRelease {
	h, err := c.Health()
	if err != nil {
		return nil
	}
	var out []nodeRelease
	if release.ValidVersion(h.Version) {
		out = append(out, nodeRelease{"node " + oneLine(h.Node, 40), h.Version})
	}
	for _, peer := range h.Peers {
		if peer.OK && release.ValidVersion(peer.Version) {
			out = append(out, nodeRelease{"the node at " + oneLine(peer.URL, 80), peer.Version})
		}
	}
	return out
}

// nodesOlderThan names the nodes of the mesh that run a release before since.
func nodesOlderThan(c *client.Client, since string) []string {
	var out []string
	for _, r := range nodeReleases(c) {
		if release.Newer(since, r.version) {
			out = append(out, r.label+" ("+r.version+")")
		}
	}
	return out
}

// nodeTooOldFor names one node of the mesh that predates release since, or "".
func nodeTooOldFor(c *client.Client, since string) string {
	if old := nodesOlderThan(c, since); len(old) > 0 {
		return old[0]
	}
	return ""
}

// SpanShort is SpanText without the zero parts of a duration: 30m, 10m, 1h.
func SpanShort(d time.Duration) string {
	s := SpanText(d)
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s") // 10m0s is 10m, 1h0m0s is 1h0m ...
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m") // ... is 1h
	}
	return s
}

// SpanText writes a duration the way --ttl reads it: whole days as 3d.
func SpanText(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}

// ListenerWarning is a warning when to names one session that is not listening while another
// session of the same machine is (standing by in a wait): the sender may have picked the wrong one
// of two look-alike ids, as a hand-off once did. "" when there is nothing to say. Both sessions are
// named with their titles, so the sender can tell them apart.
func ListenerWarning(roster []msg.RosterEntry, to, me string, now time.Time) string {
	if msg.IsBulletin(to) || msg.IsSelector(to) {
		return "" // a machine, a tool or a selector: the nodes deliver it to whoever reads first
	}
	to = strings.ToLower(to)
	host := msg.HostOf(to)
	var target *msg.RosterEntry
	var listening []msg.RosterEntry
	for i, e := range roster {
		id := strings.ToLower(e.Me)
		switch {
		case id == to:
			target = &roster[i]
		case id == strings.ToLower(me), msg.HostOf(id) != host, msg.IsBulletin(id), !e.Listening:
		case now.Sub(e.Seen) > ReachWindow:
		default:
			listening = append(listening, e) // another exact session there, in a wait right now
		}
	}
	if len(listening) == 0 || (target != nil && target.Listening) {
		return ""
	}
	var others []string
	for _, e := range listening {
		others = append(others, view.Session(e, now))
	}
	named := view.Name(to, msg.MaxIdentity) + " has not been seen by the nodes"
	if target != nil {
		named = view.Session(*target, now) + " is not listening"
	}
	return fmt.Sprintf("warning: %s, but %s on %s %s listening: %s. If you meant the one standing by, send to it instead.",
		named, plural(len(others), "another session", "other sessions"), view.Name(host, msg.MaxIdentity),
		plural(len(others), "is", "are"), strings.Join(others, ", "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
