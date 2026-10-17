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
	TargetSaid                            bool // the caller already told the sender whom a reply with no --to goes to
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
	body := strings.TrimSpace(r.Body)
	to := strings.ToLower(strings.TrimSpace(r.To))
	urgent := strings.EqualFold(r.Prio, msg.PrioUrgent)
	if r.Re != "" && hexPrefix(strings.ToLower(r.Re)) { // --re 3fa9 names the message by its first characters
		if len(r.Re) < 10 && (urgent || r.File || r.Attach) { // a peer can plant a message whose id starts with the same characters
			return p, Usagef("--re %s is an abbreviation: an urgent message, a file or a log link goes only to a message named by its full id (a peer can plant an id with the same first characters)", view.Name(r.Re, 12))
		}
		var err error
		ids := NewIDResolver(c, me)
		ids.Say = warn
		if r.Re, err = ids.Resolve(r.Re); err != nil {
			return p, fmt.Errorf("--re: %w", err)
		}
	}
	if r.Re != "" {
		var orig msg.Message
		var found bool
		var err error
		if msg.ValidID(r.Re) {
			orig, found, err = lookup(c, r.Re)
		}
		switch {
		case to == "" && !found: // a reply goes back to whoever wrote the message
			if err != nil {
				return p, err
			}
			return p, NotFoundf("message %s was not found on the nodes; say who to send to with --to", r.Re)
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
		return p, Usagef("need --to and a body (args or stdin)")
	}
	// A name that only looks like another (a Unicode hyphen, a Cyrillic letter, an invisible
	// character) would route the message to, or pass as, someone else: the nodes refuse it too.
	if why := msg.SenderProblem(me); why != "" {
		return p, fmt.Errorf("refused: this session's identity %s: its machine name %q looks like something else (pin a plain ASCII one with AIVOICEBUS_HOST), or AIVOICEBUS_ME / AIVOICEBUS_AGENT does", why, view.OneLine(msg.HostOf(me), 60))
	}
	if why := msg.TargetProblem(to); why != "" {
		return p, fmt.Errorf("refused: the target %s", why)
	}
	if f, found := credentialIn(body); found {
		return p, errors.New("refused: " + secrets.Refusal(f) + " (nothing was sent)")
	}
	for _, f := range secrets.Find(body) {
		fmt.Fprintf(warn, "warning: this message contains something that looks like a %s; if it is one, resend without it\n", f.Kind)
		p.Suspect = append(p.Suspect, f.Kind)
	}

	var err error
	if to, err = ExpandTarget(to); err != nil {
		return p, err
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
		return p, Usagef("refused: %s routes by capability, which any session can claim; an urgent message, a file or a log link never goes to such a route (name the session with --to, or use a pinned role)", view.Name(to, 80))
	}
	m := msg.Message{ID: msg.NewID(), From: me, To: to, Re: r.Re, Thread: r.Thread, Body: body,
		Prio: strings.ToLower(r.Prio), Kind: strings.ToLower(r.Kind)}
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
	if err := m.ValidateOptions(); err != nil {
		switch {
		case strings.HasPrefix(err.Error(), "priority"):
			return p, Usagef("--prio: %v", err)
		case strings.HasPrefix(err.Error(), "kind"), strings.HasPrefix(err.Error(), "an answer"), strings.HasPrefix(err.Error(), "a claim"), strings.HasPrefix(err.Error(), "a lease"):
			return p, Usagef("--kind: %v", err)
		}
		return p, err
	}

	if m.Kind == msg.KindTask || m.Kind == msg.KindClaim || m.Kind == msg.KindEnd {
		// A node older than this quarantines the record instead of relaying it.
		if old := nodeTooOldFor(c, msg.TaskSince); old != "" {
			return p, fmt.Errorf("refused: %s runs a release older than %s, which cannot hold a %s; update the nodes first", old, msg.TaskSince, m.Kind)
		}
	}
	if m.Kind == msg.KindLease {
		if old := nodeTooOldFor(c, msg.LeaseSince); old != "" {
			return p, fmt.Errorf("refused: %s runs a release older than %s, which cannot hold a lease; update the nodes first", old, msg.LeaseSince)
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

	names, known := reachList(c, to, me)
	n := len(names)
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
			return p, fmt.Errorf("no session seen in the last %s matches %s, and a file can be read for only five minutes, so it cannot wait for one to start", ReachWindowText, to)
		case known && n > 1 && !r.Keep && !strings.EqualFold(r.Kind, msg.KindTask): // a task has one reader: whoever takes it
			return p, fmt.Errorf("%s reaches %d sessions but a file is burned by the first one to read it; send to one session, or add --keep to let each of them read it for five minutes", to, n)
		}
	}

	p.Readers = 1
	if msg.IsBulletin(to) && known {
		p.Readers = n
	}
	p.Context = contextChars(m)
	if !r.Force {
		// A task interrupts the sessions it reaches only until one of them takes it, so a task to a
		// machine with many sessions is not refused; a task to everyone still is.
		if m.Priority() == msg.PrioUrgent && known && n >= UrgentFanout && (m.Kind != msg.KindTask || to == "all") {
			return p, fmt.Errorf("an urgent message to %s would interrupt %d sessions; use --prio ask, narrow the target, or add --force if that is what you want", to, n)
		}
		if known && n >= BulkReaders && utf8.RuneCountInString(body) > BulkChars {
			return p, fmt.Errorf("a %d-character message to %d sessions puts about %d characters into their contexts; shorten it, send a pointer instead, or add --force", utf8.RuneCountInString(body), n, p.Context*n)
		}
	}

	switch {
	case known && n > 0:
		p.Notes = append(p.Notes, fmt.Sprintf("reaches %d session(s) seen in the last %s", n, ReachWindowText))
	case known:
		p.Notes = append(p.Notes, "no matching session seen in the last "+ReachWindowText+"; it waits for one to start")
	}
	if m.TTL == 0 && msg.IsBulletin(to) {
		p.Expiry = msg.DefaultLife(m.Priority())
		p.Notes = append(p.Notes, "expires in "+SpanText(p.Expiry)+" unless --ttl says otherwise")
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
		return errors.New("refused: the file contains what looks like a " + f.Kind + "; say where the secret lives instead of sending it")
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
// machine's own secrets matched exactly, else anything shaped like a credential.
func Credential(body string) (kind string, found bool) {
	f, found := credentialIn(body)
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
// session start: its header line and its body, cut as the hooks cut it.
func contextChars(m msg.Message) int {
	if m.Priority() == msg.PrioFYI {
		return 0 // only counted, never shown
	}
	var b strings.Builder
	m.At = time.Now()
	view.Brief(&b, []msg.Message{m}, view.SessionStartBody)
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

// SpanText writes a duration the way --ttl reads it: whole days as 3d.
func SpanText(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}
