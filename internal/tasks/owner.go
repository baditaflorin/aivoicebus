package tasks

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// OwnerRoute is where a message for the owner of a subject goes (owner:SUBJECT, see msg/owner.go). It
// only decides; the message is then sent, asked or tasked as to any other target.
type OwnerRoute struct {
	Res     msg.OwnerResolution
	Machine string   // the owner's machine
	Needs   []string // which of its sessions answer (the record's needs)
	// To is a session there that has the needs, the machine when the record names none, or
	// MACHINE/human when nobody there is live (Person). Empty for a route made only to look.
	To     string
	Person bool   // To is the person on the machine: the message goes as a task marked [for the person]
	Why    string // what the pick came to, in one line
}

// OwnerDelivery says where a message for the owner in res goes, given who is on the roster: a live
// session that has the record's needs (PickNeeds, listening first), else, when the record names
// none, the machine (every session there is shown it, and the first claim keeps it), else the person on
// the machine. Live is what PickNeeds says: listening now, or seen within AskWithin.
func OwnerDelivery(roster []msg.RosterEntry, res msg.OwnerResolution, me string, now time.Time) OwnerRoute {
	rec := res.Record
	r := OwnerRoute{Res: res, Machine: rec.Machine, Needs: rec.Needs}
	if len(rec.Needs) > 0 {
		p, err := PickNeeds(roster, rec.Machine, rec.Needs, PickOpts{Me: me, Now: now})
		if err == nil {
			r.To, r.Why = p.To, p.Why
			return r
		}
		return r.person(fmt.Sprintf("no live session on %s has %s", msg.SafeName(rec.Machine), msg.SafeName(strings.Join(rec.Needs, ","))))
	}
	live := 0
	for _, e := range roster {
		if e.Me != me && msg.HostOf(e.Me) == rec.Machine && (e.Listening || now.Sub(e.Seen) <= AskWithin) {
			live++
		}
	}
	if live == 0 {
		return r.person(fmt.Sprintf("no session on %s is live (seen within %s)", msg.SafeName(rec.Machine), msg.SpanWords(AskWithin)))
	}
	r.To, r.Why = rec.Machine, fmt.Sprintf("%d live session(s) there are shown it, and the first to claim it keeps it", live)
	return r
}

func (r OwnerRoute) person(why string) OwnerRoute {
	r.To, r.Person = r.Machine+"/human", true
	r.Why = why + ": it goes as a task for the person there, which no agent takes"
	return r
}

// OwnerRouteOpts says what the message needs of the record.
type OwnerRouteOpts struct {
	Urgent, Attach bool // an urgent message, a file or a log link: only a record from a declarer the sender trusts
	Person         bool // to the person on the owner's machine, whoever is live (task owner:S --person)
}

// RouteOwner looks the owner of subject up on the nodes and says where a message goes: the record must
// check out (outbox.ResolveOwner), suit the message (Allows) and name a machine somebody was heard on
// lately. The route is filled in as far as it got when there is an error.
func RouteOwner(c *client.Client, subject, me string, o OwnerRouteOpts) (OwnerRoute, error) {
	res, err := outbox.ResolveOwner(c, subject)
	r := OwnerRoute{Res: res}
	if err != nil {
		return r, err
	}
	r.Machine, r.Needs = res.Record.Machine, res.Record.Needs
	if err := res.Allows(o.Urgent, o.Attach); err != nil {
		return r, err
	}
	all, err := c.RosterAll()
	switch {
	case client.IsNotFound(err):
		return r, errors.New("this node does not report who is around, so an owner cannot be reached: send to the owner's machine by name (`aivoicebus owner get " + msg.SafeName(subject) + "` shows it), or ask the user to update the nodes")
	case err != nil:
		return r, fmt.Errorf("the nodes could not say who is around (%v): try again", err)
	}
	now := time.Now()
	if err := res.MachineProblem(msg.OwnerHeardAt(all, res.Record.Machine), now); err != nil {
		return r, err
	}
	var roster []msg.RosterEntry
	for _, e := range all {
		if !msg.IsProbe(e.Me) {
			roster = append(roster, e)
		}
	}
	r = OwnerDelivery(roster, res, me, now)
	if o.Person {
		r.To, r.Person = r.Machine+"/human", true
		r.Why = "you asked for the person on " + msg.SafeName(r.Machine) + ": it goes as a task for the person there, which no agent takes"
	}
	return r, nil
}
