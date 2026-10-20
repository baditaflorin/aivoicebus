package outbox

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Two checks on what a person or an agent writes by hand (Request.Authored). Both look only at what
// the sender already has (the roster, the words), so nothing about them is on the wire.

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
	live := func(e msg.RosterEntry) bool { return e.Listening || now.Sub(e.Seen) <= within }
	st := Stale{Target: to}
	for _, e := range roster {
		if strings.EqualFold(e.Me, to) {
			st.Entry, st.Known = e, true
		}
	}
	if st.Known && live(st.Entry) {
		return Stale{}, false
	}
	repo := st.Entry.Labels["repo"]
	host := msg.HostOf(to)
	for _, e := range roster {
		if msg.HostOf(e.Me) == host && !strings.EqualFold(e.Me, to) && !strings.EqualFold(e.Me, me) && !msg.IsProbe(e.Me) && live(e) {
			st.Live = append(st.Live, e)
		}
	}
	sameRepo := func(e msg.RosterEntry) bool { return repo != "" && e.Labels["repo"] == repo }
	sort.SliceStable(st.Live, func(i, j int) bool {
		a, b := st.Live[i], st.Live[j]
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
	return st, true
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
