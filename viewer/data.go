package viewer

import (
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Session is one card: who a session is and what it is doing, as it declared it. Every string is
// self-declared by the session and cleaned here; a reader still treats it as untrusted.
type Session struct {
	Me       string    `json:"me"`
	Title    string    `json:"title,omitempty"`
	Tool     string    `json:"tool"`
	Model    string    `json:"model,omitempty"` // the model it runs, as it declared it (its model label, else its latest message)
	Machine  string    `json:"machine"`
	Repo     string    `json:"repo,omitempty"`
	Branch   string    `json:"branch,omitempty"`
	LastSeen time.Time `json:"last_seen"`
	Sent     int       `json:"sent"`
	Received int       `json:"received"` // messages addressed to exactly this session
	Threads  int       `json:"threads"`  // conversations it took part in

	modelAt        time.Time // when the message the model came from was sent
	modelFromLabel bool      // the roster's label decides; messages only fill in
	// Hidden says why the page leaves the card out by default: "automation" (a probe, a check or a
	// test identity, see Automated) or "no messages" (it was only ever seen, never wrote or was written
	// to by name). Empty: shown. The page has a toggle; nothing is dropped from the data.
	Hidden string `json:"hidden,omitempty"`
}

// automationWords are the name parts that mark an identity as a script's rather than a session's:
// `<host>/join-probe`, `rollout/agent`, `x/check-060`, `y/receipt-test`.
var automationWords = map[string]bool{
	"probe": true, "check": true, "checks": true, "test": true, "tests": true, "verify": true, "rollout": true,
	"smoke": true, "selftest": true, "e2e": true, "lab": true, "postdeploy": true, "canary": true,
}

// Automated reports whether identity me looks like a script's: a join probe, or a host or agent
// whose name has a whole word like probe, check, test, verify or rollout ("contests-macbook" does
// not: the word must stand alone). A guess for display only; it never decides delivery.
func Automated(me string) bool {
	if msg.IsProbe(me) {
		return true
	}
	host, agent, _ := strings.Cut(strings.ToLower(me), "/")
	tool, _, _ := strings.Cut(agent, ".")
	for _, part := range []string{host, tool} {
		for _, w := range strings.FieldsFunc(part, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
			if automationWords[w] {
				return true
			}
		}
	}
	return false
}

// Participant is a session in a conversation, with the name it had there.
type Participant struct {
	Me    string `json:"me"`
	Title string `json:"title,omitempty"`
}

// Conversation is one thread: who took part (senders, and sessions addressed by name), the audience
// when it went wider (all, a machine, a selector), what it is about, and when it last moved.
type Conversation struct {
	Thread       string        `json:"thread"`
	Subject      string        `json:"subject,omitempty"`
	Participants []Participant `json:"participants"`
	Audience     []string      `json:"audience,omitempty"` // targets that are not one session: "all", "laptop-a/*", "@repo=x"
	Messages     int           `json:"messages"`
	First        time.Time     `json:"first"`
	Last         time.Time     `json:"last"`
	LastFrom     string        `json:"last_from"`
	Withdrawn    int           `json:"withdrawn,omitempty"`
	// Threads lists every thread of the conversation when one message went to several sessions as
	// separate copies (a fan-out: each copy is a thread of its own); empty for a single thread.
	Threads []string `json:"threads,omitempty"`
	// Hidden is true when every participant is an automation identity (see Automated).
	Hidden bool `json:"hidden,omitempty"`
}

// fanoutWindow is how far apart the copies of one message to several sessions may be sent: a sender
// sends them one after another, seconds apart.
const fanoutWindow = 2 * time.Minute

// ThreadMessage is a message as the thread view shows it.
type ThreadMessage struct {
	msg.Message
	ReadBy int `json:"read_by"`
	// Recipients are the sessions this text went to: one, or all the copies of a fan-out.
	Recipients []string `json:"recipients"`
}

const (
	maxName    = msg.MaxIdentity
	maxLabel   = 64
	nameShown  = 120
	maxSubject = view.SubjectRunes
)

// oneSession is the identity of exactly one session: host/agent with no wildcard and no selector.
func oneSession(to string) bool {
	return !strings.HasPrefix(to, "@") && strings.Count(to, "/") == 1 && !strings.Contains(to, "*") && !strings.HasSuffix(to, "/")
}

func clean(s string, n int) string { return view.OneLine(view.Sanitise(s), n) }

// Sessions builds the cards from the roster and the messages: every session the roster knows,
// plus any that only appears as a sender, newest activity first.
func Sessions(roster []msg.RosterEntry, ms []msg.Message) []Session {
	by := map[string]*Session{}
	get := func(me string) *Session {
		s, ok := by[me]
		if !ok {
			host, agent, _ := strings.Cut(me, "/")
			tool, _, _ := strings.Cut(agent, ".")
			s = &Session{Me: clean(me, nameShown), Tool: clean(tool, maxLabel), Machine: clean(host, maxLabel)}
			by[me] = s
		}
		return s
	}
	for _, e := range roster {
		s := get(e.Me)
		if e.Seen.After(s.LastSeen) {
			s.LastSeen = e.Seen
		}
		if t := msg.CleanTitle(e.Title); t != "" {
			s.Title = t
		}
		if v := e.Labels["tool"]; v != "" {
			s.Tool = clean(v, maxLabel)
		}
		if v := e.Labels["machine"]; v != "" {
			s.Machine = clean(v, maxLabel)
		}
		s.Repo, s.Branch = clean(e.Labels["repo"], maxLabel), clean(e.Labels["branch"], maxLabel)
		if m := msg.CleanModel(e.Labels[msg.ModelLabel]); m != "" {
			s.Model, s.modelFromLabel = m, true
		}
	}
	threads := map[string]map[string]bool{}
	note := func(me, thread string) {
		if threads[me] == nil {
			threads[me] = map[string]bool{}
		}
		threads[me][thread] = true
	}
	for _, m := range ms {
		if m.From == "" {
			continue
		}
		s := get(m.From)
		s.Sent++
		if m.At.After(s.LastSeen) {
			s.LastSeen = m.At
		}
		if mm := msg.CleanModel(m.FromModel); mm != "" && !m.At.Before(s.modelAt) && !s.modelFromLabel {
			s.Model, s.modelAt = mm, m.At // the roster does not say: the latest message does
		}
		if s.Title == "" {
			if t := msg.CleanTitle(m.FromTitle); t != "" {
				s.Title = t // the roster forgot it; the message still says what it was called
			}
		}
		note(m.From, m.Thread)
		if oneSession(m.To) {
			if r, ok := by[m.To]; ok {
				r.Received++
				note(m.To, m.Thread)
			}
		}
	}
	out := make([]Session, 0, len(by))
	for me, s := range by {
		s.Threads = len(threads[me])
		switch {
		case Automated(me):
			s.Hidden = "automation"
		case s.Sent == 0 && s.Received == 0:
			s.Hidden = "no messages"
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Me < out[j].Me
	})
	return out
}

// Conversations groups the messages by thread, newest activity first, and puts the copies of one
// message sent to several sessions (the same sender and text, each its own thread, sent within
// fanoutWindow) into one conversation with all of them. With session set, only the conversations
// that session took part in (as a sender or addressed by name).
func Conversations(ms []msg.Message, titles map[string]string, session string) []Conversation {
	type acc struct {
		c       Conversation
		names   map[string]string
		aud     map[string]bool
		all     []msg.Message
		threads []string
	}
	byThread := map[string]*acc{}
	var order []string
	for _, m := range ms {
		t := m.Thread
		if t == "" {
			t = m.ID
		}
		a, ok := byThread[t]
		if !ok {
			a = &acc{c: Conversation{Thread: clean(t, msg.MaxRef), First: m.At}, names: map[string]string{}, aud: map[string]bool{}, threads: []string{t}}
			byThread[t] = a
			order = append(order, t)
		}
		a.all = append(a.all, m)
	}
	// fan-outs: threads whose opener has the same sender and text, opened close together
	type opener struct {
		key string
		at  time.Time
	}
	openers := map[string]opener{}
	for _, t := range order {
		for _, m := range byThread[t].all {
			if m.ID == t && m.Re == "" && m.Retracted == nil && oneSession(m.To) {
				openers[t] = opener{m.From + "\x00" + m.Body, m.At}
			}
		}
	}
	merged := map[string]string{} // thread -> the thread it was merged into
	var kept []string
	lead := map[string][]string{} // key -> lead threads with that key
	for _, t := range order {
		o, ok := openers[t]
		if ok {
			for _, l := range lead[o.key] {
				if d := o.at.Sub(openers[l].at); d >= -fanoutWindow && d <= fanoutWindow {
					merged[t] = l
					break
				}
			}
			if merged[t] == "" {
				lead[o.key] = append(lead[o.key], t)
			}
		}
		if merged[t] == "" {
			kept = append(kept, t)
		}
	}
	for _, t := range order {
		if into := merged[t]; into != "" {
			l := byThread[into]
			l.all = append(l.all, byThread[t].all...)
			l.threads = append(l.threads, t)
		}
	}
	var out []Conversation
	for _, t := range kept {
		a := byThread[t]
		for _, m := range a.all {
			a.c.Messages++
			if m.Retracted != nil {
				a.c.Withdrawn++
			}
			if m.At.Before(a.c.First) {
				a.c.First = m.At
			}
			if !m.At.Before(a.c.Last) {
				a.c.Last, a.c.LastFrom = m.At, clean(m.From, nameShown)
			}
			if _, ok := a.names[m.From]; !ok || m.FromTitle != "" {
				a.names[m.From] = msg.CleanTitle(m.FromTitle) // the latest name it had in this conversation
			}
			if oneSession(m.To) {
				if _, ok := a.names[m.To]; !ok {
					a.names[m.To] = ""
				}
			} else {
				a.aud[clean(m.To, nameShown)] = true
			}
		}
		if session != "" {
			if _, ok := a.names[session]; !ok {
				continue
			}
		}
		a.c.Subject = view.Subject(a.all)
		automated := len(a.names) > 0
		for me, title := range a.names {
			if title == "" {
				title = msg.CleanTitle(titles[me])
			}
			a.c.Participants = append(a.c.Participants, Participant{Me: clean(me, nameShown), Title: title})
			automated = automated && Automated(me)
		}
		a.c.Hidden = automated
		sort.Slice(a.c.Participants, func(i, j int) bool { return a.c.Participants[i].Me < a.c.Participants[j].Me })
		for x := range a.aud {
			a.c.Audience = append(a.c.Audience, x)
		}
		sort.Strings(a.c.Audience)
		if len(a.threads) > 1 {
			for _, th := range a.threads {
				a.c.Threads = append(a.c.Threads, clean(th, msg.MaxRef))
			}
		}
		out = append(out, a.c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

// Collapse shows the copies of a fanned-out message once: the same sender and text within
// fanoutWindow becomes one entry addressed to all its recipients, read by the sum of their readers.
// Everything else stays as it is, in time order.
func Collapse(ms []ThreadMessage) []ThreadMessage {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].At.Before(ms[j].At) })
	out := []ThreadMessage{} // never null in JSON
	for _, m := range ms {
		joined := false
		if m.Re == "" && m.Retracted == nil {
			for i := range out {
				o := &out[i]
				if o.Re == "" && o.Retracted == nil && o.From == m.From && o.Body == m.Body && m.At.Sub(o.At) <= fanoutWindow {
					o.Recipients = append(o.Recipients, m.To)
					o.ReadBy += m.ReadBy
					joined = true
					break
				}
			}
		}
		if !joined {
			m.Recipients = []string{m.To}
			out = append(out, m)
		}
	}
	return out
}
