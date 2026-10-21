package cli

import (
	"io"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// amendCmd replaces a message this machine sent with a corrected text: sessions that have not read the
// original are given only the corrected one, and sessions that have are given it marked as replacing
// what they read (outbox.Amend, msg/amend.go). The id may come before the text or, like a flag, after.
func amendCmd(args []string, s Streams) error {
	const usage = "usage: amend ID \"corrected text\" [--dry-run] [--force] [--json]"
	fs := newFlagSet("amend")
	dry := fs.Bool("dry-run", false, "check the message and say who it would reach and how many had read the original, without sending")
	force := fs.Bool("force", false, "send despite the guardrail on a long text to many sessions")
	asJSON := fs.Bool("json", false, "print the outcome as JSON (the new message's id, the one it replaces and the report)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	words, err := trailingFlags(fs, s)
	if err != nil {
		return err
	}
	if len(words) == 0 {
		return outbox.Usagef("%s", usage)
	}
	id, text := words[0], strings.Join(words[1:], " ")
	if strings.TrimSpace(id) == "" { // `amend "$ID" text` with $ID unset must not pick anything
		return outbox.Usagef("%s: the id is empty", usage)
	}
	if text == "" || text == "-" {
		b, _ := io.ReadAll(s.In)
		text = strings.TrimSpace(string(b))
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	c.Labels = identity.Labels("")
	// An abbreviation is looked up only among what this machine sent. No confirmation is asked, as
	// retract does: an amendment is undone by retracting it.
	if outbox.IsAbbreviation(id) && len(strings.TrimSpace(id)) < outbox.MinRetractPrefix {
		return outbox.Usagef("%q is too short to name a message: give at least %d characters or the full id (`aivoicebus inbox --all` lists ids)", strings.TrimSpace(id), outbox.MinRetractPrefix)
	}
	ids := outbox.NewIDResolver(c, identity.Me())
	ids.Say = s.Err
	if id, err = ids.ExpandSent(id, outbox.MinRetractPrefix); err != nil {
		return err
	}
	if !*asJSON {
		_, err = outbox.Amend(c, id, text, outbox.AmendOptions{DryRun: *dry, Force: *force}, s.Out)
		return err
	}
	var report strings.Builder
	out, err := outbox.Amend(c, id, text, outbox.AmendOptions{DryRun: *dry, Force: *force}, &report)
	if err != nil {
		return err // its kind (not found, usage, ...) decides the exit code, so it is not re-wrapped with the report
	}
	return writeJSON(s.Out, struct {
		ID      string `json:"id,omitempty"`
		Amended string `json:"amended"`
		Thread  string `json:"thread,omitempty"`
		DryRun  bool   `json:"dry_run"`
		Report  string `json:"report"`
	}{out.ID, strings.ToLower(strings.TrimSpace(id)), out.Thread, *dry, strings.TrimSpace(report.String())})
}
