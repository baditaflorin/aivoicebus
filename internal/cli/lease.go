package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// leaseCmd: `aivoicebus lease take KEY`, `lease release KEY`, `lease list`, `lease run KEY -- CMD`. A
// lease tells the other agents this session is working on something (a file, a branch, a deploy)
// until a time.
func leaseCmd(args []string, s Streams) error {
	usage := outbox.Usagef("usage: lease take KEY [--ttl 30m] [--note TEXT]   |   lease release KEY   |   lease list [--json] [GLOB...]   |   lease run KEY [--ttl 30m] [--note TEXT] [--override-hold WHY] -- CMD [ARGS...]   (KEY: e.g. hub:server.go, repo/branch, deploy:prod; GLOB: * any run, ? one character, e.g. 'load:prod/*')")
	if len(args) == 0 {
		return usage
	}
	c, err := client.FromEnv()
	if err != nil && args[0] != "run" { // run says what it does about no node (refuse, or --override-hold)
		return err
	}
	me := identity.Me()
	switch args[0] {
	case "list":
		fs := newFlagSet("lease list")
		asJSON := fs.Bool("json", false, "print the holders as JSON")
		var globs []string // flags may follow a glob too
		for rest := args[1:]; ; {
			if err := parseFlags(fs, rest, s); err != nil {
				return err
			}
			if fs.NArg() == 0 {
				break
			}
			globs = append(globs, fs.Arg(0))
			rest = fs.Args()[1:]
		}
		live, err := c.Leases()
		if err != nil {
			return err
		}
		hs := matchingHolders(msg.Holders(live), globs)
		if *asJSON {
			return writeJSON(s.Out, hs)
		}
		if len(hs) == 0 && len(globs) > 0 {
			fmt.Fprintf(s.Out, "(no leases held that match %s)\n", strings.Join(globs, " "))
			return nil
		}
		if len(hs) == 0 {
			fmt.Fprintln(s.Out, "(no leases held)")
			return nil
		}
		for _, h := range hs {
			fmt.Fprintln(s.Out, leaseLine(h, me))
		}
		return nil
	case "take":
		fs := newFlagSet("lease take")
		ttl := fs.String("ttl", "30m", "how long the lease lasts (take it again to extend it)")
		note := fs.String("note", "", "what you are doing, for the others")
		settle := fs.Duration("settle", time.Second, "how long to wait for a lease taken elsewhere at the same moment")
		if err := parseFlags(fs, args[1:], s); err != nil {
			return err
		}
		if fs.NArg() < 1 || !msg.ValidLeaseKey(fs.Arg(0)) {
			return outbox.Usagef("lease take KEY: one key of 1 to 120 letters, digits and . _ : / @ + -")
		}
		key := fs.Arg(0)
		if err := parseFlags(fs, fs.Args()[1:], s); err != nil { // flags may follow the key too
			return err
		}
		if fs.NArg() != 0 {
			return outbox.Usagef("lease take KEY [--ttl 30m] [--note TEXT]: one key, and the note in --note")
		}
		if h, held := holderOf(c, key); held && h.By != me {
			return fmt.Errorf("%s", leaseLine(h, me)) // nextstep: leaseLine says when it frees up
		}
		out, h, err := sendLease(c, me, key, *note, *ttl, *settle, s)
		if err != nil {
			return err
		}
		if h != nil {
			return fmt.Errorf("%s (taken at the same moment; yours is withdrawn): ask its holder, or try again once it is released", leaseLine(*h, me))
		}
		fmt.Fprintf(s.Out, "you hold %s until %s (lease %s); release it with: aivoicebus lease release %s\n", key, view.When(out.At.Add(ttlOf(*ttl)), time.Now()), out.ID, key)
		return nil
	case "run":
		return leaseRun(c, err, me, args[1:], s)
	case "release":
		if len(args) != 2 || !msg.ValidLeaseKey(args[1]) {
			return usage
		}
		n, err := releaseLeases(c, me, args[1])
		if err != nil {
			return err
		}
		if n == 0 {
			return outbox.NotFoundf("this session holds no lease on %s: `aivoicebus lease list` shows who holds what", args[1])
		}
		fmt.Fprintf(s.Out, "released %s\n", args[1])
		return nil
	}
	return usage
}

func holderOf(c *client.Client, key string) (msg.Lease, bool) {
	live, err := c.Leases()
	if err != nil {
		return msg.Lease{}, false
	}
	for _, h := range msg.Holders(live) {
		if h.Key == key {
			return h, true
		}
	}
	return msg.Lease{}, false
}

func leaseLine(h msg.Lease, me string) string {
	who := view.Name(h.By, msg.MaxIdentity)
	if h.By == me {
		who = "you"
	}
	line := fmt.Sprintf("%s is held by %s until %s", h.Key, who, view.When(h.Until, time.Now()))
	if h.Note != "" {
		line += ": " + view.OneLine(h.Note, 120)
	}
	return line
}

func ttlOf(s string) time.Duration {
	d, err := msg.ParseTTL(s)
	if err != nil {
		return 0
	}
	return d
}

// sendLease writes this session's lease on key, waits settle for one taken elsewhere at the same
// moment and, when that one is earlier, withdraws its own and returns it as the holder. The
// caller has already looked for a holder it must not take from.
func sendLease(c *client.Client, me, key, note, ttl string, settle time.Duration, s Streams) (msg.Message, *msg.Lease, error) {
	plan, err := outbox.Prepare(c, outbox.Request{To: me, Body: msg.LeaseBody(key, note), Kind: msg.KindLease, TTL: ttl}, s.Err)
	if err != nil {
		return msg.Message{}, nil, err
	}
	out, err := plan.Send(c)
	if err != nil {
		return msg.Message{}, nil, err
	}
	time.Sleep(settle)
	if h, held := holderOf(c, key); held && h.By != me {
		_, _ = c.Retract(out.ID, me, false, false) // someone else was first: give ours back
		return out, &h, nil
	}
	return out, nil, nil
}

// releaseLeases withdraws every live lease of me on key and says how many there were.
func releaseLeases(c *client.Client, me, key string) (int, error) {
	live, err := c.Leases()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range live {
		if l.Key == key && l.By == me {
			if _, err := c.Retract(l.ID, me, false, false); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// matchingHolders keeps the holders whose key matches one of globs (see msg.MatchKey); with no
// glob it keeps them all.
func matchingHolders(hs []msg.Lease, globs []string) []msg.Lease {
	if len(globs) == 0 {
		return hs
	}
	out := []msg.Lease{}
	for _, h := range hs {
		for _, g := range globs {
			if msg.MatchKey(g, h.Key) {
				out = append(out, h)
				break
			}
		}
	}
	return out
}
