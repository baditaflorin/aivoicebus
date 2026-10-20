package cli

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// leaseEnv names, for the command `lease run` starts, the keys it runs under (comma separated),
// so a script that guards itself with a lease can tell it is already inside one and not take it twice.
const leaseEnv = "AIVOICEBUS_LEASES"

// minOverrideWhy is the shortest reason --override-hold accepts: a person's name and a sentence,
// not "x" (the same bar AIVOICEBUS_SKIP_COMPAT sets for a waiver).
const minOverrideWhy = 10

// leaseRenewEvery is how often a run extends its lease while the command goes on: a third of the
// ttl, so the ttl bounds how long a run that died without releasing keeps the key, not how long
// the command may take. A variable so a test need not wait minutes.
var leaseRenewEvery = func(ttl time.Duration) time.Duration { return ttl / 3 }

// leaseRun is `lease run KEY [--ttl 30m] [--note TEXT] -- CMD [ARGS...]`: take the lease or refuse,
// run CMD, and release the lease when CMD ends, whatever ended it but a kill -9 (then the ttl
// does). It exits with CMD's code; 7 when the lease is someone else's and CMD did not run.
// noNode is what client.FromEnv said when no node is configured (c is nil then).
func leaseRun(c *client.Client, noNode error, me string, args []string, s Streams) error {
	dash := slices.Index(args, "--")
	before := args // what is for lease run: the key and its flags; CMD and its own flags follow the `--`
	if dash >= 0 {
		before = args[:dash]
	}
	fl := newFlagSet("lease run")
	ttl := fl.String("ttl", "30m", "how long the lease survives a run that dies (it is extended while the command runs)")
	note := fl.String("note", "", "what you are doing, for the others")
	settle := fl.Duration("settle", time.Second, "how long to wait for a lease taken elsewhere at the same moment")
	override := fl.String("override-hold", "", "run CMD although the lease is held or cannot be checked: who agreed and why (recorded in the audit log, the holder is told)")
	if slices.ContainsFunc(before, func(a string) bool { return a == "-h" || a == "-help" || a == "--help" }) {
		// The flags and exit 0 whatever else is given: a script probes for `lease run` with this, as
		// an aivoicebus older than it answers with a usage error.
		return parseFlags(fl, []string{"-h"}, s)
	}
	if dash < 0 || dash == len(args)-1 {
		return outbox.Usagef("lease run KEY [--ttl 30m] [--note TEXT] -- CMD [ARGS...]: the command to run follows a `--`")
	}
	if err := parseFlags(fl, before, s); err != nil {
		return err
	}
	if fl.NArg() < 1 || !msg.ValidLeaseKey(fl.Arg(0)) {
		return outbox.Usagef("lease run KEY: one key of 1 to 120 letters, digits and . _ : / @ + -")
	}
	key := fl.Arg(0)
	if err := parseFlags(fl, fl.Args()[1:], s); err != nil { // flags may follow the key too
		return err
	}
	if fl.NArg() != 0 {
		return outbox.Usagef("lease run KEY [--ttl 30m] [--note TEXT] -- CMD [ARGS...]: one key, and the note in --note")
	}
	ttlD, err := msg.ParseTTL(*ttl)
	if err != nil {
		return outbox.Usagef("lease run --ttl: %v", err)
	}
	overriding := false
	fl.Visit(func(f *flag.Flag) { overriding = overriding || f.Name == "override-hold" })
	if overriding && len([]rune(strings.TrimSpace(*override))) < minOverrideWhy {
		return outbox.Usagef("lease run --override-hold needs a reason of at least %d characters: who agreed, and why the lease does not apply", minOverrideWhy)
	}
	argv := args[dash+1:]

	// Signals are caught from here on, so one that arrives while the lease is being taken still
	// ends in a release.
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigc)

	// The lease is taken or refused here; a refusal that --override-hold waives runs CMD without one.
	var holder *msg.Lease
	var lease msg.Message
	var live []msg.Lease
	why := noNode
	if why == nil {
		live, why = c.Leases()
	}
	if why == nil {
		for _, h := range msg.Holders(live) {
			if h.Key == key {
				holder = &msg.Lease{Key: h.Key, By: h.By, ID: h.ID, At: h.At, Until: h.Until, Note: h.Note}
			}
		}
		if holder == nil {
			lease, holder, why = sendLease(c, me, key, *note, *ttl, *settle, s)
		}
	}
	held := false // this run holds the lease and must release it
	if why != nil || holder != nil {
		if !overriding {
			return refuseLease(key, argv[0], me, holder, why, s)
		}
		overrideHold(c, me, key, argv[0], *override, holder, why, s)
	} else {
		held = true
	}
	release := func() {
		if !held {
			return
		}
		if _, err := releaseLeases(c, me, key); err != nil {
			fmt.Fprintf(s.Err, "aivoicebus: lease run: the lease on %s was not released (%v): it ends when its ttl does; `aivoicebus lease release %s` frees it now\n", key, view.Sanitise(err.Error()), key)
		}
	}
	select {
	case sig := <-sigc: // asked to stop before the command began
		release()
		return exitError{signalCode(sig)}
	default:
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.In, s.Out, s.Err
	cmd.Env = append(os.Environ(), leaseEnv+"="+strings.Trim(os.Getenv(leaseEnv)+","+key, ","))
	if err := cmd.Start(); err != nil {
		release()
		code := 126 // the shell's codes: found but not runnable, and not found
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			code = 127
		}
		fmt.Fprintf(s.Err, "aivoicebus: lease run: %s did not start: %v\n", argv[0], view.Sanitise(err.Error()))
		return exitError{code}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var renew <-chan time.Time // a run that overrode the hold has no lease to extend
	if held {
		tick := time.NewTicker(max(leaseRenewEvery(ttlD), 10*time.Millisecond))
		defer tick.Stop()
		renew = tick.C
	}
	for last := lease.ID; ; {
		select {
		case sig := <-sigc:
			// A terminal's ^C has reached the command already, as it shares our process group; the
			// others are ours to pass on, and we wait for the command to end before releasing.
			if sig != os.Interrupt {
				_ = cmd.Process.Signal(sig)
			}
		case <-renew:
			next, other, err := sendLease(c, me, key, *note, *ttl, 0, s)
			switch {
			case err != nil:
				fmt.Fprintf(s.Err, "aivoicebus: lease run: the lease on %s was not extended (%v): it ends when its ttl does, and %s goes on\n", key, view.Sanitise(err.Error()), argv[0])
			case other != nil:
				fmt.Fprintf(s.Err, "aivoicebus: lease run: %s; the lease on %s was lost, and %s goes on\n", view.Sanitise(leaseLine(*other, me)), key, argv[0])
			default:
				_, _ = c.Retract(last, me, false, false) // one live lease at a time, not one per extension
				last = next.ID
			}
		case err := <-done:
			release()
			return exitError{waitCode(err)}
		}
	}
}

// refuseLease reports why CMD did not run, naming the holder, the note and when it frees up, and
// returns the error a script tells from CMD's own failures by its code.
func refuseLease(key, cmd, me string, holder *msg.Lease, why error, s Streams) error {
	if holder == nil {
		return fmt.Errorf("lease run: the lease on %s could not be taken, so %s did not run: %w", key, cmd, why)
	}
	line := view.Sanitise(leaseLine(*holder, me))
	switch {
	case holder.By == me:
		fmt.Fprintf(s.Err, "error: %s (an earlier run of this session is still going, or died); %s did not run: wait for it, or `aivoicebus lease release %s` if it is gone\n", line, cmd, key)
	default:
		fmt.Fprintf(s.Err, "error: %s; %s did not run: wait for it to be released, ask its holder, or run again with `--override-hold \"who agreed and why\"` (audited, the holder is told)\n", line, cmd)
	}
	return exitError{exitLeaseHeld}
}

// overrideHold records that CMD runs without the lease it was refused (or could not check): in the
// audit log, which says who, what and why, and to the holder, who would otherwise find out from
// the damage. The notice is best effort; the audit line is the record.
func overrideHold(c *client.Client, me, key, cmd, reason string, holder *msg.Lease, why error, s Streams) {
	reason = view.OneLine(reason, 200)
	state, shown := "the nodes could not be asked", fmt.Sprintf("%s could not be checked: no node answered", key)
	if holder != nil {
		state = "held by " + holder.By + " until " + holder.Until.UTC().Format(time.RFC3339)
		if holder.Note != "" {
			state += " (" + view.OneLine(holder.Note, 120) + ")"
		}
		shown = leaseLine(*holder, me)
	}
	config.Audit(me, "lease override", fmt.Sprintf("ran %s without the lease on %s, %s; reason: %s", cmd, key, state, reason))
	fmt.Fprintf(s.Err, "aivoicebus: lease run: %s; running %s anyway on --override-hold (%s), recorded in `aivoicebus audit`\n", view.Sanitise(shown), cmd, reason)
	if holder == nil || holder.By == me || why != nil {
		return
	}
	body := fmt.Sprintf("lease override: %s ran %s without the lease on %s, which you hold until %s. Reason given: %s", view.Name(me, msg.MaxIdentity), cmd, key, holder.Until.UTC().Format("15:04Z"), reason)
	if plan, err := outbox.Prepare(c, outbox.Request{To: holder.By, Body: body, Prio: msg.PrioFYI, Queue: true}, s.Err); err == nil {
		_, _ = plan.Send(c)
	}
}

// waitCode is the exit code for how CMD ended: its own, or 128 plus the signal that killed it.
func waitCode(err error) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return exitFailure
}

// signalCode is 128 plus the signal's number, the shell's code for a process it stopped.
func signalCode(sig os.Signal) int {
	if n, ok := sig.(syscall.Signal); ok {
		return 128 + int(n)
	}
	return exitFailure
}
