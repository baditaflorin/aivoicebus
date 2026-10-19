package cli

import (
	"fmt"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// stuckCmd: an agent says it is blocked, instead of going silent or looping. With --re the
// thread's other side hears it (as progress: its wait shows it); by default the person is told
// too (the inbox `aivoicebus answer` reads, and a short summary on the phone); --need peer or
// --need WORD asks a session that is listening right now instead.
func stuckCmd(args []string, s Streams) error {
	fs := newFlagSet("stuck")
	re := fs.String("re", "", "the task or message you are stuck on (its full id): its other side is told")
	need := fs.String("need", msg.NeedPerson, "who can unblock you: person (this machine's person), peer (any session listening now), or a tool or capability (codex, gpu): one listening with it")
	asJSON := fs.Bool("json", false, "print where it went as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	words, err := trailingFlags(fs, s)
	if err != nil {
		return err
	}
	reason := strings.TrimSpace(strings.Join(words, " "))
	if reason == "" {
		return outbox.Usagef(`usage: stuck "what blocks you" [--re ID] [--need person|peer|TOOL|CAPABILITY] [--json]`)
	}
	if *re != "" && !msg.ValidID(*re) {
		return outbox.Usagef("--re takes the full 10-character id")
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	c.Labels = identity.Labels("")
	r, err := tasks.SendStuck(c, tasks.StuckReq{Me: identity.Me(), Reason: reason, Re: *re, Need: *need, NotifyURL: notifyURL()}, s.Err)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(s.Out, r)
	}
	if r.SideID != "" {
		fmt.Fprintf(s.Out, "told %s (%s): their wait shows it\n", view.Name(r.Side, msg.MaxIdentity), r.SideID)
	}
	if r.PeerID != "" {
		fmt.Fprintf(s.Out, "asked %s for help (task %s, picked from the sessions listening now)\n", view.Name(r.Peer, msg.MaxIdentity), r.PeerID)
	}
	if r.PersonID != "" {
		phone := ""
		switch {
		case r.Phoned:
			phone = ", and a short summary went to the phone"
		case r.PhoneUnreachable != "":
			phone = " (the phone could not be told: " + r.PhoneUnreachable + ")"
		}
		fmt.Fprintf(s.Out, "told the person (%s; they read it with: aivoicebus answer)%s\n", r.PersonID, phone)
	}
	for _, n := range r.Notes {
		fmt.Fprintln(s.Err, "  "+n)
	}
	thread := ""
	switch {
	case r.PeerID != "":
		thread = r.PeerID
	case r.PersonID != "":
		thread = r.PersonID
	}
	if thread != "" {
		fmt.Fprintf(s.Out, "[aivoicebus] Do other work if you can; otherwise wait for help: aivoicebus wait --thread %s --for 30m (as a background command). Do not loop on the same step.\n", thread)
	}
	return nil
}
