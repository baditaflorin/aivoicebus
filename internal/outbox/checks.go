package outbox

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Checks on what a person or an agent writes by hand (Request.Authored): a target that reaches nobody
// (one exact session nobody has seen lately, a selector none of whose sessions is live, a session named
// without its machine, one that has read nothing for hours) and a priority the words contradict. They
// look only at what the sender already has (the roster, the words), so nothing about them is on the wire.

// LiveWithin is how recently a session must have been seen to count as live: `ping --within`
// and `ask --within` default to it, and `send` to one exact session refuses past it.
const LiveWithin = 30 * time.Minute

// maxLiveShown is how many live sessions a stale-target notice names before it counts the rest.
const maxLiveShown = 5

// Stale is one exact session a message is addressed to that nobody has seen lately.
type Stale struct {
	Target string
	Known  bool            // the roster has an entry for it (false: the nodes never heard of it, or forgot it)
	Entry  msg.RosterEntry // when Known
	// Live is who is live on the target's machine (not the sender, not a probe): sessions working in
	// the target's repo first, then the ones in a wait, then the most recently seen.
	Live []msg.RosterEntry
}

// StaleTarget reports whether to names one exact session that is not live: not in a wait, and not
// seen within `within`. A session that is not in the roster at all is stale. Anything that is not one
// session (a machine, a tool, a selector, all), the sender itself and a join probe are never stale.
func StaleTarget(roster []msg.RosterEntry, to, me string, now time.Time, within time.Duration) (Stale, bool) {
	to = strings.ToLower(strings.TrimSpace(to))
	if !msg.OneSession(to) || msg.IsProbe(to) || strings.EqualFold(to, me) {
		return Stale{}, false
	}
	st := Stale{Target: to}
	for _, e := range roster {
		if strings.EqualFold(e.Me, to) {
			st.Entry, st.Known = e, true
		}
	}
	if st.Known && isLive(st.Entry, now, within) {
		return Stale{}, false
	}
	st.Live = LiveOn(roster, msg.HostOf(to), me, st.Entry.Labels["repo"], now, within, to)
	return st, true
}

// isLive is whether a session counts as live: in a wait that takes any message, or seen within `within`.
func isLive(e msg.RosterEntry, now time.Time, within time.Duration) bool {
	return e.Listening || now.Sub(e.Seen) <= within
}

// LiveOn lists the sessions live on the machine host: in a wait, or seen within `within`; never me, a
// join probe or one of skip. Sessions working in repo come first (when repo is not ""), then the ones
// in a wait, then the most recently seen.
func LiveOn(roster []msg.RosterEntry, host, me, repo string, now time.Time, within time.Duration, skip ...string) []msg.RosterEntry {
	return liveWhere(roster, func(e msg.RosterEntry) bool { return msg.HostOf(e.Me) == host }, me, repo, now, within, skip...)
}

// liveWhere is LiveOn for the sessions keep accepts, on any machine.
func liveWhere(roster []msg.RosterEntry, keep func(msg.RosterEntry) bool, me, repo string, now time.Time, within time.Duration, skip ...string) []msg.RosterEntry {
	var live []msg.RosterEntry
	for _, e := range roster {
		if !keep(e) || strings.EqualFold(e.Me, me) || msg.IsProbe(e.Me) || !isLive(e, now, within) {
			continue
		}
		if slices.ContainsFunc(skip, func(s string) bool { return strings.EqualFold(e.Me, s) }) {
			continue
		}
		live = append(live, e)
	}
	sameRepo := func(e msg.RosterEntry) bool { return repo != "" && e.Labels["repo"] == repo }
	sort.SliceStable(live, func(i, j int) bool {
		a, b := live[i], live[j]
		switch {
		case sameRepo(a) != sameRepo(b):
			return sameRepo(a)
		case a.Listening != b.Listening:
			return a.Listening
		case !a.Seen.Equal(b.Seen):
			return a.Seen.After(b.Seen)
		}
		return a.Me < b.Me
	})
	return live
}

// Seen is when the target was last seen, in words: "last seen Oct 19 21:30 EEST (9h ago)".
func (s Stale) Seen(now time.Time) string {
	if !s.Known {
		return "has not been seen by the nodes"
	}
	return fmt.Sprintf("was last seen %s (%s)", view.When(s.Entry.Seen, now), since(now.Sub(s.Entry.Seen)))
}

// liveLine names who is live on the target's machine, and in its repo, or says nobody is.
func (s Stale) liveLine(now time.Time, within time.Duration) string {
	host := view.Name(msg.HostOf(s.Target), msg.MaxIdentity)
	if len(s.Live) == 0 {
		return fmt.Sprintf("nobody else on %s was seen in the last %s", host, ago(within))
	}
	shown := s.Live[:min(len(s.Live), maxLiveShown)]
	names := make([]string, len(shown))
	for i, e := range shown {
		state := since(now.Sub(e.Seen))
		if e.Listening {
			state = "listening"
		}
		if repo := e.Labels["repo"]; repo != "" {
			state = "repo " + view.Name(repo, 40) + ", " + state
		}
		names[i] = view.Name(e.Me, msg.MaxIdentity) + view.TitleSuffix(e.Title) + " (" + state + ")"
	}
	line := "live on " + host + " now: " + strings.Join(names, ", ")
	if more := len(s.Live) - len(shown); more > 0 {
		line += fmt.Sprintf(", +%d more", more)
	}
	return line
}

// Refusal is what a send to a stale session says instead of sending: when it was last seen, who is
// live there, and the three ways on.
func (s Stale) Refusal(now time.Time, within time.Duration) string {
	host := view.Name(msg.HostOf(s.Target), msg.MaxIdentity)
	var b strings.Builder
	fmt.Fprintf(&b, "refused: %s %s, and a session counts as live when seen in the last %s, so this message may sit unread.\n",
		view.Name(s.Target, msg.MaxIdentity), s.Seen(now), ago(within))
	fmt.Fprintf(&b, "  %s\n", s.liveLine(now, within))
	if len(s.Live) > 0 {
		fmt.Fprintf(&b, "  one live session there answers:    ask %s \"...\"\n", host)
	}
	fmt.Fprintf(&b, "  any session there can take it:     task %s \"...\"\n", host)
	b.WriteString("  keep it for that session anyway:   run it again with --queue (or --force)")
	return b.String()
}

// Warning is the same facts for a reply whose target was not named (it goes to whoever wrote the
// message answered): sent anyway, since the answer is worth keeping where its asker will find it.
func (s Stale) Warning(now time.Time, within time.Duration) string {
	return fmt.Sprintf("warning: the reply goes to %s, which %s (live means seen in the last %s), so it may sit unread; %s",
		view.Name(s.Target, msg.MaxIdentity), s.Seen(now), ago(within), s.liveLine(now, within))
}

// Quiet is a selector whose sessions are none of them live: a message to it waits for one of them to
// read, and a session nobody types in never does (nine holds sent to @repo=go-fleet-graph, read by
// nobody, and a deploy restarted the service anyway).
type Quiet struct {
	Target string
	// Matched are the sessions the selector matches that were seen within ReachWindow (never the sender or
	// a join probe), the most recently seen first.
	Matched []msg.RosterEntry
	// Live is who is live on the machine of the newest of them, sessions in its repo first (LiveOn).
	Live []msg.RosterEntry
}

// QuietSelector reports whether the selector to matches sessions seen within ReachWindow and none of them
// is live (in a wait, or seen within `within`). A selector that matches nobody in ReachWindow is not quiet:
// send refuses that on its own. Anything that is not a selector is never quiet.
func QuietSelector(roster []msg.RosterEntry, to, me string, now time.Time, within time.Duration) (Quiet, bool) {
	to = strings.ToLower(strings.TrimSpace(to))
	if !msg.IsSelector(to) {
		return Quiet{}, false
	}
	q := Quiet{Target: to}
	for _, e := range roster {
		if strings.EqualFold(e.Me, me) || msg.IsProbe(e.Me) || now.Sub(e.Seen) > ReachWindow || !msg.MatchesLabeled(to, e.Me, e.Labels) {
			continue
		}
		if isLive(e, now, within) {
			return Quiet{}, false
		}
		q.Matched = append(q.Matched, e)
	}
	if len(q.Matched) == 0 {
		return Quiet{}, false
	}
	sort.SliceStable(q.Matched, func(i, j int) bool {
		if a, b := q.Matched[i], q.Matched[j]; !a.Seen.Equal(b.Seen) {
			return a.Seen.After(b.Seen)
		}
		return q.Matched[i].Me < q.Matched[j].Me
	})
	newest := q.Matched[0]
	q.Live = LiveOn(roster, msg.HostOf(newest.Me), me, newest.Labels["repo"], now, within)
	return q, true
}

// Refusal is what a send to a quiet selector says instead of sending: who it matched and when each was
// last seen, who is live on the machine of the newest one, and the ways on.
func (q Quiet) Refusal(now time.Time, within time.Duration) string {
	shown := q.Matched[:min(len(q.Matched), maxLiveShown)]
	names := make([]string, len(shown))
	for i, e := range shown {
		names[i] = view.Name(e.Me, msg.MaxIdentity) + view.TitleSuffix(e.Title) + " (seen " + since(now.Sub(e.Seen)) + ")"
	}
	list := strings.Join(names, ", ")
	if more := len(q.Matched) - len(shown); more > 0 {
		list += fmt.Sprintf(", +%d more", more)
	}
	there := Stale{Target: q.Matched[0].Me, Live: q.Live} // the newest one's machine, as a stale session's is shown
	host := view.Name(msg.HostOf(there.Target), msg.MaxIdentity)
	var b strings.Builder
	fmt.Fprintf(&b, "refused: none of the sessions %s matches is live (seen in the last %s, or standing by), so this message may sit unread; matched in the last %s: %s\n",
		view.Name(q.Target, 80), ago(within), ReachWindowText, list)
	fmt.Fprintf(&b, "  %s\n", there.liveLine(now, within))
	if len(q.Live) > 0 {
		fmt.Fprintf(&b, "  one live session there answers:    ask %s \"...\"\n", host)
	}
	b.WriteString("  keep it for the next one to match: run it again with --queue (or --force)")
	return b.String()
}

// BareSession reports whether to names one session without its machine (claude.f324, codex.build1): no
// '/' or selector syntax, and an agent tool's name in front of a dot. The nodes read such a target as a
// machine of that name, which does not exist, so the message would reach nobody.
func BareSession(to string) bool {
	to = strings.ToLower(strings.TrimSpace(to))
	if strings.ContainsAny(to, "/@*,=: ") {
		return false
	}
	tool, rest, ok := strings.Cut(to, ".")
	return ok && rest != "" && msg.KnownTool(tool)
}

// ResolveBare finds the session a bare name means (BareSession): of the sessions on the roster whose part
// after the machine is that name (on any machine; never me), the one that is live (in a
// wait, or seen within `within`), or with queue the only one there is when none is live. It returns to
// unchanged and "" when to is not a bare name, or when a machine of that name is on the roster (then the
// target is that machine). Otherwise it returns the session and how it was found, or, when none or more
// than one fits, an error that lists them and says how to name one.
func ResolveBare(roster []msg.RosterEntry, to, me string, now time.Time, within time.Duration, queue bool) (string, string, error) {
	to = strings.ToLower(strings.TrimSpace(to))
	if !BareSession(to) {
		return to, "", nil
	}
	var known, live []msg.RosterEntry
	for _, e := range roster {
		if msg.HostOf(e.Me) == to {
			return to, "", nil // a machine really has that name
		}
		if _, agent, _ := strings.Cut(strings.ToLower(e.Me), "/"); agent != to || strings.EqualFold(e.Me, me) { // a join probe's name has no dot
			continue
		}
		known = append(known, e)
		if isLive(e, now, within) {
			live = append(live, e)
		}
	}
	name := view.Name(to, msg.MaxIdentity)
	switch {
	case len(live) == 1:
		return strings.ToLower(live[0].Me), fmt.Sprintf("%s -> %s: the one live session of that name", name, view.Name(live[0].Me, msg.MaxIdentity)), nil
	case len(live) == 0 && len(known) == 1 && queue:
		return strings.ToLower(known[0].Me), fmt.Sprintf("%s -> %s: the one session of that name (not live: it waits for it)", name, view.Name(known[0].Me, msg.MaxIdentity)), nil
	}
	head := fmt.Sprintf("refused: %s names a session without its machine, so the nodes would take it for a machine of that name and it would reach nobody", name)
	if len(known) == 0 {
		return "", "", fmt.Errorf("%s; no session of that name has been seen by the nodes: name the session with its machine, as HOST/%s (`aivoicebus who` lists them)", head, name)
	}
	list, what := live, fmt.Sprintf("%d live sessions have that name", len(live))
	if len(live) == 0 {
		list, what = known, fmt.Sprintf("no session of that name is live (seen in the last %s, or standing by)", ago(within))
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		switch {
		case a.Listening != b.Listening:
			return a.Listening
		case !a.Seen.Equal(b.Seen):
			return a.Seen.After(b.Seen)
		}
		return a.Me < b.Me
	})
	next := "name one with its machine: --to " + view.Name(list[0].Me, msg.MaxIdentity)
	if len(live) == 0 {
		next = "name it with its machine (--to " + view.Name(list[0].Me, msg.MaxIdentity) + "), and add --queue to keep it for a session that is not live"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s; %s:\n", head, what)
	for i, e := range list[:min(len(list), maxLiveShown)] {
		fmt.Fprintf(&b, "  %d  %s\n", i+1, view.Session(e, now))
	}
	if more := len(list) - maxLiveShown; more > 0 {
		fmt.Fprintf(&b, "  +%d more (`aivoicebus who` lists them)\n", more)
	}
	b.WriteString("  " + next)
	return "", "", errors.New(b.String()) // nextstep: the last line says how to name the session
}

// ReadWithin is how long the one session a message names may have read nothing before a send says so
// (send --read-within changes it, 0 turns it off). A session reads at a prompt (the hooks) or in a wait,
// so one busy with a long task of its own reads only when it next stops: it can still count as seen,
// since every message it sends moves its last-seen, and not have read anything for hours.
const ReadWithin = 2 * time.Hour

// LastRead is when a session last read its mail, as far as the roster knows: when it last declared its
// labels, which every read does; for a session that never declared any (or a node too old to say), when it
// was last seen, sends included.
func LastRead(e msg.RosterEntry) time.Time {
	if !e.LabelsAt.IsZero() {
		return e.LabelsAt
	}
	return e.Seen
}

// Unread is the one session a message names that has read nothing for a while.
type Unread struct {
	Target string
	Entry  msg.RosterEntry
	// Repo is the repository it declared. Live are the sessions live in that repo on any machine, or, when
	// it declared none, the ones live on its machine (LiveOn); never the sender, it, or a join probe.
	Repo string
	Live []msg.RosterEntry
	// Owner is the subject of the owner record found for its repository (repo:OWNER/NAME) when nobody in the
	// repo is live; NoOwner says none is recorded (FindOwner).
	Owner   string
	NoOwner bool
}

// UnreadTarget reports whether to names one exact session the roster knows that is not in a wait and has
// read nothing (LastRead) for longer than readWithin. A session the roster does not know is StaleTarget's to
// judge, and readWithin 0 or less turns the check off.
func UnreadTarget(roster []msg.RosterEntry, to, me string, now time.Time, readWithin time.Duration) (Unread, bool) {
	to = strings.ToLower(strings.TrimSpace(to))
	if readWithin <= 0 || !msg.OneSession(to) || msg.IsProbe(to) || strings.EqualFold(to, me) {
		return Unread{}, false
	}
	i := slices.IndexFunc(roster, func(e msg.RosterEntry) bool { return strings.EqualFold(e.Me, to) })
	if i < 0 || roster[i].Listening || now.Sub(LastRead(roster[i])) <= readWithin {
		return Unread{}, false
	}
	u := Unread{Target: to, Entry: roster[i], Repo: roster[i].Labels["repo"]}
	if u.Repo == "" {
		u.Live = LiveOn(roster, msg.HostOf(to), me, "", now, LiveWithin, to)
	} else {
		u.Live = liveWhere(roster, func(e msg.RosterEntry) bool { return strings.EqualFold(e.Labels["repo"], u.Repo) }, me, u.Repo, now, LiveWithin, to)
	}
	return u, true
}

// FindOwner looks up the owner record of the session's repository (subject repo:OWNER/NAME) when nobody
// else in the repo is live, with resolve (ResolveOwner on the nodes). A lookup that fails for another reason
// than "none is recorded" (two owners, nodes unreachable) leaves both fields unset: the warning then says
// nothing about an owner.
func (u *Unread) FindOwner(resolve func(subject string) (msg.OwnerResolution, error)) {
	if u.Repo == "" || len(u.Live) > 0 {
		return
	}
	subject := "repo:" + strings.ToLower(u.Repo)
	if msg.ValidOwnerLookup(subject) != nil {
		return
	}
	switch _, err := resolve(subject); {
	case err == nil:
		u.Owner = subject
	case errors.Is(err, msg.ErrNoOwner):
		u.NoOwner = true
	}
}

// Warning is what a send to a session that reads nothing says: how long since it last read, and who to try
// instead (the live sessions in its repo, or the repo's owner record, or who is live on its machine).
func (u Unread) Warning(now time.Time) string {
	read := LastRead(u.Entry)
	head := fmt.Sprintf("warning: %s has read nothing for %s (last read %s), so it may not see this until its next prompt",
		view.Name(u.Target, msg.MaxIdentity), ago(now.Sub(read)), view.When(read, now))
	if u.Repo == "" {
		return head + "; " + Stale{Target: u.Target, Live: u.Live}.liveLine(now, LiveWithin)
	}
	repo := view.Name(u.Repo, 40)
	switch {
	case len(u.Live) > 0:
		return head + "; live in repo " + repo + " now: " + liveNames(u.Live, now)
	case u.Owner != "":
		return fmt.Sprintf("%s; nobody else in repo %s is live, and its owner is recorded: `aivoicebus task owner:%s \"...\"`", head, repo, view.Name(u.Owner, 80))
	case u.NoOwner:
		return fmt.Sprintf("%s; nobody else in repo %s is live, and no owner is recorded for it (`aivoicebus owner set repo:%s` records one)", head, repo, view.Name(strings.ToLower(u.Repo), 80))
	}
	return fmt.Sprintf("%s; nobody else in repo %s was seen in the last %s", head, repo, ago(LiveWithin))
}

// liveNames lists live sessions as the stale-target notice does: name, title and state, at most maxLiveShown.
func liveNames(live []msg.RosterEntry, now time.Time) string {
	shown := live[:min(len(live), maxLiveShown)]
	names := make([]string, len(shown))
	for i, e := range shown {
		state := since(now.Sub(e.Seen))
		if e.Listening {
			state = "listening"
		}
		names[i] = view.Name(e.Me, msg.MaxIdentity) + view.TitleSuffix(e.Title) + " (" + state + ")"
	}
	line := strings.Join(names, ", ")
	if more := len(live) - len(shown); more > 0 {
		line += fmt.Sprintf(", +%d more", more)
	}
	return line
}

// HasSuccessor reports whether the nodes can say which session replaces the stale one: they know it
// and it declared a repository or a role.
func (s Stale) HasSuccessor() bool {
	return s.Known && (s.Entry.Labels["repo"] != "" || len(msg.RolesOf(s.Entry.Labels)) > 0)
}

// NoSuccessor is why no session can be named as the one that replaces the stale one.
func (s Stale) NoSuccessor() string {
	if !s.Known {
		return "the nodes have not heard of " + view.Name(s.Target, msg.MaxIdentity) + ", so they do not know its repository"
	}
	return view.Name(s.Target, msg.MaxIdentity) + " declared no repository and no role, so nothing says which session replaces it"
}

// successorLine says where a message for the stale session goes: to the session that replaces it.
// Who that is when it is read is the node's choice (the newest live one then); this is who it would be now.
func successorLine(st Stale, roster []msg.RosterEntry, me string, now time.Time) string {
	host := view.Name(msg.HostOf(st.Target), msg.MaxIdentity)
	what := "the newest live session of its repository on " + host
	if repo := st.Entry.Labels["repo"]; repo != "" {
		what = "the newest live session in repo " + view.Name(repo, 40) + " on " + host
	}
	others := slices.DeleteFunc(slices.Clone(roster), func(e msg.RosterEntry) bool { return strings.EqualFold(e.Me, me) })
	next, ok := msg.Successor(st.Entry, others, nil, now)
	head := fmt.Sprintf("%s %s: its mail goes to the session that replaces it, %s", view.Name(st.Target, msg.MaxIdentity), st.Seen(now), what)
	if !ok {
		return head + "; nobody there is live yet, so it waits for the next one"
	}
	return fmt.Sprintf("%s (now %s, seen %s)", head, view.Name(next.Me, msg.MaxIdentity), since(now.Sub(next.Seen)))
}

// successorWhy is why a message cannot carry the mark that lets the nodes hand it to a successor.
func successorWhy(m msg.Message) string {
	switch {
	case !msg.OneSession(m.To):
		return fmt.Sprintf("it applies to a message to one exact session (--to host/tool.session; `aivoicebus who` lists them), not to %s: run it again without --or-successor", view.Name(m.To, 80))
	case msg.IsPersistentSession(m.To):
		return fmt.Sprintf("%s is a name that outlives a restart, so mail for it waits for whoever wears the name: run it again without --or-successor", view.Name(m.To, 80))
	case m.Kind != "" && m.Kind != msg.KindQuestion && m.Kind != msg.KindAnswer:
		return fmt.Sprintf("a %s is the bookkeeping of one conversation and is never handed on: run it again without --or-successor", view.OneLine(m.Kind, 20))
	case msg.PersonTagged(m):
		return "a message for the person on a machine is never handed to another session (the person runs what it says): run it again without --or-successor"
	}
	return "this message cannot be handed to another session: run it again without --or-successor"
}

// since is how long ago something was, as a person says it: "just now", "45m ago", "9h ago", "3d ago".
func since(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	return ago(d) + " ago"
}

// ago is a span as a person reads it in a sentence: 45m, 9h, 3d.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under 1m"
	case d < 2*time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

var (
	// lowStart: a body that opens by saying it needs nothing.
	lowStart = regexp.MustCompile(`(?i)^[\s\[(*#>_-]*(fyi|done|no action)\b`)
	// lowAnywhere: or says so anywhere.
	lowAnywhere = regexp.MustCompile(`(?i)\bno (reply|action) needed\b`)
	// asksYou: words that want an answer without a question mark.
	asksYou = regexp.MustCompile(`(?i)\b(reply to me|who owns)\b`)
	// requests: an FYI that still asks the reader to do, or not do, something. Such a body is not
	// "no action": it keeps its priority.
	requests = regexp.MustCompile(`(?i)\b(please|tell me|let me know|do not|don['’]t)\b`)
	// codeSpans: fenced blocks and `inline code`, where a ? is not a question.
	codeSpans = regexp.MustCompile("(?s)```.*?```|`[^`\\n]*`")
	urls      = regexp.MustCompile(`https?://\S+`)
	// endsSentence: a ? followed by a space, the end, or a closing mark; not the ? of a URL's query
	// (/x?a=b), a regex (.*?x) or a ternary expression.
	endsSentence = regexp.MustCompile(`\?(\s|$|[)\]"'*_])`)
)

// asks reports whether the words want an answer.
func asks(body string) bool {
	text := urls.ReplaceAllString(codeSpans.ReplaceAllString(body, ""), "")
	return endsSentence.MatchString(text) || asksYou.MatchString(text)
}

// PrioLint is what the words of a message say about the priority it was sent at.
type PrioLint struct {
	// Lower: the message says it needs nothing and asks nothing, and no priority was chosen: send it as fyi.
	Lower bool
	// Warning: it goes out as fyi and asks something. fyi is only counted at session start, so a
	// running session never sees it.
	Warning string
}

// LintPriority checks a plain message (no kind, or a question) against the priority the sender
// gave, "" when none was. An explicit priority always stands; the words only lower an unchosen one.
func LintPriority(body, prio, kind string) PrioLint {
	if kind != "" && kind != msg.KindQuestion {
		return PrioLint{}
	}
	if strings.EqualFold(prio, msg.PrioFYI) {
		if asks(body) {
			fix := "--kind question --prio ask"
			if kind == msg.KindQuestion {
				fix = "--prio ask"
			}
			return PrioLint{Warning: "warning: this is sent as fyi, which is only counted at session start (a running session never sees it), but it asks something. To interrupt: " + fix}
		}
		return PrioLint{}
	}
	if prio != "" || kind != "" {
		return PrioLint{}
	}
	if (lowStart.MatchString(body) || lowAnywhere.MatchString(body)) && !asks(body) &&
		!requests.MatchString(codeSpans.ReplaceAllString(body, "")) {
		return PrioLint{Lower: true}
	}
	return PrioLint{}
}

// LoweredNote is what a send says after it lowered a message to fyi.
const LoweredNote = "(sent as fyi; --prio ask to interrupt)"
