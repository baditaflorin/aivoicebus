package msg

import (
	"net/url"
	"strings"
)

// FeatureWaitScope: a held inbox read says what its wait accepts (ParamWaitScope and the three
// below), and the node's roster reports it (RosterEntry.Waits). A session waiting for one reply in
// its own thread is then busy, not listening: a message from anyone else waits for its next prompt.
const FeatureWaitScope = "wait-scope"

// The query parameters of a held inbox read (GET /v1/inbox?wait=S&gen=G) that say what the wait
// accepts. ParamWaitScope=1 marks a client that says so (an older one never sends it, and its wait
// stays "listening (unverified)"); the others narrow it, absent meaning anything.
const (
	ParamWaitScope  = "wscope"
	ParamWaitThread = "wthread"
	ParamWaitFrom   = "wfrom"
	ParamWaitOnly   = "wonly"
)

// What a wait takes besides everything (wait --only): tasks only, or replies only (a message that
// answers another).
const (
	WaitOnlyTasks   = "tasks"
	WaitOnlyReplies = "replies"
)

// WaitScope is what a wait accepts: messages in Thread, from From (a session, a session's prefix
// or a machine), of Only. Empty fields accept anything, so the zero WaitScope accepts every message.
type WaitScope struct {
	Thread string `json:"thread,omitempty"`
	From   string `json:"from,omitempty"`
	Only   string `json:"only,omitempty"`
}

// Open reports whether the wait accepts any message: the session is listening.
func (w WaitScope) Open() bool { return w.Thread == "" && w.From == "" && w.Only == "" }

// Accepts reports whether m ends a wait for w.
func (w WaitScope) Accepts(m Message) bool {
	switch w.Only {
	case WaitOnlyTasks:
		if m.Kind != KindTask {
			return false
		}
	case WaitOnlyReplies:
		if m.Re == "" {
			return false
		}
	}
	if w.Thread != "" && m.Thread != w.Thread {
		return false
	}
	return w.From == "" || m.From == w.From || HostOf(m.From) == w.From || strings.HasPrefix(m.From, w.From+".")
}

// Busy is what a wait that is not open is waiting for, as `ping` and `who` say it after "busy: ".
func (w WaitScope) Busy() string {
	switch {
	case w.Thread != "":
		return "waiting for a reply in " + w.Thread
	case w.From != "":
		return "waiting for " + w.From
	case w.Only == WaitOnlyTasks:
		return "waiting for tasks only"
	case w.Only == WaitOnlyReplies:
		return "waiting for a reply"
	}
	return ""
}

// Query is w as the parameters of a held read, ParamWaitScope included.
func (w WaitScope) Query() string {
	q := "&" + ParamWaitScope + "=1"
	for _, p := range [][2]string{{ParamWaitThread, w.Thread}, {ParamWaitFrom, w.From}, {ParamWaitOnly, w.Only}} {
		if p[1] != "" {
			q += "&" + p[0] + "=" + url.QueryEscape(p[1])
		}
	}
	return q
}

// ParseWaitScope reads what a held read's wait accepts. Nil when the client did not say
// (ParamWaitScope absent), or said something no wait could mean (then the node cannot vouch for it either).
func ParseWaitScope(q url.Values) *WaitScope {
	if q.Get(ParamWaitScope) != "1" {
		return nil
	}
	w := &WaitScope{Thread: q.Get(ParamWaitThread), From: q.Get(ParamWaitFrom), Only: q.Get(ParamWaitOnly)}
	if w.Only == "all" {
		w.Only = ""
	}
	for _, v := range []string{w.Thread, w.From} {
		if v != "" && !ValidIdentity(v) {
			return nil
		}
	}
	if w.Only != "" && w.Only != WaitOnlyTasks && w.Only != WaitOnlyReplies {
		return nil
	}
	return w
}

// Accepts reports whether e's wait right now would end on m, a message from someone else: true
// when it listens to anything (or a node that cannot say what its wait accepts reports it listening,
// as every node did before FeatureWaitScope), or its narrower wait accepts m.
func (e RosterEntry) Accepts(m Message) bool {
	if e.Listening {
		return true
	}
	return e.Waits != nil && e.Waits.Accepts(m)
}

// Unverified reports whether e is listening by a node (or a client) that does not say what the
// wait accepts: it may be waiting for one reply only.
func (e RosterEntry) Unverified() bool { return e.Listening && e.Waits == nil }

// Busy is what e waits for when it is in a wait that does not accept every message ("" when it is
// listening or in no wait): "waiting for a reply in 25b8d841b3".
func (e RosterEntry) Busy() string {
	if e.Listening || e.Waits == nil {
		return ""
	}
	return e.Waits.Busy()
}
