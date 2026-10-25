package cli

import (
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// askCmd: `aivoicebus ask TARGET "question"` gets another machine's agent to answer in one
// command. It picks the one session to ask (tasks.PickSession: listening now first, then the one
// seen most recently, never a guess between look-alikes), sends the question as a task in a
// conversation with limits (4 turns, 15 minutes), waits for it to be taken and answered, and
// prints the answer. Exit: 0 answered, 3 nobody took it (it was taken back), 6 taken but not
// answered in time, 2 sessions that look alike (or a usage error).
func askCmd(args []string, s Streams) error {
	usage := outbox.Usagef(`usage: ask TARGET "question" [--tool codex] [--pick N] [--wait-reply 15m] [--claim-within 2m] [--fallback M2,M3] [--max-turns 4] [--deadline 15m] [--json]
       ask --any [--tool codex] "question"
       ask [TARGET] --needs repo:owner/name[,gpu...] "question"
  TARGET: a machine, host/tool, or one session (host/tool.id)`)
	fs := newFlagSet("ask")
	tool := fs.String("tool", "", "ask this tool's sessions there (host/TOOL), or with --any anywhere")
	anyOne := fs.Bool("any", false, "any session that matches: no TARGET needed, and sessions that look alike are not refused (one listening now is picked at random)")
	needs := fs.String("needs", "", "ask the session that has all of these capabilities (repo:owner/name, branch:, tool:, model:, role: or a declared word such as gpu; `aivoicebus who` lists them): listening now first, then the most recently seen. No TARGET needed")
	pick := fs.Int("pick", 0, "of the sessions that look alike (as listed when ask refuses), ask the Nth")
	within := fs.Duration("within", tasks.AskWithin, "a session seen this recently is live, when nobody is listening")
	readWithin := fs.Duration("read-within", outbox.ReadWithin, "warn (it still asks) when the session picked has read nothing for longer than this; 0: never")
	waitReply := fs.Duration("wait-reply", 0, "how long to wait for the answer (default: --deadline)")
	claimWithin := fs.Duration("claim-within", tasks.AskClaimWithin, "nobody took it (claimed, or read by the session asked) in this long: take it back, go to the next --fallback, else exit 3")
	fallback := fs.String("fallback", "", "the next machines, in order, for when nobody at TARGET takes it in --claim-within")
	maxTurns := fs.Int("max-turns", tasks.AskMaxTurns, "the conversation holds at most this many messages (the question and the answer are 2)")
	deadline := fs.Duration("deadline", tasks.AskDeadline, "the conversation closes this long after the question")
	ttl := fs.String("ttl", "", "how long the question stays relevant (default: --deadline)")
	prio := fs.String("prio", "", "the question's priority (default ask)")
	asJSON := fs.Bool("json", false, "print one result object: outcome (answered | nobody | timeout), why, taken_by, answers")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	words, err := trailingFlags(fs, s)
	if err != nil {
		return err
	}
	machine := ""
	if (!*anyOne && *needs == "") || len(words) > 1 {
		if len(words) < 2 {
			return usage
		}
		machine, words = words[0], words[1:]
	}
	question := strings.Join(words, " ")
	switch {
	case strings.TrimSpace(question) == "":
		return usage
	case *maxTurns < 2 || *maxTurns > msg.MaxTurnsLimit:
		return outbox.Usagef("--max-turns is 2 (the question and its answer) to %d: an ask always has a limit", msg.MaxTurnsLimit)
	case *deadline < time.Minute:
		return outbox.Usagef("--deadline is at least 1m: an ask always closes")
	case *pick < 0:
		return usage
	}
	if *waitReply == 0 {
		*waitReply = *deadline
	}
	if *waitReply > *deadline {
		return outbox.Usagef("--wait-reply %s is longer than --deadline %s: the conversation closes before that", *waitReply, *deadline)
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	*claimWithin = askClaimWithin(explicit["claim-within"], *claimWithin, *waitReply)
	if *ttl == "" {
		*ttl = deadline.String()
	}
	ownerOf := "" // the subject, when TARGET is owner:SUBJECT
	if subject, isOwner := msg.OwnerSubjectOf(machine); isOwner {
		if *anyOne || *needs != "" || *tool != "" || *fallback != "" || *pick > 0 {
			return outbox.Usagef("owner:SUBJECT picks its own session (the record's machine and what it needs): leave out --any, --needs, --tool, --fallback and --pick")
		}
		oc, err := sessionClient()
		if err != nil {
			return err
		}
		r, err := tasks.RouteOwner(oc, subject, identity.Me(), tasks.OwnerRouteOpts{Urgent: strings.EqualFold(*prio, msg.PrioUrgent)})
		if err := reportOwnerRoute(r, err, s); err != nil {
			return err
		}
		ownerOf, machine, *needs = subject, r.Machine, strings.Join(r.Needs, ",")
	}
	var ns []string
	if *needs != "" {
		if *pick > 0 || *fallback != "" {
			return outbox.Usagef("--needs picks the one session that has it all: leave out --pick and --fallback")
		}
		if ns, err = msg.ParseNeeds(*needs); err != nil {
			return outbox.Usagef("%v", err)
		}
	}
	target, err := askTarget(machine, *tool, *anyOne || ns != nil)
	if err != nil {
		return err
	}
	var next []string
	if *fallback != "" {
		if machine == "" {
			return outbox.Usagef("--fallback names the next machines after TARGET")
		}
		for _, m := range strings.Split(*fallback, ",") {
			if m = strings.TrimSpace(m); m == "" {
				continue
			}
			t, err := taskTarget(m, *tool, "", "")
			if err != nil {
				return outbox.Usagef("--fallback: %v", err)
			}
			next = append(next, t)
		}
	}
	c, err := sessionClient()
	if err != nil {
		return err
	}
	opts := tasks.PickOpts{Me: identity.Me(), Any: *anyOne, Pick: *pick, Within: *within, Rand: rand.IntN}
	roster, err := c.Roster()
	if err != nil {
		return err
	}
	if outbox.BareSession(target) { // claude.f324: the session of that name, wherever it runs
		full, how, err := outbox.ResolveBare(roster, target, opts.Me, time.Now(), *within, false)
		if err != nil {
			return outbox.Usagef("%v", err)
		}
		if how != "" {
			fmt.Fprintf(s.Err, "ask: %s\n", how)
			target = full
		}
	}
	var p tasks.Pick
	if ns != nil {
		np, err := tasks.PickNeeds(roster, target, ns, opts)
		if errors.Is(err, tasks.ErrNoMatch) {
			if *asJSON {
				_ = writeJSON(s.Out, resultView{To: view.Name(target, msg.MaxIdentity), Answers: []answerView{}, Outcome: "nobody", Why: err.Error(),
					Note: "nobody was asked: no live session has everything --needs asks for"})
			}
			if ownerOf != "" {
				fmt.Fprintf(s.Err, "the owner record asks for %s: `aivoicebus task owner:%s \"...\"` leaves it for the person on %s\n", view.OneLine(*needs, 80), view.Name(ownerOf, 80), view.OneLine(machine, 40))
			}
			return needsRefusal(err, np.Closest)
		}
		if err != nil {
			return err
		}
		p = tasks.Pick{To: np.To, Why: np.Why, Listening: np.Listening}
	} else {
		p, err = tasks.PickSession(roster, target, opts)
	}
	opts.Pick = 0 // --pick is for TARGET; a next machine is picked on its own
	switch {
	case errors.Is(err, tasks.ErrAlike):
		return alikeError(target, p.Alike, question, s)
	case errors.Is(err, tasks.ErrNobody):
		fmt.Fprintf(s.Err, "  %v\n", err)
		for len(next) > 0 && p.To == "" { // nobody at TARGET: the first next machine with someone there
			var why string
			p.To, why = pickAt(c, next[0], opts, s)
			p.Why, next = why, next[1:]
		}
		if p.To == "" {
			if ownerOf != "" {
				fmt.Fprintf(s.Err, "nobody took it: nothing was sent. To leave it for the owner: `aivoicebus task owner:%s \"...\"` (it goes to the next session there, or to the person there when nobody is live)\n", view.Name(ownerOf, 80))
			} else {
				fmt.Fprintf(s.Err, "nobody took it: nothing was sent. To leave it for the next session there: aivoicebus task %s \"...\"\n", view.Name(target, msg.MaxIdentity))
			}
			if *asJSON {
				_ = writeJSON(s.Out, resultView{To: view.Name(target, msg.MaxIdentity), Answers: []answerView{}, Outcome: "nobody", Why: err.Error(),
					Note: "nobody was asked: no live session matched"})
			}
			return exitError{exitNotFound}
		}
	case err != nil:
		return outbox.Usagef("%v", err)
	}
	fmt.Fprintf(s.Err, "ask: %s: %s\n", view.Name(p.To, msg.MaxIdentity), p.Why)
	send := []string{"--to", p.To, "--kind", msg.KindTask, "--wait-reply", waitReply.String(), "--max-turns", strconv.Itoa(*maxTurns),
		"--deadline", deadline.String(), "--ttl", *ttl, "--read-within", readWithin.String()}
	if *prio != "" {
		send = append(send, "--prio", *prio)
	}
	if *asJSON {
		send = append(send, "--json")
	}
	mv := taskMove{next: next, claimWithin: *claimWithin, ask: true, why: p.Why,
		pick: func(m string) (string, string) { return pickAt(c, m, opts, s) }}
	return sendWith(append(send, "--", question), s, false, mv)
}

// askTarget is who an ask may go to: a machine, host/tool or one session, or with --any anyone
// (of a tool).
func askTarget(machine, tool string, anyOne bool) (string, error) {
	if machine == "" {
		if !anyOne {
			return "", outbox.Usagef("ask needs a TARGET, or --any")
		}
		if tool == "" {
			return "all", nil
		}
		return taskTarget("", tool, "", "")
	}
	if msg.IsSelector(machine) || machine == "all" {
		return "", outbox.Usagef("ask goes to one session: name a machine, host/tool or a session, or say --any (not %s)", machine)
	}
	if strings.Contains(machine, "/") {
		if tool != "" {
			return "", outbox.Usagef("%s already names the tool; leave out --tool", machine)
		}
		return machine, nil
	}
	t, err := taskTarget(machine, tool, "", "")
	if err != nil {
		return "", outbox.Usagef("%v", err)
	}
	return t, nil
}

// pickAt is the session a next machine's copy goes to: the one picked there; the machine itself
// (its first claim wins) when several there look alike; "" when nobody there is live.
func pickAt(c *client.Client, target string, o tasks.PickOpts, s Streams) (string, string) {
	roster, err := c.Roster()
	if err != nil {
		return target, "the roster could not be read; the first session there to claim it has it"
	}
	p, err := tasks.PickSession(roster, target, o)
	switch {
	case errors.Is(err, tasks.ErrAlike):
		return target, fmt.Sprintf("%d sessions there look alike; the first to claim it has it", len(p.Alike))
	case errors.Is(err, tasks.ErrNobody):
		fmt.Fprintf(s.Err, "  %v: skipping it\n", err)
		return "", ""
	case err != nil:
		return target, err.Error()
	}
	return p.To, p.Why
}

// alikeError lists the sessions an ask would have to guess between and refuses (exit 2).
func alikeError(target string, alike []msg.RosterEntry, question string, s Streams) error {
	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "%d sessions at %s look alike, and asking one of them would be a guess:\n", len(alike), view.Name(target, msg.MaxIdentity))
	for i, e := range alike {
		fmt.Fprintf(&b, "  %d  %s\n", i+1, view.Session(e, now))
	}
	q := view.OneLine(question, 40)
	fmt.Fprintf(&b, "say which: aivoicebus ask %s %q --pick N, or name the session: aivoicebus ask %s %q",
		view.Name(target, msg.MaxIdentity), q, view.Name(alike[0].Me, msg.MaxIdentity), q)
	return outbox.Usagef("%s", b.String())
}

// askClaimWithin is how long ask gives a session to take the question. The default (2m) must not
// break a short --wait-reply: when the person did not set --claim-within, it fits inside the wait
// (half of it, at least 5s). A value the person set is kept, and checked as on task.
func askClaimWithin(set bool, claim, waitReply time.Duration) time.Duration {
	if set || claim < waitReply {
		return claim
	}
	return max(waitReply/2, 5*time.Second)
}
