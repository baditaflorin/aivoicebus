package mcp

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// confirmOwnerNote is what a tool says when an owner record nobody pinned would route the message: anyone
// with the token can write one, so the agent looks at who it resolved to first (as for a capability).
const confirmOwnerNote = "nothing was sent: this owner record is not pinned, and anyone with the token can write one. Check who it resolved to above, then call again with confirm_caps true."

// ownerRoute resolves a target of the form owner:SUBJECT for a tool call. isOwner is false for any other
// target. text says who the record came to and where the message goes; err explains a lookup that did not
// come to one owner (the records that were not used come first).
func ownerRoute(c *client.Client, to string, o tasks.OwnerRouteOpts) (r tasks.OwnerRoute, text string, isOwner bool, err error) {
	subject, isOwner := msg.OwnerSubjectOf(to)
	if !isOwner {
		return r, "", false, nil
	}
	r, err = tasks.RouteOwner(c, subject, identity.Me(), o)
	var b strings.Builder
	for _, ig := range r.Res.Ignored {
		fmt.Fprintf(&b, "not used: record %s by %s: %s\n", view.Name(ig.Record.ID, msg.MaxRef), view.Name(ig.Record.By, msg.MaxIdentity), ig.Why)
	}
	if err != nil {
		return r, "", true, errors.New(b.String() + err.Error())
	}
	fmt.Fprintf(&b, "%s\n-> %s: %s\n", r.Res.Line(time.Now()), view.Name(r.To, msg.MaxIdentity), r.Why)
	return r, b.String(), true, nil
}

// ownerLeft is the way on when nobody on an owner's machine could be asked: a task, which waits for the next
// session there and goes to the person when none is live. "" for any other target.
func ownerLeft(ownerText, machine string) string {
	if ownerText == "" {
		return ""
	}
	return " To leave it for the owner, call aivoicebus_task with the same owner:SUBJECT target (the owner's machine, " + view.OneLine(machine, 40) + ", is named above)."
}
