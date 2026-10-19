package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// nameCmd manages this machine's long-lived agent names (identity/names.go): a name registered
// here and asked for with AIVOICEBUS_NAME is the same identity after every restart.
func nameCmd(args []string, s Streams) error {
	const usage = "usage: name [list] | name add NAME | name rm NAME | name rename OLD NEW   (NAME looks like codex.build1)"
	if wantsHelp(args) {
		fmt.Fprintln(s.Out, usage)
		return nil
	}
	fs := newFlagSet("name")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	a := fs.Args()
	if len(a) == 0 {
		a = []string{"list"}
	}
	switch want := map[string]int{"list": 1, "add": 2, "rm": 2, "rename": 3}[a[0]]; {
	case want == 0 || len(a) != want:
		return outbox.Usagef("%s", usage)
	}
	switch a[0] {
	case "add":
		if err := identity.AddName(a[1]); err != nil {
			return outbox.Usagef("%v", err)
		}
		fmt.Fprintf(s.Out, "registered %s on this machine; as an identity it is %s/%s\n", strings.ToLower(a[1]), identity.Label(), strings.ToLower(a[1]))
		fmt.Fprintf(s.Out, "use it: AIVOICEBUS_NAME=%s <tool>   (one live session per name; a name for another tool than the one running is ignored)\n", strings.ToLower(a[1]))
	case "rm":
		if !slices.Contains(identity.RegisteredNames(), strings.ToLower(strings.TrimSpace(a[1]))) {
			return outbox.NotFoundf("%q is not registered on this machine: `aivoicebus name` lists the names", strings.ToLower(strings.TrimSpace(a[1])))
		}
		if err := identity.RemoveName(a[1]); err != nil {
			return err
		}
		fmt.Fprintf(s.Out, "removed %s; a session still asking for it falls back to its automatic identity\n", strings.ToLower(a[1]))
	case "rename":
		old, to := strings.ToLower(a[1]), strings.ToLower(a[2])
		if !slices.Contains(identity.RegisteredNames(), old) {
			return outbox.NotFoundf("%q is not registered on this machine: `aivoicebus name` lists the names", old)
		}
		if err := identity.AddName(to); err != nil { // validates before anything is dropped
			return outbox.Usagef("%v", err)
		}
		if err := identity.RemoveName(old); err != nil {
			return err
		}
		fmt.Fprintf(s.Out, "renamed %s to %s here; start sessions with AIVOICEBUS_NAME=%s from now on.\nMessages already sent to %s stay addressed to it: the mesh never rewrites them.\n", old, to, to, old)
	default:
		names := identity.RegisteredNames()
		for _, n := range names {
			fmt.Fprintf(s.Out, "%s/%s\n", identity.Label(), n)
		}
		if len(names) == 0 {
			fmt.Fprintln(s.Out, "(no long-lived names registered on this machine; aivoicebus name add codex.build1)")
		}
	}
	return nil
}
