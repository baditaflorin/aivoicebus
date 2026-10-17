package agentcfg

import (
	"os"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/assets"
)

// Claude Code's /aivoicebus-standby command: a file in ~/.claude/commands that tells the session to
// wait for messages in the background and wake on one. It carries a marker, so a file of the
// user's own by that name is never overwritten or removed.

const standbyMarker = "<!-- aivoicebus:managed"

func (p Paths) standbyCommand() string { return p.join(".claude", "commands", "aivoicebus-standby.md") }

func (in *installer) claudeStandby() {
	const what = "claude standby"
	if !in.wants("claude") {
		return
	}
	if _, err := os.Stat(in.p.join(".claude")); err != nil {
		in.add(what, Skipped, "no ~/.claude (Claude Code not installed)")
		return
	}
	path := in.p.standbyCommand()
	want := assets.StandbyClaude()
	have, exists := readFile(path)
	switch {
	case exists && have == want:
		in.add(what, OK, "/aivoicebus-standby in "+path)
	case exists && !strings.Contains(have, standbyMarker):
		in.add(what, Skipped, path+" is a file of your own; left alone")
	default:
		if err := in.write(path, want, 0o755); err != nil {
			in.add(what, Failed, err.Error())
			return
		}
		in.add(what, Changed, "/aivoicebus-standby in "+path+" (tell a session to stand by for messages)")
	}
}

func (in *installer) removeClaudeStandby() {
	const what = "claude standby"
	path := in.p.standbyCommand()
	have, exists := readFile(path)
	switch {
	case !exists:
		return
	case !strings.Contains(have, standbyMarker):
		in.add(what, Skipped, path+" is a file of your own; left alone")
	default:
		if !in.o.DryRun {
			if err := os.Remove(path); err != nil {
				in.add(what, Failed, err.Error())
				return
			}
		}
		in.add(what, Changed, "removed "+path)
	}
}
