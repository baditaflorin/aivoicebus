package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// ownerTarget turns a target of the form owner:SUBJECT into where the message goes (tasks.RouteOwner), before
// anything is made of it: a live session on the owner's machine that has the record's needs, the machine,
// or the person there as a task. It says on stderr who the record came to and what it did, and it changes
// *to, *kind and *body only when it turns the message into a task for the person.
func ownerTarget(c *client.Client, to, kind, body *string, prio string, attach bool, s Streams) error {
	subject, ok := msg.OwnerSubjectOf(*to)
	if !ok {
		return nil
	}
	r, err := tasks.RouteOwner(c, subject, identity.Me(), tasks.OwnerRouteOpts{Urgent: strings.EqualFold(prio, msg.PrioUrgent), Attach: attach})
	return applyOwnerRoute(r, err, to, kind, body, attach, s)
}

// reportOwnerRoute says on stderr what a lookup came to: the records that were not used, then who the
// owner is and where a message goes. It returns the lookup's error, as a not-found for no owner.
func reportOwnerRoute(r tasks.OwnerRoute, err error, s Streams) error {
	for _, ig := range r.Res.Ignored {
		fmt.Fprintf(s.Err, "  not used: record %s by %s: %s\n", view.Name(ig.Record.ID, msg.MaxRef), view.Name(ig.Record.By, msg.MaxIdentity), ig.Why)
	}
	if errors.Is(err, msg.ErrNoOwner) {
		return outbox.NotFoundf("%v", err)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(s.Err, "  "+r.Res.Line(time.Now()))
	fmt.Fprintf(s.Err, "  -> %s: %s\n", view.Name(r.To, msg.MaxIdentity), r.Why)
	return nil
}

// applyOwnerRoute is ownerTarget's second half, for callers that made the route themselves.
func applyOwnerRoute(r tasks.OwnerRoute, err error, to, kind, body *string, attach bool, s Streams) error {
	if err := reportOwnerRoute(r, err, s); err != nil {
		return err
	}
	*to = r.To
	if r.Person {
		switch {
		case attach:
			return outbox.Usagef("nobody on %s is live, and a file or a log link lives minutes, so it cannot wait in a task for the person there: send it when `aivoicebus ping %s` shows a session, or send the text alone", msg.SafeName(r.Machine), msg.SafeName(r.Machine))
		case *kind != "" && *kind != msg.KindTask:
			return outbox.Usagef("nobody on %s is live, so this goes as a task for the person there, and it is a %s: run it again without --kind, or send it to a session by name", msg.SafeName(r.Machine), view.OneLine(*kind, 20))
		}
		*kind = msg.KindTask
		*body = msg.PersonTag + " " + *body
	}
	return nil
}
