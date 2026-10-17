package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// pauseCmd is the kill switch: every worker the target reaches stops its run and takes no task,
// schedules hold, and sessions are told to stop acting on peer messages, until --for runs out or
// a person on this machine runs `aivoicebus resume`. Anyone may pull it, an agent included.
func pauseCmd(args []string, s Streams) error {
	usage := outbox.Usagef(`usage: pause [all | MACHINE] [--for 1h] "why"   (default all; resume lifts it)`)
	var target, reason string
	d := time.Hour
	var words []string
	for i := 0; i < len(args); i++ {
		if a := args[i]; a == "--for" || strings.HasPrefix(a, "--for=") {
			v, hasEq := strings.CutPrefix(a, "--for=")
			if !hasEq {
				if i+1 >= len(args) {
					return usage
				}
				i++
				v = args[i]
			}
			var err error
			if d, err = time.ParseDuration(v); err != nil || d < time.Minute || d > 7*24*time.Hour {
				return outbox.Usagef("--for is 1m to 168h")
			}
			continue
		}
		words = append(words, args[i])
	}
	switch len(words) {
	case 0:
		return usage
	case 1:
		target, reason = "all", words[0]
	default:
		target, reason = words[0], strings.Join(words[1:], " ")
	}
	until := time.Now().Add(d)
	if err := sendWith([]string{"--to", target, "--prio", msg.PrioUrgent, "--ttl", d.String(), "--force", "--queue", msg.PauseBody(until, reason)}, s, false); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "paused %s until %s: workers stop and take no task, schedules hold, sessions are told to stop. A person here lifts it: aivoicebus resume %s\n", target, until.Local().Format("01-02 15:04"), target)
	return nil
}

// resumeCmd lifts the pauses this machine sent. It needs a person: it asks on the terminal, which
// an agent's tool shell does not have.
func resumeCmd(args []string, s Streams) error {
	target := "all"
	if len(args) == 1 {
		target = args[0]
	} else if len(args) > 1 {
		return outbox.Usagef("usage: resume [all | MACHINE]")
	}
	line, err := readPassphrase("Type resume to let the agents go on: ")
	if err != nil {
		return fmt.Errorf("resume needs a person at a terminal: %v", err)
	}
	if strings.TrimSpace(line) != "resume" {
		return errors.New("not resumed")
	}
	if err := sendWith([]string{"--to", target, "--prio", msg.PrioUrgent, "--force", "--queue", msg.ResumeBody("")}, s, false); err != nil {
		return err
	}
	fmt.Fprintln(s.Out, "resumed: the pauses this machine sent are lifted")
	return nil
}
