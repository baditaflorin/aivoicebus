package tasks

import (
	"fmt"
	"io"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// Getting unstuck: an agent that is blocked says so (`aivoicebus stuck`) instead of going silent
// or looping. The thread's other side hears it as progress, so its wait shows it and goes on; the
// person, or a peer that is listening right now, gets a copy to act on.

// StuckReq is one stuck call.
type StuckReq struct {
	Me, Reason string
	Re         string // the message (a task, usually) the agent is stuck on; "" for none
	Need       string // msg.NeedPerson (""), msg.NeedPeer, or a tool or capability word
	NotifyURL  string // the person's phone (watch notify set); "" for none
}

// StuckResult says where a stuck call went.
type StuckResult struct {
	Side             string   `json:"side,omitempty"`              // the thread's other side
	SideID           string   `json:"side_id,omitempty"`           // the progress message it got
	PersonID         string   `json:"person_id,omitempty"`         // the copy in the person's inbox
	Phoned           bool     `json:"phoned,omitempty"`            // the person's phone was notified
	PhoneUnreachable string   `json:"phone_unreachable,omitempty"` // why the phone could not be told
	Peer             string   `json:"peer,omitempty"`              // the listening session asked for help
	PeerID           string   `json:"peer_id,omitempty"`           // the task it got
	Notes            []string `json:"notes,omitempty"`
}

var needWord = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

// Listening lists the sessions target reaches that wait for messages right now (a message reaches
// them within a second), other than skip. It is what task --any picks from.
func Listening(c *client.Client, target string, skip ...string) ([]msg.RosterEntry, error) {
	roster, err := c.Roster()
	if err != nil {
		return nil, err
	}
	var out []msg.RosterEntry
	for _, e := range roster {
		if !e.Listening || slices.Contains(skip, e.Me) || !msg.MatchesLabeled(target, e.Me, e.Labels) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// otherSide is who in the thread of m hears that me is stuck on it: m's author, or, when me wrote
// m, the session that took it (a task) or the one session it went to.
func otherSide(me string, m msg.Message, ms []msg.Message) string {
	if m.From != me {
		return m.From
	}
	if m.Kind == msg.KindTask {
		if owner, taken := msg.TaskOwner(m.From, Takes(ms, m.ID)); taken && owner.From != me {
			return owner.From
		}
	}
	if m.To != "all" && !msg.IsSelector(m.To) && !strings.Contains(m.To, "*") && m.To != me {
		return m.To
	}
	return ""
}

// Summary is reason as one short line for a phone: no line breaks or control characters, cut
// to n runes.
func Summary(reason string, n int) string {
	var b []rune
	for _, r := range strings.Join(strings.Fields(reason), " ") {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b = append(b, r)
	}
	if len(b) > n {
		return strings.TrimSpace(string(b[:n-1])) + "…"
	}
	return string(b)
}

// SendStuck tells the thread's other side (with Re) and, by Need, the person or a listening peer
// that r.Me is blocked. A credential in the reason stops it before anything is sent.
func SendStuck(c *client.Client, r StuckReq, warn io.Writer) (StuckResult, error) {
	var res StuckResult
	reason := strings.TrimSpace(r.Reason)
	if reason == "" {
		return res, outbox.Usagef(`say what blocks you, like: aivoicebus stuck "waiting for the person's yes on the deploy"`)
	}
	need := strings.ToLower(strings.TrimSpace(r.Need))
	if need == "" {
		need = msg.NeedPerson
	}
	if !needWord.MatchString(need) {
		return res, outbox.Usagef("--need is person, peer, or one tool or capability word (codex, gpu): run it again with one, like `--need person`")
	}
	body := msg.StuckPrefix + reason
	var thread msg.Message
	var side *outbox.Plan
	if r.Re != "" {
		ms, err := c.Thread(r.Re)
		if err != nil {
			return res, err
		}
		for _, m := range ms {
			if m.ID == r.Re {
				thread = m
			}
		}
		if thread.ID == "" {
			return res, outbox.NotFoundf("message %s was not found on the nodes (stuck --re takes the full 10-character id): check it with `aivoicebus inbox --all`", r.Re)
		}
		if res.Side = otherSide(r.Me, thread, ms); res.Side != "" {
			p, err := outbox.Prepare(c, outbox.Request{To: res.Side, Re: thread.ID, Thread: thread.Thread, Kind: msg.KindProgress, Body: body, TargetSaid: true}, warn)
			if err != nil {
				return res, err // a credential in the reason, most likely: nothing went anywhere
			}
			side = &p
		} else {
			res.Notes = append(res.Notes, "nobody else is in that thread yet, so only the "+map[bool]string{true: "person", false: "helper"}[need == msg.NeedPerson]+" hears it")
		}
	}
	// The helper is picked before anything goes out, so that nothing half-sent is left when the
	// target is wrong.
	if need != msg.NeedPerson {
		var picked []msg.RosterEntry
		var err error
		if need == msg.NeedPeer {
			picked, err = Listening(c, "all", r.Me, res.Side)
		} else {
			var byTool, byCan []msg.RosterEntry
			byTool, err = Listening(c, "*/"+need, r.Me, res.Side)
			if err == nil {
				byCan, err = Listening(c, "@can="+need, r.Me, res.Side)
			}
			picked = append(byTool, byCan...)
		}
		if err != nil && !client.IsUnreachable(err) {
			return res, err
		}
		if len(picked) == 0 {
			what := "nobody"
			if need != msg.NeedPeer {
				what = "no session with " + need
			}
			res.Notes = append(res.Notes, what+" is listening right now: told the person instead")
			need = msg.NeedPerson
		} else {
			res.Peer = picked[rand.IntN(len(picked))].Me
		}
	}
	if side != nil {
		out, err := side.Send(c)
		if err != nil {
			return res, fmt.Errorf("telling %s: %w", msg.SafeName(res.Side), err)
		}
		res.SideID = out.ID
	}
	where := ""
	if thread.ID != "" {
		where = fmt.Sprintf(" (on %s %s, thread %s)", KindWord(thread.Kind), thread.ID, thread.Thread)
	}
	if res.Peer != "" {
		p, err := outbox.Prepare(c, outbox.Request{To: res.Peer, Kind: msg.KindTask, TargetSaid: true,
			Body: fmt.Sprintf("%s%s is blocked%s and asks for help: %s", msg.StuckPrefix, msg.SafeName(r.Me), where, reason)}, warn)
		if err == nil {
			var out msg.Message
			if out, err = p.Send(c); err == nil {
				res.PeerID = out.ID
				return res, nil
			}
		}
		res.Notes = append(res.Notes, fmt.Sprintf("asking %s failed (%v): told the person instead", msg.SafeName(res.Peer), err))
		res.Peer = ""
	}
	// The person: the whole reason in their inbox (aivoicebus answer lists it and answers it), and
	// a short summary on the phone, which names who and what only.
	p, err := outbox.Prepare(c, outbox.Request{To: msg.HumanOf(r.Me), Body: body + where, Prio: msg.PrioUrgent, Queue: true, TargetSaid: true}, warn)
	if err != nil {
		return res, err
	}
	out, err := p.Send(c)
	if err != nil {
		return res, fmt.Errorf("telling the person: %w", err)
	}
	res.PersonID = out.ID
	if r.NotifyURL != "" {
		on := ""
		if thread.ID != "" {
			on = " on " + KindWord(thread.Kind) + " " + thread.ID
		}
		text := fmt.Sprintf("%s is stuck%s: %s (answer: aivoicebus answer %s)", msg.SafeName(r.Me), on, Summary(reason, msg.StuckSummaryRunes), out.ID)
		if err := NotifyActions(r.NotifyURL, "aivoicebus: an agent is stuck", text, ""); err != nil {
			res.PhoneUnreachable = err.Error()
		} else {
			res.Phoned = true
		}
	}
	return res, nil
}
