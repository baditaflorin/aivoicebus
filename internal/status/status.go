// Package status is `aivoicebus status` with no message id: one screen that says where this session
// stands (who it is, its token, the release, the nodes, what waits for it and for its person, pauses,
// the worker, standby and quiet hours), each problem with the command that fixes it.
//
// Every part is gathered at once, under one short budget: a node that hangs costs the screen that
// budget and no more, and whatever has not answered by then is shown as unknown.
package status

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// DefaultBudget is how long the screen waits for its parts. A hedged read gets past one hung node
// in a quarter of a second, so a healthy mesh answers well inside it.
const DefaultBudget = 1500 * time.Millisecond

// minLeft is the least time the parts get, however long the caller took before Gather.
const minLeft = 500 * time.Millisecond

// maxSkew is the clock difference that counts as a problem (as `aivoicebus mesh` uses).
const maxSkew = 2 * time.Second

// Options say what to look at. Probes left nil are not asked, and their parts stay unknown.
type Options struct {
	// Client reaches the nodes; nil when none is configured.
	Client *client.Client
	Me     string
	Why    func() string // how the identity was worked out
	Here   string        // the release this binary is
	Budget time.Duration // 0 means DefaultBudget
	// Start is when the command started (zero: now). The budget counts from it, so the time spent
	// working out the identity before Gather is part of it.
	Start time.Time
	Now    func() time.Time

	Quiet  func() config.Quiet
	Held   func() int     // messages held back from agents and not released by the person
	Worker func() *Worker // this machine's worker, from the service manager and the worker's lock
}

// Report is the whole screen. A part that did not answer within the budget is nil, and named in Unknown.
type Report struct {
	Me       Me        `json:"me"`
	Token    *Token    `json:"token"`
	Release  *Release  `json:"release"`
	Nodes    *Nodes    `json:"nodes"`
	Inbox    *Inbox    `json:"inbox"`
	Tasks    *Tasks    `json:"tasks"`
	Person   *Person   `json:"person"`
	Held     *Held     `json:"held"`
	Pause    *Pause    `json:"pause"`
	Worker   *Worker   `json:"worker"`
	Standby  *Standby  `json:"standby"`
	Quiet    *Quiet    `json:"quiet"`
	NoNodes  bool      `json:"no_nodes"` // no node is configured: the parts that need one are unknown
	Problems []Problem `json:"problems"`
	Unknown  []string  `json:"unknown"`
	TookMs   int64     `json:"took_ms"`
	BudgetMs int64     `json:"budget_ms"`
}

// Me is who this session is on the mesh, and how that was decided.
type Me struct {
	ID  string `json:"id"`
	Why string `json:"why"`
}

// Token is what the nodes say this machine's token is: machine, shared, observer, rejected, or
// "" from nodes too old to say.
type Token struct {
	Kind string `json:"kind"`
	Host string `json:"host,omitempty"`
}

// Release compares the release here with what the nodes run and announce.
type Release struct {
	Here   string         `json:"here"`
	Nodes  map[string]int `json:"nodes"`  // release -> how many answering nodes run it
	Latest string         `json:"latest"` // the newest release a node announced ("" if none did)
	Update bool           `json:"update"` // Latest is newer than Here: `aivoicebus update` installs it
}

// Nodes is the mesh in one line.
type Nodes struct {
	Configured int      `json:"configured"`
	Up         int      `json:"up"`
	Down       []string `json:"down"` // host:port of each node that did not answer
	PeersOK    int      `json:"peers_ok"`
	Peers      int      `json:"peers"`
	SpreadMs   int64    `json:"spread_ms"` // the furthest a node's clock is from the middle of the nodes' clocks
	LocalMs    int64    `json:"local_ms"`  // the middle of the nodes' clocks minus this machine's
	List       []Node   `json:"list"`
}

// Node is one configured node.
type Node struct {
	Host    string `json:"host"` // host:port only: a node URL may carry credentials
	Name    string `json:"name,omitempty"`
	Up      bool   `json:"up"`
	Version string `json:"version,omitempty"`
	SkewMs  int64  `json:"skew_ms"`
	PeersOK int    `json:"peers_ok"`
	Peers   int    `json:"peers"`
}

// Inbox counts this session's unread messages (tasks apart) by priority.
type Inbox struct {
	Unread int `json:"unread"`
	Urgent int `json:"urgent"`
	Ask    int `json:"ask"`
	FYI    int `json:"fyi"`
}

// Tasks are the tasks waiting for this session: sent to it or its machine, not taken yet.
type Tasks struct {
	Waiting int      `json:"waiting"`
	IDs     []string `json:"ids"`
}

// Person is what waits for this machine's person: questions (ask-human) and approvals.
type Person struct {
	Questions []string `json:"questions"` // message ids
	Approvals []string `json:"approvals"`
}

// Held counts the messages kept from agents that the person has not released.
type Held struct {
	Count int `json:"count"`
}

// Pause is the kill switch in force for this session, if any.
type Pause struct {
	Active bool       `json:"active"`
	By     string     `json:"by,omitempty"`
	Until  *time.Time `json:"until,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

// Worker is this machine's worker.
type Worker struct {
	Running bool   `json:"running"`
	Service string `json:"service"` // "running", "stopped" or "not installed"
	Detail  string `json:"detail,omitempty"`
}

// Standby lists the sessions on this machine waiting for a message right now.
type Standby struct {
	Listening []string `json:"listening"`
}

// Quiet is the do-not-disturb setting.
type Quiet struct {
	Window string     `json:"window"` // "22:00-07:00", or "off"
	Until  *time.Time `json:"until,omitempty"`
	Active bool       `json:"active"`
}

// Problem is one thing to act on, and the command that does it.
type Problem struct {
	Section string `json:"section"`
	Text    string `json:"text"`
	Fix     string `json:"fix"`
}

// Sections in screen order.
var Sections = []string{"me", "token", "release", "nodes", "inbox", "tasks", "person", "held", "pause", "worker", "standby", "quiet"}

// part is one gathered piece, applied to the report by Gather's own goroutine only.
type part struct {
	name  string
	apply func(*Report)
}

// Gather asks everything at once and returns what answered within the budget.
func Gather(o Options) Report {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	budget := o.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	start := o.Start
	if start.IsZero() {
		start = time.Now()
	}
	// What is left of the budget, but never less than minLeft: a slow identity probe must not
	// leave the nodes no time at all.
	deadline := start.Add(budget)
	if floor := time.Now().Add(minLeft); deadline.Before(floor) {
		deadline = floor
	}
	// The nodes are asked to answer a little before the screen is drawn, so a part that waited out
	// a hung node still arrives in time to be shown.
	netDeadline := deadline.Add(-min(200*time.Millisecond, time.Until(deadline)/5))
	r := Report{Me: Me{ID: o.Me}, BudgetMs: budget.Milliseconds(), NoNodes: o.Client == nil}

	var latestMu sync.Mutex
	latest := ""
	noteLatest := func(v string) {
		latestMu.Lock()
		defer latestMu.Unlock()
		if v != "" && (latest == "" || release.Newer(v, latest)) {
			latest = v
		}
	}
	// clone is a client of its own for one part: a Client's headers must not be shared between
	// goroutines, and each part is bounded by the same deadline.
	clone := func(urls []string) *client.Client {
		c := o.Client
		return &client.Client{URLs: urls, Token: c.Token, Labels: c.Labels, Title: c.Title, HedgeDelay: c.HedgeDelay, Timeout: budget, Deadline: netDeadline}
	}

	parts := make(chan part, 16)
	pending := map[string]bool{}
	launch := func(name string, f func() func(*Report)) {
		pending[name] = true
		go func() { parts <- part{name, f()} }()
	}

	if o.Why != nil {
		launch("me", func() func(*Report) {
			why := o.Why()
			return func(r *Report) { r.Me.Why = why }
		})
	}
	if o.Quiet != nil {
		launch("quiet", func() func(*Report) {
			q, t := o.Quiet(), now()
			v := &Quiet{Window: q.String(), Active: q.Active(t)}
			if q.Until.After(t) {
				u := q.Until
				v.Until = &u
			}
			return func(r *Report) { r.Quiet = v }
		})
	}
	if o.Held != nil {
		launch("held", func() func(*Report) {
			n := o.Held()
			return func(r *Report) { r.Held = &Held{Count: n} }
		})
	}
	if o.Worker != nil {
		launch("worker", func() func(*Report) {
			w := o.Worker()
			return func(r *Report) { r.Worker = w }
		})
	}
	if o.Client != nil {
		urls := o.Client.URLs
		launch("token", func() func(*Report) {
			c := clone(urls)
			kind, host, err := c.TokenKind()
			noteLatest(c.Latest)
			switch {
			case client.IsRejected(err):
				return func(r *Report) { r.Token = &Token{Kind: "rejected"} }
			case client.IsNotFound(err):
				return func(r *Report) { r.Token = &Token{} }
			case err != nil:
				return nil
			}
			return func(r *Report) { r.Token = &Token{Kind: kind, Host: host} }
		})
		launch("nodes", func() func(*Report) {
			n := nodes(urls, o.Client.Token, netDeadline, budget, noteLatest)
			return func(r *Report) { r.Nodes = n }
		})
		launch("inbox", func() func(*Report) {
			ms, err := clone(urls).Inbox(o.Me, false)
			if err != nil {
				return nil
			}
			in, ts := &Inbox{}, &Tasks{IDs: []string{}}
			for _, m := range ms {
				if m.Kind == msg.KindTask {
					ts.Waiting++
					ts.IDs = append(ts.IDs, m.ID)
					continue
				}
				in.Unread++
				switch m.Prio {
				case msg.PrioUrgent:
					in.Urgent++
				case msg.PrioAsk:
					in.Ask++
				default:
					in.FYI++
				}
			}
			return func(r *Report) { r.Inbox, r.Tasks = in, ts }
		})
		launch("pause", func() func(*Report) {
			// All, read or not: a pause the hook already showed (and marked read) is still in force.
			ms, err := clone(urls).Inbox(o.Me, true)
			if err != nil {
				return nil
			}
			p := &Pause{}
			if m, until, on := msg.ActivePause(ms, now()); on {
				_, why, _ := msg.ParsePause(m.Body)
				p = &Pause{Active: true, By: view.Name(m.From, msg.MaxIdentity), Until: &until, Reason: view.OneLine(why, 100)}
			}
			return func(r *Report) { r.Pause = p }
		})
		launch("person", func() func(*Report) {
			ms, err := clone(urls).Inbox(msg.HumanOf(o.Me), false)
			if err != nil {
				return nil
			}
			p := &Person{Questions: []string{}, Approvals: []string{}}
			for _, m := range ms {
				switch {
				case strings.HasPrefix(m.Body, msg.AskPrefix):
					p.Questions = append(p.Questions, m.ID)
				case strings.HasPrefix(m.Body, "[approval needed]"):
					p.Approvals = append(p.Approvals, m.ID)
				}
			}
			return func(r *Report) { r.Person = p }
		})
		launch("standby", func() func(*Report) {
			es, err := clone(urls).RosterAll()
			if err != nil {
				return nil
			}
			s := &Standby{Listening: []string{}}
			for _, e := range es {
				if e.Listening && msg.HostOf(e.Me) == msg.HostOf(o.Me) && !msg.IsProbe(e.Me) {
					s.Listening = append(s.Listening, view.Name(e.Me, msg.MaxIdentity))
				}
			}
			sort.Strings(s.Listening)
			return func(r *Report) { r.Standby = s }
		})
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for len(pending) > 0 {
		select {
		case p := <-parts:
			delete(pending, p.name)
			if p.apply != nil {
				p.apply(&r)
			}
			continue
		case <-timer.C:
		}
		break
	}

	latestMu.Lock()
	rel := &Release{Here: o.Here, Nodes: map[string]int{}, Latest: latest}
	latestMu.Unlock()
	if r.Nodes != nil {
		for _, n := range r.Nodes.List {
			if n.Up {
				v := n.Version
				if v == "" {
					v = "unknown"
				}
				rel.Nodes[v]++
			}
		}
	}
	rel.Update = release.Newer(rel.Latest, rel.Here)
	r.Release = rel
	r.Unknown = []string{}
	for _, s := range Sections {
		if !known(&r, s) {
			r.Unknown = append(r.Unknown, s)
		}
	}
	r.Problems = problems(&r)
	r.TookMs = time.Since(start).Milliseconds()
	return r
}

func known(r *Report, section string) bool {
	switch section {
	case "me":
		return r.Me.ID != ""
	case "token":
		return r.Token != nil
	case "release":
		return r.Release != nil
	case "nodes":
		return r.Nodes != nil
	case "inbox":
		return r.Inbox != nil
	case "tasks":
		return r.Tasks != nil
	case "person":
		return r.Person != nil
	case "held":
		return r.Held != nil
	case "pause":
		return r.Pause != nil
	case "worker":
		return r.Worker != nil
	case "standby":
		return r.Standby != nil
	case "quiet":
		return r.Quiet != nil
	}
	return false
}

// nodes asks each node about itself at once, each bounded by deadline.
func nodes(urls []string, token string, deadline time.Time, budget time.Duration, noteLatest func(string)) *Nodes {
	list := make([]Node, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			n := Node{Host: hostOf(u)}
			c := &client.Client{URLs: []string{u}, Token: token, Timeout: budget, Deadline: deadline}
			t0 := time.Now()
			h, err := c.Health()
			t1 := time.Now()
			noteLatest(c.Latest)
			if err == nil {
				n.Up, n.Name, n.Version = true, h.Node, h.Version
				n.SkewMs = h.Now.Sub(t0.Add(t1.Sub(t0) / 2)).Milliseconds()
				for _, p := range h.Peers {
					n.Peers++
					if p.OK {
						n.PeersOK++
					}
				}
			}
			list[i] = n
		}(i, u)
	}
	wg.Wait()
	out := &Nodes{Configured: len(urls), Down: []string{}, List: list}
	var skews []int64
	for _, n := range list {
		if !n.Up {
			out.Down = append(out.Down, n.Host)
			continue
		}
		out.Up++
		out.Peers += n.Peers
		out.PeersOK += n.PeersOK
		skews = append(skews, n.SkewMs)
	}
	if len(skews) > 0 {
		sorted := append([]int64(nil), skews...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		mid := sorted[len(sorted)/2]
		out.LocalMs = mid
		for _, s := range skews {
			out.SpreadMs = max(out.SpreadMs, abs(s-mid))
		}
	}
	return out
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// hostOf is host:port of a node URL and nothing else of it (it may carry credentials or a path).
func hostOf(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "(bad url)"
	}
	return view.OneLine(p.Host, 80)
}

func seconds(ms int64) string {
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}
