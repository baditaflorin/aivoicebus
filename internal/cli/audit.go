package cli

import (
	"fmt"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// auditCmd shows the audit log: what was approved, run, stopped, released or allowed on this
// machine, and with --verify whether every line still holds (chained and signed).
func auditCmd(args []string, s Streams) error {
	fs := newFlagSet("audit")
	since := fs.Duration("since", 7*24*time.Hour, "how far back to list")
	verify := fs.Bool("verify", false, "check every line: in order, chained to the one before, signed by this machine (exit 1 if one is not)")
	asJSON := fs.Bool("json", false, "print the entries as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return outbox.Usagef("usage: audit [--since 168h] [--verify] [--json]")
	}
	es, bad := config.ReadAudit()
	if *asJSON {
		var out []config.AuditEntry
		for _, e := range es {
			if !e.At.Before(time.Now().Add(-*since)) {
				out = append(out, e)
			}
		}
		if out == nil {
			out = []config.AuditEntry{}
		}
		return writeJSON(s.Out, out)
	}
	if *verify {
		if len(bad) == 0 {
			fmt.Fprintf(s.Out, "audit log holds: %d line(s), in order, each chained to the one before and signed by this machine (cut lines at the end would not show)\n", len(es))
			return nil
		}
		for i := 1; i <= len(es)+len(bad); i++ {
			if why, ok := bad[i]; ok {
				fmt.Fprintf(s.Out, "line %d: %s\n", i, why)
			}
		}
		return exitError{exitFailure}
	}
	from := time.Now().Add(-*since)
	n := 0
	for _, e := range es {
		if e.At.Before(from) {
			continue
		}
		n++
		fmt.Fprintf(s.Out, "%4d  %s  %-36s %-18s %s\n", e.Seq, e.At.Local().Format("01-02 15:04:05"), msg.SafeName(e.Who), e.Action, e.Detail)
	}
	if n == 0 {
		fmt.Fprintln(s.Out, "nothing in the audit log in that time")
	}
	if len(bad) > 0 {
		fmt.Fprintf(s.Out, "\nwarning: %d line(s) do not hold: aivoicebus audit --verify\n", len(bad))
	}
	return nil
}
