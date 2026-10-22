package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// ownerCmd: who answers for a subject (msg/owner.go). `owner set` records that this machine does, `owner get`
// says who a message to owner:SUBJECT would reach, and `send --to owner:SUBJECT`, `task owner:SUBJECT` and
// `ask owner:SUBJECT` use it.
func ownerCmd(args []string, s Streams) error {
	const usageText = "usage: owner get SUBJECT [--json]   |   owner list [GLOB...] [--json]   |   owner set SUBJECT [--machine M] [--role R] [--needs repo:o/x,...] [--person NAME] [--ttl 30d]   |   owner rm SUBJECT   |   owner renew [GLOB...] [--ttl 30d]   |   owner trust [HOST/AGENT-PATTERN]   |   owner untrust HOST/AGENT-PATTERN   (SUBJECT: 0mcp:vm701, service:gateway, alias:0mcp_docker_build, 0mcp:* for a family)"
	usage := outbox.Usagef("%s", usageText)
	if len(args) == 0 {
		return usage
	}
	if wantsHelp(args) {
		fmt.Fprintln(s.Out, usageText)
		return nil
	}
	switch args[0] {
	case "trust":
		return ownerTrust(args[1:], s)
	case "untrust":
		return ownerUntrust(args[1:], s)
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	switch args[0] {
	case "get":
		return ownerGet(c, args[1:], s)
	case "list":
		return ownerList(c, args[1:], s)
	case "set":
		return ownerSet(c, args[1:], s)
	case "rm":
		return ownerRm(c, args[1:], s)
	case "renew":
		return ownerRenew(c, args[1:], s)
	}
	return usage
}

func ownerSubjectArg(words []string, verb string) (string, error) {
	if len(words) != 1 {
		return "", outbox.Usagef("owner %s takes one SUBJECT, such as 0mcp:vm701 or service:gateway", verb)
	}
	subject := strings.ToLower(words[0])
	subject = strings.TrimPrefix(subject, msg.OwnerPrefix)
	return subject, nil
}

// ownerView is one record as `owner get --json` and `owner list --json` print it.
type ownerView struct {
	Subject string    `json:"subject"`
	Machine string    `json:"machine,omitempty"`
	Needs   []string  `json:"needs,omitempty"`
	Person  string    `json:"person,omitempty"`
	By      string    `json:"by"`
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Until   time.Time `json:"until"`
	Check   string    `json:"check"` // signed, unsigned, bad, changed, unchecked or does-not-parse
	Problem string    `json:"problem,omitempty"`
}

// ownerAnswer is `owner get --json`: the owner, how far to trust it, and where a message would go now.
type ownerAnswer struct {
	ownerView
	Via     string         `json:"via,omitempty"` // the declared family the subject matched, when it was not exact
	Pinned  bool           `json:"pinned"`
	To      string         `json:"to,omitempty"`
	Person  bool           `json:"person,omitempty"` // To is the person on the machine
	Why     string         `json:"why,omitempty"`
	NotSent string         `json:"not_sent,omitempty"` // why a message would not go
	NotUsed []ownerNotUsed `json:"not_used,omitempty"`
}

type ownerNotUsed struct {
	ownerView
	Why string `json:"why"`
}

func viewOf(r msg.OwnerRecord) ownerView {
	check := r.Verified
	switch {
	case r.Problem != "":
		check = "does-not-parse"
	case check == "":
		check = "unchecked"
	}
	return ownerView{Subject: r.Subject, Machine: r.Machine, Needs: r.Needs, Person: r.Person, By: r.By, ID: r.ID, At: r.At, Until: r.Until, Check: check, Problem: r.Problem}
}

func ownerGet(c *client.Client, args []string, s Streams) error {
	fs := newFlagSet("owner get")
	asJSON := fs.Bool("json", false, "print the answer as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	words, err := trailingFlags(fs, s)
	if err != nil {
		return err
	}
	subject, err := ownerSubjectArg(words, "get")
	if err != nil {
		return err
	}
	now := time.Now()
	res, err := outbox.ResolveOwner(c, subject)
	for _, ig := range res.Ignored {
		fmt.Fprintf(s.Err, "  not used: record %s by %s: %s\n", view.Name(ig.Record.ID, msg.MaxRef), view.Name(ig.Record.By, msg.MaxIdentity), ig.Why)
	}
	if errors.Is(err, msg.ErrNoOwner) {
		return outbox.NotFoundf("%v", err)
	}
	if err != nil {
		return err
	}
	// what a message would do now: the same route send, task and ask use, read-only
	route, rerr := tasks.RouteOwner(c, subject, identity.Me(), tasks.OwnerRouteOpts{})
	if *asJSON {
		out := ownerAnswer{ownerView: viewOf(res.Record), Pinned: res.Pinned, To: route.To, Person: route.Person, Why: route.Why}
		out.Subject = subject
		if res.Record.Subject != subject {
			out.Via = res.Record.Subject
		}
		if rerr != nil {
			out.NotSent = rerr.Error()
		}
		for _, ig := range res.Ignored {
			out.NotUsed = append(out.NotUsed, ownerNotUsed{viewOf(ig.Record), ig.Why})
		}
		return writeJSON(s.Out, out)
	}
	fmt.Fprintln(s.Out, res.Line(now))
	switch {
	case rerr != nil:
		fmt.Fprintf(s.Out, "  a message would not be sent: %v\n", rerr)
	case route.Person:
		fmt.Fprintf(s.Out, "  a message would go to the person on %s: %s\n", msg.SafeName(route.Machine), route.Why)
	default:
		fmt.Fprintf(s.Out, "  a message would go to %s: %s\n", view.Name(route.To, msg.MaxIdentity), route.Why)
	}
	return nil
}

func ownerList(c *client.Client, args []string, s Streams) error {
	fs := newFlagSet("owner list")
	asJSON := fs.Bool("json", false, "print the records as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	globs, err := trailingFlags(fs, s)
	if err != nil {
		return err
	}
	for i := range globs {
		globs[i] = strings.TrimPrefix(strings.ToLower(globs[i]), msg.OwnerPrefix)
	}
	recs, err := outbox.OwnerRecords(c, func(subject string) bool {
		return len(globs) == 0 || slices.ContainsFunc(globs, func(g string) bool { return msg.MatchKey(g, subject) })
	})
	if err != nil {
		return err
	}
	if *asJSON {
		out := []ownerView{}
		for _, r := range recs {
			out = append(out, viewOf(r))
		}
		return writeJSON(s.Out, out)
	}
	if len(recs) == 0 {
		fmt.Fprintln(s.Out, "(no owner records; the owner of a subject records itself with `aivoicebus owner set SUBJECT`)")
		return nil
	}
	now := time.Now()
	fmt.Fprintf(s.Out, "%-30s %-24s %-22s %-10s %-26s %-8s %-8s %s\n", "SUBJECT", "MACHINE", "NEEDS", "PERSON", "DECLARED BY", "AGE", "EXPIRES", "CHECK")
	for _, r := range recs {
		v := viewOf(r)
		if r.Problem != "" {
			fmt.Fprintf(s.Out, "%-30s (%s)  by %s  [%s]\n", view.OneLine(r.Subject, 30), view.OneLine(r.Problem, 80), view.Name(r.By, msg.MaxIdentity), v.Check)
			continue
		}
		fmt.Fprintf(s.Out, "%-30s %-24s %-22s %-10s %-26s %-8s %-8s %s\n", view.OneLine(r.Subject, 30), view.OneLine(r.Machine, 24), view.OneLine(strings.Join(r.Needs, ","), 22), view.OneLine(r.Person, 10),
			view.Name(r.By, 26), msg.SpanWords(now.Sub(r.At)), msg.SpanWords(r.Until.Sub(now)), v.Check)
	}
	return nil
}

func ownerSet(c *client.Client, args []string, s Streams) error {
	fs := newFlagSet("owner set")
	machine := fs.String("machine", "", "the owner's machine (default: this one); a record for another machine counts only for senders that trust you (owner trust)")
	needs := fs.String("needs", "", "which sessions there answer: repo:owner/name, branch:, tool:, model:, role: or a declared word (the --needs words); default: any session there")
	role := fs.String("role", "", "shorthand for --needs role:ROLE: the session there that claims that role (AIVOICEBUS_ROLE)")
	person := fs.String("person", "", "a label for people, one word (not used for routing: the person is reached by machine)")
	ttl := fs.String("ttl", "30d", "how long the record lasts, at most 30d; renew it with owner renew")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	words, err := trailingFlags(fs, s)
	if err != nil {
		return err
	}
	subject, err := ownerSubjectArg(words, "set")
	if err != nil {
		return err
	}
	if err := msg.ValidOwnerSubject(subject); err != nil {
		return outbox.Usagef("owner set %s: %v: `aivoicebus owner set -h` shows the form", msg.SafeName(subject), err)
	}
	me := identity.Me()
	f := msg.OwnerFields{Machine: strings.ToLower(*machine), Person: *person}
	if f.Machine == "" {
		f.Machine = msg.HostOf(me)
	}
	if *needs != "" {
		if f.Needs, err = msg.ParseNeeds(*needs); err != nil {
			return outbox.Usagef("%v", err)
		}
	}
	if *role != "" {
		if !msg.ValidRoleName(strings.ToLower(*role)) {
			return outbox.Usagef("--role %q is not a role name (lower-case letters, digits, - and _): `aivoicebus role` lists the pinned ones", msg.SafeName(*role))
		}
		if n := "role:" + strings.ToLower(*role); !slices.Contains(f.Needs, n) {
			f.Needs = append(f.Needs, n)
		}
	}
	if err := f.Check(); err != nil {
		return outbox.Usagef("owner set %s: %v: `aivoicebus owner set -h` shows the flags", msg.SafeName(subject), err)
	}
	d, err := msg.ParseTTL(*ttl)
	if err != nil {
		return outbox.Usagef("--ttl: %v", err)
	}
	out, err := sendOwnerRecord(c, me, subject, f, *ttl, s.Err)
	if err != nil {
		return err
	}
	n, err := retireOwnerRecords(c, me, subject, out.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "owner:%s -> %s%s recorded until %s (lease %s)", msg.SafeName(subject), msg.SafeName(f.Machine), needsSuffix(f), view.When(out.At.Add(d), time.Now()), out.ID)
	if n > 0 {
		fmt.Fprintf(s.Out, "; it replaces %d earlier record(s) of this machine", n)
	}
	fmt.Fprintln(s.Out)
	if f.Machine != msg.HostOf(me) {
		fmt.Fprintf(s.Out, "this names another machine as owner: senders use it only if they trust you (`%s`); %s can say so itself with `aivoicebus owner set %s`\n",
			msg.OwnerTrustHint(me), msg.SafeName(f.Machine), msg.SafeName(subject))
	}
	fmt.Fprintln(s.Out, "it expires by itself: `aivoicebus owner renew` extends this machine's records, `aivoicebus owner rm "+shellWord(msg.SafeName(subject))+"` removes it")
	return nil
}

func needsSuffix(f msg.OwnerFields) string {
	if len(f.Needs) == 0 {
		return ""
	}
	return " needs " + msg.SafeName(strings.Join(f.Needs, ","))
}

// sendOwnerRecord writes one record as the lease it is.
func sendOwnerRecord(c *client.Client, me, subject string, f msg.OwnerFields, ttl string, warn io.Writer) (msg.Message, error) {
	plan, err := outbox.Prepare(c, outbox.Request{To: me, Body: msg.LeaseBody(msg.OwnerKey(subject), f.Note()), Kind: msg.KindLease, TTL: ttl}, warn)
	if err != nil {
		return msg.Message{}, err
	}
	return plan.Send(c)
}

// retireOwnerRecords withdraws the live records about exactly subject that this machine declared, except
// keepID: any session of the machine may (retraction is by machine), so a restarted session can take over.
func retireOwnerRecords(c *client.Client, me, subject, keepID string) (int, error) {
	live, err := c.Leases()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range live {
		rec, ok := msg.OwnerOf(l)
		if !ok || rec.Subject != subject || l.ID == keepID || msg.HostOf(l.By) != msg.HostOf(me) {
			continue
		}
		if _, err := c.Retract(l.ID, me, false, false); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func ownerRm(c *client.Client, args []string, s Streams) error {
	fs := newFlagSet("owner rm")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	words, err := trailingFlags(fs, s)
	if err != nil {
		return err
	}
	subject, err := ownerSubjectArg(words, "rm")
	if err != nil {
		return err
	}
	n, err := retireOwnerRecords(c, identity.Me(), subject, "")
	if err != nil {
		return err
	}
	if n == 0 {
		return outbox.NotFoundf("this machine declared no owner record for %s: `aivoicebus owner list` shows what is recorded and by whom", msg.SafeName(subject))
	}
	fmt.Fprintf(s.Out, "removed %d record(s) for owner:%s\n", n, msg.SafeName(subject))
	return nil
}

func ownerRenew(c *client.Client, args []string, s Streams) error {
	fs := newFlagSet("owner renew")
	ttl := fs.String("ttl", "30d", "how long the renewed records last, at most 30d")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	globs, err := trailingFlags(fs, s)
	if err != nil {
		return err
	}
	for i := range globs {
		globs[i] = strings.TrimPrefix(strings.ToLower(globs[i]), msg.OwnerPrefix)
	}
	d, err := msg.ParseTTL(*ttl)
	if err != nil {
		return outbox.Usagef("--ttl: %v", err)
	}
	me := identity.Me()
	live, err := c.Leases()
	if err != nil {
		return err
	}
	renewed := 0
	for _, l := range live {
		rec, ok := msg.OwnerOf(l)
		if !ok || rec.Problem != "" || msg.HostOf(l.By) != msg.HostOf(me) {
			continue
		}
		if len(globs) > 0 && !slices.ContainsFunc(globs, func(g string) bool { return msg.MatchKey(g, rec.Subject) }) {
			continue
		}
		out, err := sendOwnerRecord(c, me, rec.Subject, msg.OwnerFields{Machine: rec.Machine, Needs: rec.Needs, Person: rec.Person}, *ttl, s.Err)
		if err != nil {
			return err
		}
		if _, err := retireOwnerRecords(c, me, rec.Subject, out.ID); err != nil {
			return err
		}
		fmt.Fprintf(s.Out, "owner:%s renewed until %s\n", msg.SafeName(rec.Subject), view.When(out.At.Add(d), time.Now()))
		renewed++
	}
	if renewed == 0 {
		return outbox.NotFoundf("this machine declared no owner record to renew: `aivoicebus owner list` shows what is recorded and by whom, and `aivoicebus owner set SUBJECT` records one")
	}
	return nil
}

func ownerTrust(args []string, s Streams) error {
	if len(args) == 0 {
		text, err := config.ReadOwnerTrust()
		if err != nil {
			return err
		}
		pins := msg.ParseOwnerPins(text)
		for _, p := range pins {
			fmt.Fprintln(s.Out, msg.SafeName(p))
		}
		if len(pins) == 0 {
			fmt.Fprintln(s.Out, "(no declarers trusted; `aivoicebus owner trust 'host/claude.*'` trusts the sessions that match)")
		}
		return nil
	}
	if len(args) != 1 {
		return outbox.Usagef("usage: owner trust [HOST/AGENT-PATTERN]   (e.g. owner trust 'imac-*/claude.*')")
	}
	pat := strings.ToLower(args[0])
	if err := msg.ValidRolePin(pat); err != nil {
		return outbox.Usagef("%v", err)
	}
	err := config.UpdateSmall(config.OwnerTrustFile, func(old string) (string, error) {
		if slices.Contains(msg.ParseOwnerPins(old), pat) {
			return old, nil
		}
		if old != "" && !strings.HasSuffix(old, "\n") {
			old += "\n"
		}
		return old + pat + "\n", nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "owner records declared by %s are now trusted: they outrank other claims to the same subject, and are the only ones used for an urgent message, a file or a log link\n", msg.SafeName(pat))
	return nil
}

func ownerUntrust(args []string, s Streams) error {
	if len(args) != 1 {
		return outbox.Usagef("usage: owner untrust HOST/AGENT-PATTERN   (`aivoicebus owner trust` lists them)")
	}
	pat := strings.ToLower(args[0])
	removed := false
	err := config.UpdateSmall(config.OwnerTrustFile, func(old string) (string, error) {
		var keep []string
		for _, line := range strings.Split(old, "\n") {
			body, _, _ := strings.Cut(line, "#")
			if strings.ToLower(strings.TrimSpace(body)) == pat {
				removed = true
				continue
			}
			keep = append(keep, line)
		}
		return strings.Join(keep, "\n"), nil
	})
	if err != nil {
		return err
	}
	if !removed {
		return outbox.NotFoundf("%s is not trusted: `aivoicebus owner trust` lists the trusted declarers", msg.SafeName(pat))
	}
	fmt.Fprintf(s.Out, "%s is no longer trusted\n", msg.SafeName(pat))
	return nil
}
