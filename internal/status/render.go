package status

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// problems lists what to act on, each with the one command that does it, in screen order.
func problems(r *Report) []Problem {
	ps := []Problem{}
	add := func(section, text, fix string) { ps = append(ps, Problem{section, text, fix}) }
	host := msg.HostOf(r.Me.ID)

	if t := r.Token; t != nil {
		switch t.Kind {
		case "rejected":
			add("token", "the nodes refuse this machine's token: no message reaches it", "aivoicebus doctor")
		case "shared":
			add("token", "the mesh's shared token: whoever holds it can send as this machine", "the operator: deploy/machine-token.sh add "+host+" --apply")
		}
	}
	if rel := r.Release; rel != nil {
		if rel.Update {
			add("release", fmt.Sprintf("release %s is available (this is %s)", rel.Latest, rel.Here), "aivoicebus update")
		}
		if len(rel.Nodes) > 1 {
			add("release", "the nodes run different releases: "+releases(rel.Nodes), "aivoicebus mesh")
		}
	}
	if r.NoNodes {
		add("nodes", "no node is configured, so nothing reaches this machine", "aivoicebus join --url URL")
	}
	if n := r.Nodes; n != nil {
		if len(n.Down) > 0 {
			add("nodes", fmt.Sprintf("%d of %d nodes do not answer: %s", len(n.Down), n.Configured, strings.Join(n.Down, ", ")), "aivoicebus mesh")
		}
		if n.PeersOK < n.Peers {
			add("nodes", fmt.Sprintf("%d of %d links between nodes are down", n.Peers-n.PeersOK, n.Peers), "aivoicebus mesh")
		}
		if time.Duration(n.SpreadMs)*time.Millisecond > maxSkew {
			add("nodes", "a node's clock is "+seconds(n.SpreadMs)+" off the others (message expiry uses node clocks)", "aivoicebus mesh")
		}
		if n.Up > 0 && time.Duration(abs(n.LocalMs))*time.Millisecond > maxSkew {
			add("nodes", "this machine's clock is "+seconds(abs(n.LocalMs))+" off the nodes'", clockFix())
		}
	}
	if in := r.Inbox; in != nil && in.Unread > 0 {
		add("inbox", unreadText(in), "aivoicebus inbox")
	}
	if t := r.Tasks; t != nil && t.Waiting > 0 {
		add("tasks", fmt.Sprintf("%d task(s) wait for this session or machine: %s", t.Waiting, strings.Join(first(t.IDs, 3), " ")), "aivoicebus queue")
	}
	if p := r.Person; p != nil {
		if len(p.Approvals) > 0 {
			add("person", fmt.Sprintf("%d approval(s) wait for the person", len(p.Approvals)), "the person, in a terminal: aivoicebus approve "+p.Approvals[0])
		}
		if len(p.Questions) > 0 {
			add("person", fmt.Sprintf("%d question(s) wait for the person", len(p.Questions)), "the person, in a terminal: aivoicebus answer")
		}
		if len(p.Stuck) > 0 {
			add("person", fmt.Sprintf("%d agent(s) here are stuck and need the person", len(p.Stuck)), "the person, in a terminal: aivoicebus answer "+p.Stuck[0])
		}
	}
	if h := r.Held; h != nil && h.Count > 0 {
		add("held", fmt.Sprintf("%d message(s) held back from agents", h.Count), "the person, in a terminal: aivoicebus held")
	}
	if p := r.Pause; p != nil && p.Active {
		fix := "a person on " + msg.HostOf(p.By) + ", in a terminal: aivoicebus resume (or wait until " + view.When(*p.Until, time.Now()) + ")"
		if msg.HostOf(p.By) == host {
			fix = "a person here, in a terminal: aivoicebus resume"
		}
		text := "paused by " + p.By + " until " + view.When(*p.Until, time.Now()) + ": agents take no task"
		if p.Reason != "" {
			text += fmt.Sprintf(" (%q)", p.Reason)
		}
		add("pause", text, fix)
	}
	if w := r.Worker; w != nil && w.Service == "stopped" && !w.Running {
		add("worker", "the worker service is installed but not running", "aivoicebus worker status")
	}
	var lost []string
	for _, s := range r.Unknown {
		if !r.NoNodes || !network[s] {
			lost = append(lost, s)
		}
	}
	if len(lost) > 0 {
		add("unknown", fmt.Sprintf("no answer within %s for: %s", (time.Duration(r.BudgetMs)*time.Millisecond).String(), strings.Join(lost, ", ")), "aivoicebus doctor")
	}
	return ps
}

// network names the parts that need the nodes.
var network = map[string]bool{"token": true, "nodes": true, "inbox": true, "tasks": true, "person": true, "pause": true, "standby": true}

func clockFix() string {
	if runtime.GOOS == "darwin" {
		return "sudo sntp -sS time.apple.com"
	}
	return "sudo timedatectl set-ntp true"
}

func releases(m map[string]int) string {
	var parts []string
	for v, n := range m {
		parts = append(parts, fmt.Sprintf("%s on %d", v, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func unreadText(in *Inbox) string {
	return fmt.Sprintf("%d unread: %d urgent, %d ask, %d fyi", in.Unread, in.Urgent, in.Ask, in.FYI)
}

// Render prints the report: one line a part, "!" before one to act on (it ends with the command
// that does it), "?" before one that did not answer in time.
func Render(w io.Writer, r Report) {
	fixes := map[string][]Problem{}
	for _, p := range r.Problems {
		fixes[p.Section] = append(fixes[p.Section], p)
	}
	unknown := map[string]bool{}
	for _, s := range r.Unknown {
		unknown[s] = true
	}
	fmt.Fprintf(w, "aivoicebus status (%s, %dms)\n", r.Me.ID, r.TookMs)
	for _, s := range Sections {
		ps := fixes[s]
		text := ""
		if !unknown[s] {
			text = line(r, s)
		}
		switch {
		case unknown[s] && r.NoNodes && network[s]:
			fmt.Fprintf(w, "? %-8s unknown: no node is configured\n", s)
		case unknown[s]:
			fmt.Fprintf(w, "? %-8s unknown: no answer within %dms\n", s, r.BudgetMs)
		case text == "" && len(ps) > 0: // the part is its problem
			fmt.Fprintf(w, "! %-8s %s -> %s\n", s, ps[0].Text, ps[0].Fix)
			ps = ps[1:]
		default:
			fmt.Fprintf(w, "  %-8s %s\n", s, text)
		}
		for _, p := range ps {
			fmt.Fprintf(w, "! %-8s %s -> %s\n", "", p.Text, p.Fix)
		}
	}
	for _, p := range fixes["unknown"] {
		fmt.Fprintf(w, "? %-8s %s -> %s\n", "", p.Text, p.Fix)
	}
}

// line is a part's summary when there is nothing to act on; "" leaves the line to the part's problem.
func line(r Report, section string) string {
	switch section {
	case "me":
		if r.Me.Why == "" {
			return r.Me.ID
		}
		return r.Me.ID + " (" + r.Me.Why + ")"
	case "token":
		switch t := r.Token; t.Kind {
		case "machine":
			return "a machine token, bound to " + t.Host
		case "observer":
			return "the observer token: read-only, this machine cannot send"
		case "shared", "rejected":
			return ""
		}
		return "kind unknown (the nodes are too old to say)"
	case "release":
		rel := r.Release
		s := rel.Here + " here"
		if len(rel.Nodes) > 0 {
			s += "; nodes run " + releases(rel.Nodes)
		}
		switch {
		case rel.Update:
			return s + "; " + rel.Latest + " is available"
		case rel.Latest != "":
			return s + "; up to date"
		}
		return s
	case "nodes":
		n := r.Nodes
		if n == nil { // no node configured: the problem says so
			return ""
		}
		s := fmt.Sprintf("%d of %d up", n.Up, n.Configured)
		if n.Up > 0 {
			s += fmt.Sprintf("; peers %d/%d ok; clocks within %s, this machine %s off", n.PeersOK, n.Peers, seconds(n.SpreadMs), seconds(abs(n.LocalMs)))
		}
		return s
	case "inbox":
		if r.Inbox.Unread > 0 {
			return ""
		}
		return "nothing unread"
	case "tasks":
		if r.Tasks.Waiting > 0 {
			return ""
		}
		return "none waiting for this session or machine"
	case "person":
		if p := r.Person; len(p.Questions)+len(p.Approvals)+len(p.Stuck) > 0 {
			return ""
		}
		return "no question or approval waits for the person"
	case "held":
		if r.Held.Count == 0 {
			return "nothing held back from agents"
		}
		return ""
	case "pause":
		if r.Pause.Active {
			return ""
		}
		return "none"
	case "worker":
		w := r.Worker
		switch {
		case w.Running && w.Detail != "":
			return "running (" + w.Detail + ")"
		case w.Running:
			return "running"
		case w.Service == "stopped":
			return ""
		}
		return "not running (service " + w.Service + ")"
	case "standby":
		if len(r.Standby.Listening) == 0 {
			return "no session here is waiting for a message"
		}
		return fmt.Sprintf("%d listening here: %s", len(r.Standby.Listening), strings.Join(first(r.Standby.Listening, 3), ", "))
	case "quiet":
		q := r.Quiet
		if q.Window == "off" && q.Until == nil {
			return "off"
		}
		var parts []string
		if q.Window != "off" {
			parts = append(parts, q.Window+" daily")
		}
		if q.Until != nil {
			parts = append(parts, "until "+view.When(*q.Until, time.Now()))
		}
		state := "not active now"
		if q.Active {
			state = "active now: only urgent messages reach sessions"
		}
		return strings.Join(parts, ", ") + ", " + state
	}
	return ""
}

func first(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(xs[:n:n], fmt.Sprintf("and %d more", len(xs)-n))
}
