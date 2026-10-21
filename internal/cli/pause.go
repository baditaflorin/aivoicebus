package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// validOnly reports whether s names exactly one session for --only: host/tool.id, a long-lived name
// of one session, or a machine's worker (host/worker: a tool's name alone reaches every session of
// the tool, but a machine has one worker).
func validOnly(s string) bool {
	if msg.OneSession(s) {
		return true
	}
	host, agent, ok := strings.Cut(s, "/")
	return ok && host != "" && host != "*" && agent == "worker" && msg.ValidIdentity(s)
}

// pauseCmd is the kill switch: every worker the target reaches stops its run and takes no task,
// schedules hold, and sessions are told to stop acting on peer messages, until --for runs out or
// a person on this machine runs `aivoicebus resume`. Anyone may pull it, an agent included.
//
// --only SESSION narrows it to exactly one session (host/tool.id, or a long-lived name such as
// host/worker), so testing the switch cannot stop a whole machine; the default target is still all.
// --test sends it in the test lane (msg/testlane.go): only a session that opted in and reads the
// lane, such as a worker started with --test-lane, sees it, and it ends with the message.
func pauseCmd(args []string, s Streams) error {
	const usageText = `usage: pause [all | MACHINE | --only SESSION] [--for 1h] [--test] "why"   (default all; --only stops exactly one session; resume lifts it)`
	usage := outbox.Usagef("%s", usageText)
	var target, reason, only string
	test := false
	d := time.Hour
	var words []string
	for i := 0; i < len(args); i++ {
		if a := args[i]; a == "--test" {
			test = true
			continue
		} else if a == "--only" || strings.HasPrefix(a, "--only=") {
			v, hasEq := strings.CutPrefix(a, "--only=")
			if !hasEq {
				if i+1 >= len(args) {
					return usage
				}
				i++
				v = args[i]
			}
			if only = strings.ToLower(strings.TrimSpace(v)); !validOnly(only) {
				return outbox.Usagef("--only is for exactly one session (host/tool.id, or host/worker), not %q: a machine, all, host/*, a selector or a person is not one session; plain `pause MACHINE` stops all of them: %s", view.Name(v, 80), usageText)
			}
			continue
		}
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
		switch a := args[i]; {
		case a == "-h" || a == "-help" || a == "--help":
			fmt.Fprintln(s.Out, usageText) // help, never a pause with "-h" as its reason
			return exitError{0}
		case strings.HasPrefix(a, "-") && a != "-":
			// a mistyped flag is not a reason: pausing the mesh must never happen by accident
			return outbox.Usagef("unknown flag %s (pause takes only --for): %s", a, usageText)
		}
		words = append(words, args[i])
	}
	switch {
	case only != "" && len(words) == 0:
		return usage
	case only != "": // everything that is not a flag is the reason
		target, reason = only, strings.Join(words, " ")
	case len(words) == 0:
		return usage
	case len(words) == 1:
		target, reason = "all", words[0]
	default:
		target, reason = words[0], strings.Join(words[1:], " ")
	}
	if test && d > msg.TestLife {
		return outbox.Usagef("a test pause lasts at most %s (--for): it is purged after that", outbox.SpanShort(msg.TestLife))
	}
	until := time.Now().Add(d)
	send := []string{"--to", target, "--prio", msg.PrioUrgent, "--ttl", d.String(), "--force", "--queue"}
	if test {
		send = append(send, "--test")
	}
	if err := sendWith(append(send, msg.PauseBody(until, reason)), s, false); err != nil {
		return err
	}
	config.Audit(identity.Me(), "pause", fmt.Sprintf("%s until %s: %s", target, until.UTC().Format(time.RFC3339), reason))
	fmt.Fprintf(s.Out, "paused %s until %s: workers stop and take no task, schedules hold, sessions are told to stop. A person here lifts it: aivoicebus resume %s\n", target, view.When(until, time.Now()), target)
	return nil
}

// resumeCmd lifts the pauses this machine sent. It needs a person: it asks on the terminal, which
// an agent's tool shell does not have.
func resumeCmd(args []string, s Streams) error {
	target := "all"
	switch {
	case len(args) == 2 && args[0] == "--only":
		if target = strings.ToLower(strings.TrimSpace(args[1])); !validOnly(target) {
			return outbox.Usagef("--only is for exactly one session (host/tool.id, or host/worker), not %q", view.Name(args[1], 80))
		}
	case len(args) == 1 && !strings.HasPrefix(args[0], "-"):
		target = args[0]
	case len(args) != 0:
		return outbox.Usagef("usage: resume [all | MACHINE | --only SESSION]")
	}
	line, err := readPassphrase("Type resume to let the agents go on: ")
	if err != nil {
		return fmt.Errorf("resume needs a person at a terminal: %v", err)
	}
	if strings.TrimSpace(line) != "resume" {
		return errors.New("not resumed: run it again and type resume")
	}
	if err := sendWith([]string{"--to", target, "--prio", msg.PrioUrgent, "--force", "--queue", msg.ResumeBody("")}, s, false); err != nil {
		return err
	}
	config.Audit(identity.Me(), "resume", target)
	fmt.Fprintln(s.Out, "resumed: the pauses this machine sent are lifted")
	return nil
}
