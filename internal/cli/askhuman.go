package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

type askResult struct {
	ID       string `json:"id"`
	Answered bool   `json:"answered"`
	Answer   string `json:"answer,omitempty"`
	Via      string `json:"via,omitempty"` // "phone" (one-time code) or "terminal" (aivoicebus answer)
}

// askHumanCmd: an agent asks this machine's person a question and waits for the answer, which
// comes from the phone (the notification's choice buttons) or the terminal (aivoicebus answer).
func askHumanCmd(args []string, s Streams) error {
	fs := newFlagSet("ask-human")
	choicesT := fs.String("choices", "", "the answers to offer, comma-separated (e.g. yes,no): buttons on the phone")
	wait := fs.Duration("wait", 30*time.Minute, "how long to wait for the answer (exit 6 if none)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	words, err := trailingFlags(fs, s)
	if err != nil {
		return err
	}
	q := strings.TrimSpace(strings.Join(words, " "))
	if q == "" {
		return outbox.Usagef(`usage: ask-human "question" [--choices yes,no] [--wait 30m] [--json]`)
	}
	var choices []string
	for _, ch := range strings.Split(*choicesT, ",") {
		if ch = strings.ToLower(strings.TrimSpace(ch)); ch == "" {
			continue
		}
		if !msg.ValidChoice(ch) {
			return outbox.Usagef("a choice is one word: lowercase letters, digits, - or _")
		}
		choices = append(choices, ch)
	}
	if len(choices) > 3 {
		return outbox.Usagef("at most 3 choices (the phone shows 3 buttons)")
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	me := identity.Me()
	nu := notifyURL()
	code := ""
	if nu != "" && len(choices) > 0 && len(c.URLs) > 0 {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		code = hex.EncodeToString(b)
	}
	plan, err := outbox.Prepare(c, outbox.Request{To: msg.HumanOf(me), Body: msg.AskBody(q, choices, code), Prio: msg.PrioUrgent, Queue: true, TargetSaid: true}, io.Discard)
	if err != nil {
		return err
	}
	req, err := plan.Send(c)
	if err != nil {
		return err
	}
	if nu != "" {
		actions := ""
		if code != "" {
			var parts []string
			for _, ch := range choices {
				parts = append(parts, "http, "+ch+", "+strings.TrimRight(c.URLs[0], "/")+"/v1/decide?id="+req.ID+"&code="+code+"&say="+ch+", method=POST, clear=true")
			}
			actions = strings.Join(parts, "; ")
		}
		_ = tasks.NotifyActions(nu, "aivoicebus: "+msg.SafeName(me)+" asks you", q, actions)
	}
	fmt.Fprintf(s.Err, "asked (%s); the person answers on the phone, or in a terminal: aivoicebus answer %s\n", req.ID, req.ID)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res := askResult{ID: req.ID}
	deadline := time.Now().Add(*wait)
	for time.Until(deadline) > time.Second && !res.Answered {
		got, err := tasks.Wait(ctx, c, me, tasks.Filter{Thread: req.Thread}, time.Until(deadline), 3*time.Second)
		if err != nil {
			break
		}
		for _, m := range got {
			_ = c.AckVia(m.ID, me, msg.ViaCLI, 0)
			if say, got, ok := msg.ParsePhoneDecision(m.Body); ok && code != "" && subtle.ConstantTimeCompare([]byte(got), []byte(code)) == 1 {
				res.Answered, res.Answer, res.Via = true, say, "phone"
				break
			}
			if text, ok := strings.CutPrefix(m.Body, msg.HumanAnswerPrefix); ok && m.From == msg.HumanOf(me) {
				res.Answered, res.Answer, res.Via = true, text, "terminal"
				break
			}
		}
	}
	if *asJSON {
		if err := writeJSON(s.Out, res); err != nil {
			return err
		}
	} else if res.Answered {
		fmt.Fprintf(s.Out, "the person answered (%s): %s\n", res.Via, res.Answer)
	} else {
		fmt.Fprintf(s.Out, "no answer in %s\n", *wait)
	}
	if !res.Answered {
		return exitError{exitTimeout}
	}
	return nil
}

// readTTYLine reads one line typed on the terminal (a variable so tests can answer it).
var readTTYLine = func(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("no terminal here") // nextstep: wrapped by "answer needs a person at a terminal"
	}
	defer tty.Close()
	fmt.Fprint(tty, prompt)
	line, err := bufio.NewReader(tty).ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

// answerCmd is the person's side: with no id it lists the open questions to this machine's person,
// with an id it reads the answer on the terminal and sends it. Not for an agent's session.
func answerCmd(args []string, s Streams) error {
	if err := personOnly("answer"); err != nil {
		return err
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	human := msg.HumanOf(identity.Me())
	if len(args) == 0 {
		ms, err := c.Inbox(human, false)
		if err != nil {
			return err
		}
		n := 0
		fmt.Fprintln(s.Out, view.UntrustedBanner)
		for _, m := range ms {
			if msg.IsStuck(m.Body) {
				n++
				fmt.Fprintf(s.Out, "%s  %s is stuck: %s\n", m.ID, view.Name(m.From, msg.MaxIdentity), view.OneLine(strings.TrimPrefix(m.Body, msg.StuckPrefix), 300))
				continue
			}
			if strings.HasPrefix(m.Body, msg.AskPrefix) {
				n++
				q := strings.SplitN(strings.TrimPrefix(m.Body, msg.AskPrefix), "\n", 2)[0]
				fmt.Fprintf(s.Out, "%s  from %s: %s", m.ID, view.Name(m.From, msg.MaxIdentity), view.OneLine(q, 300))
				if ch := msg.AskChoices(m.Body); len(ch) > 0 {
					fmt.Fprintf(s.Out, "  [%s]", strings.Join(ch, "/"))
				}
				fmt.Fprintln(s.Out)
			}
		}
		if n == 0 {
			fmt.Fprintln(s.Out, "no open questions, and no agent says it is stuck")
		} else {
			fmt.Fprintln(s.Out, "answer one: aivoicebus answer ID")
		}
		return nil
	}
	if len(args) != 1 || !msg.ValidID(args[0]) {
		return outbox.Usagef("usage: answer [ID]")
	}
	ms, err := c.Thread(args[0])
	if err != nil {
		return err
	}
	var q msg.Message
	for _, m := range ms {
		if m.ID == args[0] && (strings.HasPrefix(m.Body, msg.AskPrefix) || (msg.IsStuck(m.Body) && m.To == msg.HumanOf(identity.Me()))) {
			q = m
		}
	}
	if q.ID == "" {
		return outbox.Usagef("%s is not a question to a person, nor an agent saying it is stuck", args[0])
	}
	fmt.Fprintln(s.Err, view.UntrustedBanner)
	if msg.IsStuck(q.Body) {
		fmt.Fprintf(s.Err, "%s is stuck: %s\n", view.Name(q.From, msg.MaxIdentity), view.OneLine(strings.TrimPrefix(q.Body, msg.StuckPrefix), 600))
	} else {
		fmt.Fprintf(s.Err, "%s asks: %s\n", view.Name(q.From, msg.MaxIdentity), view.OneLine(strings.TrimPrefix(q.Body, msg.AskPrefix), 600))
	}
	choices := msg.AskChoices(q.Body)
	prompt := "Your answer: "
	if len(choices) > 0 {
		prompt = "Your answer (" + strings.Join(choices, "/") + "): "
	}
	text, err := readTTYLine(prompt)
	if err != nil {
		return fmt.Errorf("answer needs a person at a terminal: %v", err)
	}
	if text = strings.TrimSpace(text); text == "" {
		return errors.New("no answer given; nothing sent: run `aivoicebus answer` again to answer")
	}
	if len(choices) > 0 && !slices.Contains(choices, strings.ToLower(text)) {
		return fmt.Errorf("the choices are %s: run `aivoicebus answer` again and type one", strings.Join(choices, ", "))
	}
	plan, err := outbox.Prepare(c, outbox.Request{To: q.From, Re: q.ID, Thread: q.Thread, Kind: msg.KindAnswer, Body: msg.HumanAnswerPrefix + text, TargetSaid: true, From: msg.HumanOf(identity.Me())}, io.Discard)
	if err != nil {
		return err
	}
	if _, err := plan.Send(c); err != nil {
		return err
	}
	_ = c.Ack(q.ID, msg.HumanOf(identity.Me()))
	fmt.Fprintln(s.Out, "sent")
	return nil
}

// progressCmd says how a task goes, to whoever waits on it: `progress --re ID "step 3/5"`.
func progressCmd(args []string, s Streams) error {
	return sendWith(append([]string{"--kind", msg.KindProgress}, args...), s, true)
}
