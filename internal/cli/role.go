package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// roleCmd manages the sender's role pins (~/.config/aivoicebus/roles): which sessions a message to
// @role may go to. A pinned role resolves normally; an unpinned one needs --trust-role.
func roleCmd(args []string, s Streams) error {
	const usage = "usage: role [list] | role pin ROLE 'host/agent-pattern' | role unpin ROLE   (e.g. role pin build 'build-*/claude.*')"
	if wantsHelp(args) {
		fmt.Fprintln(s.Out, usage)
		return nil
	}
	fs := newFlagSet("role")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	a := fs.Args()
	if len(a) == 0 {
		a = []string{"list"}
	}
	switch want := map[string]int{"list": 1, "pin": 3, "unpin": 2}[a[0]]; {
	case want == 0 || len(a) != want:
		return outbox.Usagef("%s", usage)
	}
	switch a[0] {
	case "pin":
		role, pat := strings.ToLower(a[1]), strings.ToLower(a[2])
		if !msg.ValidRoleName(role) {
			return outbox.Usagef("%q is not a role name (lower-case letters, digits, - and _; not a label or target word)", msg.SafeName(role))
		}
		if err := msg.ValidRolePin(pat); err != nil {
			return outbox.Usagef("%v", err)
		}
		err := config.UpdateSmall("roles", func(old string) (string, error) {
			if slices.Contains(msg.ParseRolePins(old)[role], pat) {
				return old, nil
			}
			if old != "" && !strings.HasSuffix(old, "\n") {
				old += "\n"
			}
			return old + role + "=" + pat + "\n", nil
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(s.Out, "@%s is pinned to %s: a send to it goes only to a live session that matches\n", role, msg.SafeName(pat))
	case "unpin":
		role := strings.ToLower(a[1])
		removed := false
		err := config.UpdateSmall("roles", func(old string) (string, error) {
			var keep []string
			for _, line := range strings.Split(old, "\n") {
				body, _, _ := strings.Cut(line, "#")
				if name, _, ok := strings.Cut(body, "="); ok && strings.ToLower(strings.TrimSpace(name)) == role {
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
			return outbox.NotFoundf("@%s is not pinned: `aivoicebus role` lists the pinned roles", msg.SafeName(role))
		}
		fmt.Fprintf(s.Out, "@%s is no longer pinned: a send to it needs --trust-role again\n", role)
	default:
		text, err := config.ReadRoles()
		if err != nil {
			return err
		}
		pins := msg.ParseRolePins(text)
		for _, role := range slices.Sorted(maps.Keys(pins)) {
			fmt.Fprintf(s.Out, "@%s -> %s\n", role, strings.Join(safeAll(pins[role]), ", "))
		}
		if len(pins) == 0 {
			fmt.Fprintln(s.Out, "(no roles pinned; aivoicebus role pin build 'build-*/claude.*')")
		}
	}
	return nil
}

func safeAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, x := range ss {
		out[i] = msg.SafeName(x)
	}
	return out
}
