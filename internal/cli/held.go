package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// heldCmd shows the person what was held back from agents, with the text, and unholdCmd lets
// agents see one. Both are the person's: they refuse in an agent's session, and ask on the terminal.
func heldCmd(args []string, s Streams) error {
	if len(args) != 0 {
		return outbox.Usagef("usage: held   (then: aivoicebus unhold ID)")
	}
	if err := personOnly("held"); err != nil {
		return err
	}
	hs := config.ReadHeld()
	if len(hs) == 0 {
		fmt.Fprintln(s.Out, "nothing was held back from agents")
		return nil
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	fmt.Fprintln(s.Out, view.UntrustedBanner)
	for _, h := range hs {
		state := "held"
		if config.Released(h.ID) {
			state = "released"
		}
		fmt.Fprintf(s.Out, "\n%s  %s  %s: %s\n", h.ID, view.When(h.At, time.Now()), state, h.Reason)
		ms, err := c.Thread(h.ID)
		if err != nil {
			continue
		}
		for _, m := range ms {
			if m.ID == h.ID {
				fmt.Fprintf(s.Out, "from %s to %s:\n", view.Name(m.From, msg.MaxIdentity), view.Name(m.To, msg.MaxIdentity))
				for _, line := range strings.Split(view.Sanitise(m.Body), "\n") {
					fmt.Fprintln(s.Out, "  | "+line)
				}
			}
		}
	}
	fmt.Fprintln(s.Out, "\nLet agents see one: aivoicebus unhold ID")
	return nil
}

func unholdCmd(args []string, s Streams) error {
	if len(args) != 1 || !msg.ValidID(args[0]) {
		return outbox.Usagef("usage: unhold ID   (a held message's full id, from aivoicebus held)")
	}
	if err := personOnly("unhold"); err != nil {
		return err
	}
	line, err := readPassphrase("Type unhold to let agents read " + args[0] + ": ")
	if err != nil {
		return fmt.Errorf("unhold needs a person at a terminal: %v", err)
	}
	if strings.TrimSpace(line) != "unhold" {
		return errors.New("not unheld: run it again and type unhold")
	}
	if err := config.Release(args[0]); err != nil {
		return err
	}
	config.Audit(identity.Me(), "unhold", args[0])
	fmt.Fprintf(s.Out, "released %s: agents on this machine now see it\n", args[0])
	return nil
}

// personOnly refuses inside an agent's session: these show or release what was kept from agents.
func personOnly(cmd string) error {
	if os.Getenv("AIVOICEBUS_AGENT") != "" {
		return fmt.Errorf("%s is for the user, in their own terminal, not for an agent's session: ask the user to run it", cmd)
	}
	return nil
}
