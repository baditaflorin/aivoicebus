package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/share"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
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
		if h.Kind == "share" {
			heldShare(h, s.Out)
			continue
		}
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
		return outbox.Usagef("usage: unhold ID   (a held message's or shared file's id, from aivoicebus held)")
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
	if agent, why := identity.AgentCaller(); agent {
		return fmt.Errorf("%s is for the user, in their own terminal, not for an agent's session (%s): ask the user to run it", cmd, why)
	}
	return nil
}

// claimer is who takes a task from this process: an agent in an agent's session (AIVOICEBUS_AGENT,
// or an agent tool above this process), else the person at their terminal. Only the person takes
// a task meant for them (msg.PersonOnly).
func claimer() tasks.Caller {
	if agent, _ := identity.AgentCaller(); agent {
		return tasks.Agent
	}
	return tasks.Person
}

// heldShare shows the person a version of a shared space that was held back from agents.
func heldShare(h config.Held, w io.Writer) {
	kind := share.KindFile
	if h.Name == share.NotesName {
		kind = share.KindNotes
	}
	fmt.Fprintf(w, "%s in thread %s's shared space:\n", view.OneLine(h.Name, share.MaxName), h.Thread)
	sp, err := openSpace(h.Thread, h.Name)
	if err != nil {
		fmt.Fprintln(w, "  (cannot be read now: "+view.OneLine(err.Error(), 200)+")")
		return
	}
	sp.Screen = nil // the person reads it
	v, data, err := sp.Get(h.Name, kind, 0, h.ID)
	if err != nil {
		fmt.Fprintln(w, "  (cannot be read now: "+view.OneLine(err.Error(), 200)+")")
		return
	}
	fmt.Fprintf(w, "version %d by %s%s:\n", v.Num, view.Name(v.By, msg.MaxIdentity), verdictNote(v.Verified))
	if !utf8.Valid(data) {
		fmt.Fprintf(w, "  (not text, %s: save it with `aivoicebus share get %s %s --id %s --out FILE`)\n", share.SizeText(v.Size), h.Thread, shellWord(h.Name), h.ID)
		return
	}
	for _, line := range strings.Split(view.Sanitise(string(data)), "\n") {
		fmt.Fprintln(w, "  | "+line)
	}
}
