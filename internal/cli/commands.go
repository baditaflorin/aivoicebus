package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/hook"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

func whoami(args []string, s Streams) error {
	fs := newFlagSet("whoami")
	labels := fs.Bool("labels", false, "print this session's labels (key=value,...) instead of its identity")
	why := fs.Bool("why", false, "also say which tool was detected and how")
	asJSON := fs.Bool("json", false, "print the identity, labels and detection as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(s.Out, struct {
			Me     string     `json:"me"`
			Labels msg.Labels `json:"labels"`
			Why    string     `json:"why"`
		}{identity.Me(), identity.Labels(""), identity.Printable(identity.Explain().Why())})
	}
	if *labels {
		fmt.Fprintln(s.Out, identity.Labels("").String())
		return nil
	}
	fmt.Fprintln(s.Out, identity.Me())
	if *why {
		fmt.Fprintln(s.Out, identity.Printable(identity.Explain().Why()))
	}
	return nil
}

func hookCmd(args []string, s Streams) error {
	hook.Run(args, stdinOrNil(s.In), s.Out)
	return nil
}

func send(args []string, s Streams) error { return sendImpl(args, s, false, true) }

// reply is send with --re taken from the last unread message when it is not given.
func reply(args []string, s Streams) error { return sendImpl(args, s, true, true) }

// sendWith is send and reply for the commands that send as part of their own work (pause, room, start,
// the worker's approval, ask): what they send is not checked for a stale target or a contradicted priority.
func sendWith(args []string, s Streams, reply bool, moves ...taskMove) error {
	return sendImpl(args, s, reply, false, moves...)
}

// trailingFlags parses the flags written after the text (task laptop-b "do this" --wait-reply 10m):
// the flag package stops at the first word that is not a flag, which would send "--wait-reply 10m"
// as part of the text and never wait. Only a word that names one of this command's flags starts
// them, so text such as "-5" stays text, and "--" keeps everything after it as text. It returns the
// text's words.
func trailingFlags(fs *flag.FlagSet, s Streams) ([]string, error) {
	words := fs.Args()
	for i, w := range words {
		if w == "--" {
			return append(words[:i:i], words[i+1:]...), nil
		}
		name, _, _ := strings.Cut(strings.TrimLeft(w, "-"), "=")
		if !strings.HasPrefix(w, "-") || name == "" || fs.Lookup(name) == nil {
			continue
		}
		if err := parseFlags(fs, words[i:], s); err != nil {
			return nil, err
		}
		if fs.NArg() > 0 {
			return nil, outbox.Usagef("text on both sides of the flags (%q after them): put the text in one place, quoted", view.OneLine(strings.Join(fs.Args(), " "), 60))
		}
		return words[:i], nil
	}
	return words, nil
}

// taskMove is task --fallback: the next targets, in order, and how long each has to take it.
type taskMove struct {
	next        []string
	claimWithin time.Duration
	// ask is `aivoicebus ask`: a task nobody took in claimWithin, with no next machine left, is
	// taken back and the command exits 3 (nobody took it); a session it went to has it once it
	// read it; and each next machine's copy goes to the session pick chooses there ("" skips one
	// with nobody live). why is who it went to and why, for --json.
	ask  bool
	why  string
	pick func(machine string) (to, why string)
}

// sendImpl is send and reply. reply answers the newest unread message unless --re says which.
// authored is a message somebody wrote by hand, which outbox.Prepare also checks (Request.Authored).
func sendImpl(args []string, s Streams, reply, authored bool, moves ...taskMove) error {
	name := "send"
	if reply {
		name = "reply"
	}
	fs := newFlagSet(name)
	args = joinWaitState(args)
	ws := &waitState{}
	fs.Var(ws, "wait-state", "after sending, wait until the conversation gets to this state or past it (offered|taken|ready|doing|done|stuck|declined), e.g. --wait-state ready 10m; it also ends when the other side writes, or it becomes stuck or final (exit 1)")
	to := fs.String("to", "", "all | host | host/agent | */agent | host/* | @repo=x,branch=y | @here | @+group (see: aivoicebus usage); default for --re: whoever wrote that message")
	re := fs.String("re", "", "reply to message id (or its first characters, if they name one message)")
	thread := fs.String("thread", "", "thread id (default: new, or the reply's)")
	prio := fs.String("prio", "", strings.Join(msg.PrioNames(), " | ")+" (default ask, or fyi when the text says FYI/DONE/no action needed and asks nothing): fyi is only counted at session start, urgent is shown first")
	kind := fs.String("kind", "", strings.Join(msg.KindNames(), " | ")+": a question to several sessions stops being delivered once someone answers it (an answer needs --re)")
	ttl := fs.String("ttl", "", "how long the message stays relevant, e.g. 90m, 2h, 3d; expired messages are not delivered. Default: 1d for fyi and 3d otherwise when it can reach several sessions, never for one session")
	queue := fs.Bool("queue", false, "send a selector message even if no session seen in the last 24h matches it, or one to a session not seen for 30m (it waits for one that starts later)")
	dry := fs.Bool("dry-run", false, "check the message and say who it would reach and what it would cost, without sending")
	force := fs.Bool("force", false, "send despite the guardrails (urgent to many sessions, a long body to many sessions, one session not seen for 30m)")
	file := fs.String("file", "", "send a file: it is encrypted here, held for 5 minutes (read once) by the pipe or by a node (--via), and the message carries the link; the reader runs: aivoicebus get ID")
	via := fs.String("via", "", "with --file: pipe (the pipe server, default) or node (this mesh's own nodes hold it); default $AIVOICEBUS_FILE_VIA, else pipe")
	tail := fs.String("tail", "", "point at a live log held by tail server: a stream name, or the word new for a fresh one; feed it with: cmd | aivoicebus tail push --stream NAME")
	openLink := fs.Bool("open-link", false, "with --file: put the key in the link as before instead of sealing it for the recipient machine (for a reader on 0.10.4 or older, or the pipe's own p save): whoever reads the board can then fetch the file once")
	keep := fs.Bool("keep", false, "with --file: let several sessions read the file (it is not burned by the first read)")
	wait := fs.Bool("wait", false, "with --file: wait up to 5 minutes and say whether it was read")
	var metas metaFlag
	fs.Var(&metas, "meta", "with --file: seal a key=value inside the file's encrypted payload for the reader (repeatable; any keys: it is open data the reader may use or ignore; the file name, size and sha256 are sealed with it)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	private := fs.Bool("private", false, "seal the text to the recipient machine's key: nodes and every other reader see only that a private message went there (one machine or session, not a selector or all)")
	deadline := fs.Duration("deadline", 0, "start a conversation that closes this long after it starts (e.g. 1h): the nodes refuse later messages in it, an [end] always goes")
	maxTurns := fs.Int("max-turns", 0, "start a conversation that holds at most this many messages (claims not counted): the nodes refuse more, an [end] always goes; stops two agents from going on for ever")
	budgetChars := fs.Int("budget-chars", 0, "start a conversation that holds at most this many characters of messages in all: the nodes refuse a message that would pass it, an [end] always goes; bounds what it costs")
	waitReply := fs.Duration("wait-reply", 0, "after sending, wait this long (e.g. 10m) for the answer in the thread and print it: two agents can go back and forth without anyone relaying (exit 6 if nothing came)")
	quietAfter := fs.Duration("quiet-after", tasks.QuietAfter, "with a task and --wait-reply: say so once when the session that took it says nothing (no progress, answer or end) this long, and with --fallback give it to the next machine; 0: never")
	var df deadlineFlags
	fs.DurationVar(&df.replyBy, "reply-by", 0, "wait this long (up to 24h) for a reply, then say whether anyone read the message: read by 0 of 1 is exit 8 (silence is not consent), a reply is exit 1, read and no reply is exit 0; ends early on a reply")
	fs.StringVar(&df.needBy, "need-by", "", "--reply-by as a time: 15:40Z (UTC), 15:40 (this machine's clock) or 2026-10-22T15:40:00Z")
	fs.DurationVar(&df.until, "until", 0, "with a task (the offer): wait up to this long (1m to 24h) for someone to take it; untaken, its ttl is this so the nodes stop delivering it, and the command says it is not being done (exit 3)")
	fs.StringVar(&df.escalate, "escalate", "", "with --reply-by, when nobody read it by the deadline: machine (an urgent reminder to the live sessions on its machine), person (ask-human), or machine,person in turn")
	fs.DurationVar(&df.escWait, "escalate-wait", 10*time.Minute, "with --escalate person: how long to wait for the person's answer (0: ask and return)")
	fs.BoolVar(&df.detach, "detach", false, "with a deadline: send, remember it and return at once; watch reports it when it passes, status ID shows it any time")
	ackIt := new(bool)
	end := new(bool)
	if reply {
		ackIt = fs.Bool("ack", false, "mark the message replied to as read once the reply is sent")
		end = fs.Bool("end", false, "this reply closes the conversation: the other side's wait stops, and nobody needs to answer it")
	}
	orSuccessor := fs.Bool("or-successor", reply, "if the session named is gone for 15 minutes with this unread, the nodes hand it to the session that replaces it (the newest live one on its machine with the same tool and repo, or one that claims its role): for one exact session, a note, a question or an answer. On for a reply (--or-successor=false keeps it for the session named), off for send")
	trustRole := fs.Bool("trust-role", false, "use a role alias (@build) that nobody pinned and one session claims; never for urgent or file/log sends. Better: aivoicebus role pin build 'host/claude.*'")
	test := fs.Bool("test", false, "send in the test lane: it reaches only sessions that opted in (AIVOICEBUS_TEST=1), is never put into a hook's context or a replay, is left out of stats, view and export, lives at most 10 minutes and is purged; a reply to a test message is one already. Read the answers with inbox --test")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	words, err := trailingFlags(fs, s)
	if err != nil {
		return err
	}
	body := strings.Join(words, " ")
	succSaid := false // --or-successor was typed, not only on by default
	fs.Visit(func(f *flag.Flag) { succSaid = succSaid || f.Name == "or-successor" })
	if succSaid && *orSuccessor && (*file != "" || *tail != "" || *test) {
		return outbox.Usagef("--or-successor does not combine with --file, --tail or --test (a file lives five minutes and a test ten): run it again without it")
	}
	if len(metas.m) > 0 && *file == "" {
		return outbox.Usagef("--meta goes with --file: it is sealed inside the file's payload")
	}
	if ws.state != "" && *waitReply > 0 {
		return outbox.Usagef("--wait-state and --wait-reply are two ways to wait: use one")
	}
	if *end {
		if ws.state != "" {
			return outbox.Usagef("--end closes the conversation, so there is no state to wait for")
		}
		if *kind != "" && *kind != msg.KindEnd {
			return outbox.Usagef("--end and --kind %s say different things; use one", *kind)
		}
		*kind = msg.KindEnd
		if *waitReply > 0 {
			return outbox.Usagef("--end closes the conversation, so there is no reply to wait for")
		}
	}
	if *quietAfter < 0 || (*quietAfter > 0 && *quietAfter < time.Second) {
		return outbox.Usagef("--quiet-after is at least 1s, or 0 for never")
	}
	dl, err := parseDeadline(df, *kind, time.Now())
	if err != nil {
		return err
	}
	ttlText := *ttl
	if dl != nil {
		switch {
		case *waitReply > 0 || ws.state != "":
			return outbox.Usagef("a deadline and --wait-reply or --wait-state are ways to wait: use one")
		case len(moves) > 0:
			return outbox.Usagef("--fallback and ask have their own claim window: leave the deadline out")
		case *file != "" || *tail != "":
			return outbox.Usagef("a deadline does not combine with --file or --tail: send the message, then watch it with `aivoicebus status ID`")
		case *test:
			return outbox.Usagef("a deadline does not combine with --test: a test message lives at most 10 minutes")
		case *end:
			return outbox.Usagef("--end closes the conversation, so there is nothing to wait for")
		}
		if ttlText, err = deadlineTTL(*ttl, dl); err != nil {
			return err
		}
	}
	var fallbacks []string
	claimWithin := new(time.Duration)
	if len(moves) > 0 {
		fallbacks, *claimWithin = moves[0].next, moves[0].claimWithin
		switch {
		case *kind != msg.KindTask:
			return outbox.Usagef("--fallback is for tasks: aivoicebus task MACHINE \"...\" --fallback M2")
		case *waitReply <= 0:
			return outbox.Usagef("--fallback needs --wait-reply: the waiting command is what moves the task on")
		case *claimWithin < 5*time.Second || *claimWithin >= *waitReply:
			return outbox.Usagef("--claim-within is at least 5s and shorter than --wait-reply")
		case *private || *file != "" || *tail != "":
			return outbox.Usagef("--fallback does not combine with --private, --file or --tail")
		}
	}
	if *private && (*file != "" || *tail != "") {
		return outbox.Usagef("--private does not combine with --file or --tail: a file is already encrypted on this machine")
	}
	if *file != "" || *tail != "" {
		if *file != "" && *tail != "" {
			return errors.New("use --file or --tail, not both: run it again with one")
		}
		c, err := client.FromEnv()
		if err != nil {
			return err
		}
		if reply && *re == "" {
			return outbox.Usagef("reply with --file or --tail needs --re ID: nothing is inferred for an attachment")
		}
		if ws.state != "" {
			return outbox.Usagef("--wait-state does not combine with --file or --tail: send it, then aivoicebus state ID --wait-state %s", ws.state)
		}
		if err := ownerTarget(c, to, kind, &body, *prio, true, s); err != nil {
			return err
		}
		return sendAttachment(c, outbox.Request{To: *to, Re: *re, Thread: *thread, Prio: *prio, TTL: *ttl, Kind: *kind,
			Queue: *queue, Force: *force, Body: body, TrustRole: *trustRole, Authored: authored, Test: *test}, sendOpts{file: *file, tail: *tail, via: *via, openLink: *openLink, meta: metas.m, keep: *keep, wait: *wait, dry: *dry, waitReply: *waitReply}, s)
	}
	if body == "" || body == "-" {
		b, _ := io.ReadAll(s.In)
		body = strings.TrimSpace(string(b))
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	c.Labels = identity.Labels("") // a --wait-reply reads this session's mail with it
	inferred, err := inferRe(c, re, *to, reply, s)
	if err != nil {
		return err
	}
	if err := ownerTarget(c, to, kind, &body, *prio, false, s); err != nil {
		return err
	}
	plan, err := outbox.Prepare(c, outbox.Request{To: *to, Body: body, Re: *re, Thread: *thread, Prio: *prio,
		TTL: ttlText, Kind: *kind, Queue: *queue, Force: *force, TrustRole: *trustRole, TargetSaid: inferred, MaxTurns: *maxTurns, Deadline: *deadline, BudgetChars: *budgetChars,
		Authored: authored, Test: *test, Successor: *orSuccessor && authored, SuccessorAsked: succSaid && *orSuccessor}, s.Err)
	if err != nil {
		return err
	}
	if plan.Msg.IsTest() && (*waitReply > 0 || ws.state != "") && !*dry { // the answer is a test message too: only a session that opted in can read it
		if err := laneFor(c); err != nil {
			return err
		}
	}
	if *private {
		if to := plan.Msg.To; to == "all" || strings.HasPrefix(to, "@") || strings.HasPrefix(to, "*/") {
			return outbox.Usagef("--private goes to one machine (laptop-b) or one of its sessions, not to %s", to)
		}
		box, err := boxKeyFor(c, msg.HostOf(plan.Msg.To))
		if err != nil {
			return err
		}
		sealed, err := msg.Seal(plan.Msg.Body, box) // the text was checked for credentials before this
		if err != nil {
			return err
		}
		plan.Msg.Body = sealed
	}
	if dl != nil && slices.Contains(dl.escalate, stepMachine) && !namesOneMachine(plan.Msg.To) {
		return outbox.Usagef("--escalate machine needs a target on one machine (a machine, one of its sessions or tools), and %s reaches several: name one, or use --escalate person", view.Name(plan.Msg.To, msg.MaxIdentity))
	}
	if dl != nil {
		if h, err := c.Health(); err == nil && !h.Has(msg.FeatureReceipts) {
			return fmt.Errorf("nothing was sent: the node runs %s, which cannot report who read a message (it needs 0.4.0), so a deadline could never be judged; `aivoicebus mesh` shows what each node runs", view.OneLine(h.Version, 20))
		}
	}
	if *dry {
		dlLine := ""
		if dl != nil {
			dlLine = dl.describe(time.Now())
		}
		if *asJSON {
			return writeJSON(s.Out, planView(plan, dlLine))
		}
		plan.Describe(s.Out)
		if dlLine != "" {
			fmt.Fprintln(s.Out, "deadline: "+dlLine)
		}
		return nil
	}
	if inferred {
		// Someone may have written since the target was chosen: it was only safe to guess while it
		// was the one candidate, so look again and refuse rather than answer a different message.
		if replyRaceHook != nil {
			replyRaceHook()
		}
		if m, err := outbox.LastUnread(c, identity.Me()); err != nil || m.ID != plan.Msg.Re {
			if err == nil {
				err = outbox.Usagef("the unread message changed while the reply was being prepared (it was %s, it is now %s): nothing was sent; run it again or pick one with --re ID", plan.Msg.Re, m.ID)
			}
			return err
		}
	}
	if plan.Role != nil { // before the message goes: who the alias came to, in a line a script can read
		roleOut := s.Out
		if *asJSON { // stdout stays one JSON document; the line is still printed before anything is sent
			roleOut = s.Err
		}
		fmt.Fprintln(roleOut, plan.Role.Line())
	}
	for _, l := range plan.CapLines() { // who a capability route resolved to, before anything goes out
		fmt.Fprintln(s.Err, "  "+l)
	}
	out, err := plan.Send(c)
	if err != nil {
		return err
	}
	if *ackIt && plan.Msg.Re != "" { // the reply is out: its message is handled
		_ = c.AckVia(plan.Msg.Re, identity.Me(), msg.ViaCLI, 0)
	}
	if *asJSON && *waitReply == 0 && ws.state == "" && dl == nil {
		return writeJSON(s.Out, sentView{ID: out.ID, Thread: out.Thread, To: view.Name(out.To, msg.MaxIdentity), Re: out.Re, Reach: reachOf(plan), Notes: append([]string{}, plan.Notes...)})
	}
	sentOut := s.Out
	if *asJSON { // with --wait-reply, stdout is the one result object at the end
		sentOut = s.Err
	}
	fmt.Fprintln(sentOut, "sent", out.ID, "thread", out.Thread)
	repeat := out.ID != plan.Msg.ID // the node already held these very words from this sender (store dupWindow)
	if repeat {
		ago := "moments"
		if d := time.Since(out.At).Round(time.Second); d >= time.Second {
			ago = d.String()
		}
		fmt.Fprintf(s.Err, "  the same words went to the same target %s ago as %s, and the node keeps one copy of them for 10 minutes: this is that message, not a new one (change the words to send it again)\n", ago, out.ID)
	}
	if len(plan.Notes) > 0 {
		fmt.Fprintln(s.Err, "  "+strings.Join(plan.Notes, "; "))
	}
	fmt.Fprintln(s.Err, "undo: aivoicebus retract "+out.ID) // on stderr with the notes: stdout stays one line a script can read the id from
	if len(plan.Suspect) > 0 {
		fmt.Fprintln(s.Err, "if that went out: aivoicebus retract --leak "+out.ID)
	}
	if dl != nil {
		said := body
		if *private {
			said = ""
		}
		return runDeadline(c, dl, plan, said, out, *asJSON, s)
	}
	if ws.state != "" {
		return waitForState(c, identity.Me(), out.ID, ws, *asJSON, "", s)
	}
	if *waitReply > 0 {
		me := identity.Me()
		takenBy := ""
		var mv taskMove
		if len(moves) > 0 {
			mv = moves[0]
		}
		result := func(cur msg.Message, got []msg.Message, outcome string) resultView {
			r := resultOf(cur, takenBy, got)
			if mv.ask {
				r.Outcome, r.Why = outcome, mv.why
			}
			return withState(c, r) // the conversation's state when the wait ended
		}
		turns, closeAfter, budget := *maxTurns, *deadline, *budgetChars // the conversation's limits, for a copy moved on
		deliver := func(cur msg.Message, got []msg.Message, ack bool) error {
			if !*asJSON {
				showArrived(c, me, got, ack, s)
				return nil
			}
			for _, m := range got {
				if ack {
					_ = c.AckVia(m.ID, me, msg.ViaCLI, 0)
				}
			}
			if takenBy == "" && cur.Kind == msg.KindTask { // a claim this wait did not see (a repeat): the nodes know
				if r, err := c.Receipts(cur.ID); err == nil && r.Owner != nil {
					takenBy = r.Owner.By
				}
			}
			return writeJSON(s.Out, result(cur, got, "answered"))
		}
		if repeat { // its answer may be in already, read by the first wait: show it rather than wait for another
			if done := answeredIn(c, out, me); len(done) > 0 {
				fmt.Fprintln(s.Err, "that message was answered already:")
				return deliver(out, done, false)
			}
		}
		fmt.Fprintf(s.Err, "waiting up to %s for an answer in thread %s...\n", waitReply.String(), out.Thread)
		deadline := time.Now().Add(*waitReply)
		cur := out
		// Who took the current task, when, and when they last said how it goes (a claim or progress):
		// the local time it arrived drives the quiet timer, the node's times are what the alert says.
		var takenAt, lastWordAt, lastHeard time.Time
		claimBy := time.Now().Add(*claimWithin) // with --fallback: when the current target's turn ends
		quietSaid := false
		held := false // ask: the nodes say the one session asked read it, or cannot say: not moved on
		quiet := time.Duration(0)
		if cur.Kind == msg.KindTask {
			quiet = *quietAfter
		}
		// moveOn takes cur back and gives the task to the next fallback; why says what happened.
		moveOn := func(why string) error {
			if _, err := c.Retract(cur.ID, me, false, false); err != nil {
				fmt.Fprintf(s.Err, "%s, and it could not be taken back (%v): not moving it on\n", why, err)
				fallbacks, held = nil, true
				return nil
			}
			next, nextWhy := "", ""
			for next == "" && len(fallbacks) > 0 {
				next, fallbacks = fallbacks[0], fallbacks[1:]
				if mv.pick != nil {
					next, nextWhy = mv.pick(next)
				}
			}
			if next == "" { // ask, and no next machine with anyone there
				fmt.Fprintf(s.Err, "%s: took %s back. Nobody answered; ask again later or another target.\n", why, cur.ID)
				if *asJSON {
					_ = writeJSON(s.Out, result(cur, nil, "nobody"))
				}
				return exitError{exitNotFound}
			}
			np, err := outbox.Prepare(c, outbox.Request{To: next, Body: plan.Msg.Body, Prio: plan.Msg.Prio, Kind: msg.KindTask, Queue: true, TargetSaid: true,
				MaxTurns: turns, Deadline: closeAfter, BudgetChars: budget}, io.Discard)
			if err != nil {
				return fmt.Errorf("moving the task on to %s: %w", next, err)
			}
			old := cur.ID
			if cur, err = np.Send(c); err != nil {
				return fmt.Errorf("moving the task on to %s: %w", next, err)
			}
			fmt.Fprintf(s.Err, "%s: took %s back and gave it to %s as %s\n", why, old, view.Name(next, msg.MaxIdentity), cur.ID)
			if nextWhy != "" {
				mv.why = nextWhy
				fmt.Fprintf(s.Err, "  %s: %s\n", view.Name(next, msg.MaxIdentity), nextWhy)
			}
			out = cur
			takenBy, takenAt, lastWordAt, lastHeard, quietSaid, held = "", time.Time{}, time.Time{}, time.Time{}, false, false
			claimBy = time.Now().Add(*claimWithin)
			return nil
		}
		for {
			window := time.Until(deadline)
			switch {
			case takenBy == "" && !held && (len(fallbacks) > 0 || mv.ask):
				window = min(window, max(time.Until(claimBy), 0))
			case takenBy != "" && quiet > 0 && !quietSaid:
				window = min(window, max(time.Until(lastHeard.Add(quiet)), 0))
			}
			if quiet > 0 && !quietSaid {
				// A claim does not end a wait, so a wait that starts untaken looks again within
				// quiet: the timer then runs from the claim, not from the end of a long window.
				window = min(window, quiet)
			}
			progress := claimProgress(s)
			got, err := waitFor(c, me, tasks.Filter{Thread: cur.Thread, Progress: func(m msg.Message) {
				if takenBy == "" {
					takenBy, takenAt = m.From, m.At
				}
				lastWordAt, lastHeard = m.At, time.Now()
				progress(m)
			}}, window, 3*time.Second)
			if err == nil {
				return deliver(cur, got, true)
			}
			if !errors.Is(err, tasks.ErrTimeout) {
				return err
			}
			if takenBy != "" && quiet > 0 && !quietSaid && time.Since(lastHeard) >= quiet {
				quietSaid = true
				line := tasks.SilentText(tasks.Silent{ID: cur.ID, To: cur.To, By: takenBy, TakenAt: takenAt, LastWord: lastWordAt}, time.Now())
				fmt.Fprintf(s.Err, "[aivoicebus] %s. Ask them: aivoicebus reply --re %s \"...\"\n", strings.TrimPrefix(line, "aivoicebus: "), cur.ID)
				if len(fallbacks) > 0 && time.Until(deadline) >= time.Second {
					if err := moveOn(fmt.Sprintf("%s took it and then said nothing for %s", view.Name(takenBy, msg.MaxIdentity), quiet.String())); err != nil {
						return err
					}
				}
				continue
			}
			if time.Until(deadline) < time.Second {
				if mv.ask && takenBy == "" && !held { // nobody took it at all: take it back (exit 3)
					fallbacks = nil
					return moveOn(fmt.Sprintf("nobody took it within %s", waitReply.String()))
				}
				fmt.Fprintf(s.Err, "no answer in %s; later: aivoicebus wait --thread %s\n", waitReply.String(), cur.Thread)
				if *asJSON {
					_ = writeJSON(s.Out, result(cur, nil, "timeout"))
				}
				return exitError{exitTimeout}
			}
			// Nobody took it in --claim-within: take it back there and give it to the next target.
			// (A window can end sooner, to look at the quiet timer: that is not this.)
			if takenBy == "" && !held && (len(fallbacks) > 0 || mv.ask) && !time.Now().Before(claimBy) {
				if mv.ask { // a session asked may answer without claiming: once it read it, it has it
					if by, how, known := tasks.TakenBy(c, cur); !known || by != "" {
						held = true
						if by != "" {
							takenBy, lastHeard = by, time.Now()
							fmt.Fprintf(s.Err, "[aivoicebus] %s %s; still waiting for the answer...\n", view.Name(by, msg.MaxIdentity), how)
						}
						continue
					}
				}
				if err := moveOn(fmt.Sprintf("nobody took it within %s", claimWithin.String())); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// answeredIn is what others said in m's thread after m (answers, ends, replies; not claims), for a
// repeated send whose first wait already read them.
func answeredIn(c *client.Client, m msg.Message, me string) []msg.Message {
	ms, err := c.Thread(m.Thread)
	if err != nil {
		return nil
	}
	var out []msg.Message
	for _, x := range ms {
		if x.From != me && x.Retracted == nil && x.Kind != msg.KindClaim && !x.At.Before(m.At) && x.ID != m.ID {
			out = append(out, x)
		}
	}
	return out
}

// retractCmd takes a sent message back. The id may come before or after the flags
// (`retract ID --leak` and `retract --leak ID` are the same). With no id it is this session's last
// message and no other: --last is the person's way to say "whatever this machine sent last".
func retractCmd(args []string, s Streams) error {
	const usage = "usage: retract [ID] [--leak] [--dry-run] [--last] [--as IDENTITY]"
	fs := newFlagSet("retract")
	leak := fs.Bool("leak", false, "the message held a credential: say so in the tombstone and the dashboard (rotate the credential; hiding the message does not unsend it)")
	dry := fs.Bool("dry-run", false, "say which message this would take back and what would happen, without writing anything")
	last := fs.Bool("last", false, "with no id, take back the latest message sent from this machine by any session on it, not only this session's own last message (the default); it asks for no id")
	as := fs.String("as", "", "retract as IDENTITY (host/agent), which must be on the machine that sent the message; needs the shared token, for an operator who is not on that machine")
	asJSON := fs.Bool("json", false, "print the outcome as JSON (the id and the report)")
	yes := fs.Bool("yes", false, "retract by an abbreviated id without asking (an abbreviation is only looked up among what this machine sent, at least 6 characters)")
	var id string
	var given bool
	rest := args
	for {
		if err := parseFlags(fs, rest, s); err != nil {
			return err
		}
		a := fs.Args()
		if len(a) == 0 {
			break
		}
		if given {
			return outbox.Usagef("%s", usage)
		}
		id, given, rest = a[0], true, a[1:]
	}
	if given && id == "" { // `retract "$ID"` with $ID unset must not mean "the last message"
		return outbox.Usagef("%s: the id is empty", usage)
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	if given && len(strings.TrimSpace(id)) < 10 { // retract 3fa9c0 names the message by its first characters
		ids := outbox.NewIDResolver(c, identity.Me())
		ids.Say = s.Err
		abbrev := id
		var to string
		var at time.Time
		if id, to, at, err = ids.ResolveSent(abbrev); err != nil {
			return err
		}
		fmt.Fprintf(s.Err, "retract target: %s to %s, sent %s ago\n", id, view.Name(to, 60), outbox.SpanText(time.Since(at).Round(time.Second)))
		if !*dry && !*yes {
			// Taking a message back is not undone by reading again, and an abbreviation is a
			// guess: a person at a terminal confirms it, anything else must say --yes.
			if !isTerminal(s.In) {
				return outbox.Usagef("retracting by an abbreviation needs --yes when there is no terminal to ask on (the target is named above)")
			}
			fmt.Fprint(s.Err, "take it back? [y/N] ")
			line, _ := bufio.NewReader(s.In).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				return outbox.Usagef("not confirmed: nothing was taken back")
			}
		}
	} else if given {
		id = strings.ToLower(strings.TrimSpace(id))
	}
	if !*asJSON {
		return outbox.Retract(c, id, outbox.RetractOptions{Leak: *leak, DryRun: *dry, As: *as, Last: *last}, s.Out)
	}
	var report strings.Builder
	err = outbox.Retract(c, id, outbox.RetractOptions{Leak: *leak, DryRun: *dry, As: *as, Last: *last}, &report)
	if err != nil {
		return err // its kind (not found, ...) decides the exit code, so it is not re-wrapped with the report
	}
	return writeJSON(s.Out, struct {
		ID     string `json:"id,omitempty"`
		DryRun bool   `json:"dry_run"`
		Report string `json:"report"`
	}{id, *dry, strings.TrimSpace(report.String())})
}

func inbox(args []string, s Streams) error {
	fs := newFlagSet("inbox")
	all := fs.Bool("all", false, "include already-acked")
	ack := fs.Bool("ack", false, "mark shown messages as read")
	watch := fs.Duration("watch", 0, "poll interval (e.g. 20s); 0 = once")
	asJSON := fs.Bool("json", false, "print the messages as a JSON array (with --watch: one compact array per poll that has any)")
	test := fs.Bool("test", false, "also read the test lane (needs AIVOICEBUS_TEST=1): the test messages sent to this session, which no other read shows")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	c.Labels = identity.Labels("")
	if *test {
		if err := laneFor(c); err != nil {
			return err
		}
	}
	for {
		me := identity.Me()
		ms, err := c.Inbox(me, *all)
		if err != nil {
			return err
		}
		if *asJSON && (len(ms) > 0 || *watch == 0) {
			if err := writeMessages(s.Out, ms, *watch > 0); err != nil {
				return err
			}
		}
		if len(ms) > 0 {
			if !*asJSON {
				view.ForAgent(s.Out, ms, 0)
			}
			if *ack {
				for _, m := range ms {
					if m.Retracted != nil { // --all lists a withdrawn message as a tombstone: nobody read it, so it is no read receipt
						continue
					}
					_ = c.AckVia(m.ID, me, msg.ViaInbox, utf8.RuneCountInString(m.Body))
				}
			}
		}
		if *watch == 0 {
			if len(ms) == 0 && !*asJSON {
				fmt.Fprintln(s.Out, "(no unread messages)")
			}
			return nil
		}
		time.Sleep(*watch)
	}
}

func ack(args []string, s Streams) error {
	if wantsHelp(args) {
		fmt.Fprintln(s.Out, "usage: ack ID [ID...]   mark messages as read (an id may be its first 4+ characters if they name one message)")
		return nil
	}
	if len(args) == 0 {
		return nil
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	ids := outbox.NewIDResolver(c, identity.Me())
	ids.Say = s.Err
	full := make([]string, len(args)) // resolved first: an ambiguous one acknowledges nothing
	for i, id := range args {
		if full[i], err = ids.Resolve(id); err != nil {
			return err
		}
	}
	for _, id := range full {
		if err := c.AckVia(id, identity.Me(), msg.ViaCLI, 0); err != nil {
			return err
		}
	}
	return nil
}

func thread(args []string, s Streams) error {
	fs := newFlagSet("thread")
	asJSON := fs.Bool("json", false, "print the messages as a JSON array")
	digest := fs.Bool("digest", false, "the thread in a few lines: who took part, the task's state, each side's last word, open or ended")
	stale := fs.Bool("close-stale", false, "end a conversation nobody has written in for --stale-after, with a summary (only its sender or the user; never done on its own)")
	staleAfter := fs.Duration("stale-after", 30*time.Minute, "with --close-stale: how long without a message makes a conversation stale")
	test := fs.Bool("test", false, "read the test lane too (needs AIVOICEBUS_TEST=1): a thread of test messages is shown by no other read")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	rest := takeJSON(fs.Args(), asJSON)
	if len(rest) > 1 { // flags after the id: thread ID --digest, thread ID --close-stale
		if err := parseFlags(fs, rest[1:], s); err != nil {
			return err
		}
		rest = append([]string{rest[0]}, takeJSON(fs.Args(), asJSON)...)
	}
	if len(rest) != 1 {
		return outbox.Usagef("usage: thread <id> [--json | --digest | --close-stale [--stale-after 30m]]")
	}
	if *stale && (*asJSON || *digest) {
		return outbox.Usagef("--close-stale writes an [end]; it does not combine with --json or --digest: run it again with one of them")
	}
	if *staleAfter < time.Minute {
		return outbox.Usagef("--stale-after is at least 1m: run it again with a longer span")
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	if *test {
		c.Labels = identity.Labels("")
		if err := laneFor(c); err != nil {
			return err
		}
	}
	ids := outbox.NewIDResolver(c, identity.Me())
	ids.Say = s.Err
	id, err := ids.Resolve(rest[0])
	if err != nil {
		return err
	}
	ms, err := c.Thread(id)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		return outbox.NotFoundf("no thread with message %s on the nodes (a typo, or it has not reached them yet): try again in a minute, or check the id with `aivoicebus inbox --all`", id)
	}
	if *stale {
		return closeStale(c, ms, *staleAfter, s)
	}
	if *asJSON {
		return writeMessages(s.Out, ms, false)
	}
	if *digest {
		view.Digest(s.Out, ms)
		return nil
	}
	view.Thread(s.Out, ms, ms, 0)
	return nil
}

// writeMessages prints ms as a JSON array ([] for none); compact on one line when streaming.
func writeMessages(w io.Writer, ms []msg.Message, compact bool) error {
	ms = view.SanitiseMessages(ms) // JSON is read by programs that print it: C1 and bidi controls go too
	if compact {
		return json.NewEncoder(w).Encode(ms)
	}
	return writeJSON(w, ms)
}

// replyRaceHook runs between choosing a reply's target and re-checking it, for tests.
var replyRaceHook func()

// inferRe fills in --re for a reply that names none, but only when exactly one unread message
// could be answered (see outbox.LastUnread) and no --to was given: a recipient named by hand is
// never threaded under a message nobody chose. It says on stderr whom it will answer.
func inferRe(c *client.Client, re *string, to string, reply bool, s Streams) (bool, error) {
	if !reply || *re != "" {
		return false, nil
	}
	if to != "" {
		return false, outbox.Usagef("reply --to needs --re ID: a reply is threaded under the message it answers, and nothing is inferred when you name the recipient (use send for a new message)")
	}
	m, err := outbox.LastUnread(c, identity.Me())
	if err != nil {
		return false, err
	}
	*re = m.ID
	fmt.Fprintf(s.Err, "replying to %s from %s: %s\n", m.ID, view.Name(m.From, 60), view.OneLine(m.Body, 60))
	return true, nil
}

// sentView is `send --json`'s answer.
type sentView struct {
	ID     string   `json:"id"`
	Thread string   `json:"thread"`
	To     string   `json:"to"`
	Re     string   `json:"re,omitempty"`
	Reach  *int     `json:"reach"` // sessions it reaches; null when the nodes could not say
	Notes  []string `json:"notes"`
}

func reachOf(p outbox.Plan) *int {
	if !p.Known {
		return nil
	}
	return &p.Reach
}

// planView is `send --dry-run --json`'s answer.
func planView(p outbox.Plan, deadline string) any {
	m := p.Msg
	return struct {
		DryRun   bool     `json:"dry_run"`
		From     string   `json:"from"`
		To       string   `json:"to"`
		Re       string   `json:"re,omitempty"`
		Priority string   `json:"priority"`
		Kind     string   `json:"kind,omitempty"`
		TTL      int64    `json:"ttl_seconds"`
		Expiry   int64    `json:"default_expiry_seconds"`
		Reach    *int     `json:"reach"`
		Context  int      `json:"context_chars"`
		Body     int      `json:"body_chars"`
		Notes    []string `json:"notes"`
		Deadline string   `json:"deadline,omitempty"`
	}{true, view.Name(m.From, msg.MaxIdentity), view.Name(m.To, msg.MaxIdentity), m.Re, m.Priority(), m.Kind, m.TTL, int64(p.Expiry / time.Second), reachOf(p), p.Context,
		utf8.RuneCountInString(m.Body), append([]string{}, p.Notes...), deadline}
}

// who lists the sessions the answering node has heard from, with the labels
// each declared. Sessions not seen within --within (default 24h) are hidden, and
// the output says how many, so a stale entry is never mistaken for a live one.
// `who @repo=x` lists only those a message to that selector would reach, which
// previews a send before it goes out.
func who(args []string, s Streams) error {
	fs := newFlagSet("who")
	within := fs.String("within", "24h", "only sessions seen this recently (90m, 24h, 3d)")
	all := fs.Bool("all", false, "include sessions not seen within --within")
	asJSON := fs.Bool("json", false, "print the sessions as JSON (with --tools: the same list; the capabilities are in the labels)")
	tools := fs.Bool("tools", false, "group the sessions by tool and repo instead of listing them")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	var span time.Duration
	if !*all {
		var err error
		if span, err = msg.ParseSpan(*within); err != nil {
			return outbox.Usagef("--within: %v", err)
		}
	}
	var sel msg.Selector
	if a := fs.Args(); len(a) > 0 {
		if role, ok := msg.IsRoleAlias(strings.ToLower(a[0])); ok { // @build: who holds the role, and who a send would reach
			c, err := client.FromEnv()
			if err != nil {
				return err
			}
			outbox.WhoRole(s.Out, c, role, identity.Me())
			return nil
		}
		to, err := outbox.ExpandTarget(a[0]) // @here and @+group mean here what they mean to send
		if err != nil {
			return err
		}
		if sel, err = msg.ParseSelector(to); err != nil {
			return fmt.Errorf("bad selector %q: %v", a[0], err)
		}
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	now := time.Now()
	cutoff := now.Add(-span)
	hidden, shown := 0, 0
	rows := []msg.RosterEntry{} // what --json prints and --tools groups
	show := func(me string, seen time.Time, labels msg.Labels, title string, listening bool) {
		if !*all && seen.Before(cutoff) {
			hidden++
			return
		}
		shown++
		if *asJSON || *tools {
			rows = append(rows, msg.RosterEntry{Me: me, Seen: seen, Labels: labels, Title: title, Listening: listening})
			return
		}
		line := view.Session(msg.RosterEntry{Me: me, Seen: seen, Labels: labels, Title: title, Listening: listening}, now)
		if l := labels.String(); l != "" {
			line += "  " + view.Name(l, 300)
		}
		if caps := msg.CapsOf(me, labels); len(caps) > 0 { // what task/ask --needs matches
			line += "  can: " + view.Name(strings.Join(caps, " "), 300)
		}
		fmt.Fprintln(s.Out, line)
	}

	roster, err := c.Roster()
	switch {
	case err == nil:
		for _, e := range roster {
			if sel == nil || sel.Match(e.Labels) {
				show(e.Me, e.Seen, e.Labels, e.Title, e.Listening)
			}
		}
	case client.IsNotFound(err):
		// A node without a roster: list identities without labels.
		if sel != nil {
			return errors.New("this node does not report labels, so a selector cannot be previewed: ask the user to update the nodes")
		}
		w, err := c.Who()
		if err != nil {
			return err
		}
		for _, k := range slices.Sorted(maps.Keys(w)) {
			show(k, w[k], nil, "", false)
		}
	default:
		return err
	}

	switch {
	case *asJSON:
		type withCaps struct {
			msg.RosterEntry
			Caps []string `json:"caps"` // what task/ask --needs matches (msg.CapsOf)
		}
		out := []withCaps{}
		for _, e := range view.SanitiseRoster(rows) {
			out = append(out, withCaps{e, append([]string{}, msg.CapsOf(e.Me, e.Labels)...)})
		}
		return writeJSON(s.Out, struct {
			Sessions []withCaps `json:"sessions"`
			Hidden   int        `json:"hidden"` // seen, but not within --within
		}{out, hidden})
	case *tools:
		printToolGroups(s.Out, rows)
	}
	recent := "in the last " + *within
	switch {
	case sel != nil && shown == 0 && hidden == 0:
		fmt.Fprintf(s.Out, "(no session this node knows of matches %s)\n", sel)
	case sel != nil && shown == 0:
		fmt.Fprintf(s.Out, "(no session seen %s matches %s; %d older one(s) hidden, --all shows them)\n", recent, sel, hidden)
	case hidden > 0:
		fmt.Fprintf(s.Out, "(%d session(s) not seen %s hidden; --all shows them)\n", hidden, recent)
	}
	return nil
}

// cancelCmd takes a task back so that whoever runs it stops: a worker running it kills the run
// within seconds (and a write-mode run commits nothing); a session that took it sees it withdrawn.
// It is retract, said for what it does to a task.
func cancelCmd(args []string, s Streams) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return outbox.Usagef("usage: cancel ID   (a task this session sent)")
	}
	if err := retractCmd([]string{args[0]}, s); err != nil {
		return err
	}
	config.Audit(identity.Me(), "cancel", "task "+args[0])
	fmt.Fprintln(s.Out, "cancelled: a worker running it stops it within seconds and answers that it did; a session that took it sees it withdrawn")
	return nil
}
