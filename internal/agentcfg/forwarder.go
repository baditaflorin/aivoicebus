package agentcfg

import (
	"os"
	"strings"
)

// The webhook forwarder (`aivoicebus forward --webhook`) wakes an agent that cannot run hooks, Grok
// Bot first. Unlike the hooks it is not part of "every tool that is installed": `setup --tools grokbot`
// asks for it by name. It writes a small script and one line in the shell's rc file that starts the
// forwarder when a shell opens and it is not running (a box without systemd has nothing else to do it).
// It sends nothing and holds no webhook: the URL and key come from the environment or their own files.

// ForwarderTool is the name `--tools` takes.
const ForwarderTool = "grokbot"

const (
	forwarderScriptName = "ensure-aivoicebus-forward"
	forwarderMarker     = "# aivoicebus forwarder"
	forwarderScript     = `#!/bin/sh
# Starts the aivoicebus webhook forwarder if it is not running (written by aivoicebus setup --tools grokbot).
[ -x "$HOME/.local/bin/aivoicebus" ] || exit 0
[ -f "$HOME/.config/aivoicebus/env" ] && . "$HOME/.config/aivoicebus/env"
exec "$HOME/.local/bin/aivoicebus" forward ensure >/dev/null 2>&1
`
)

func (p Paths) Bashrc() string          { return p.join(".bashrc") }
func (p Paths) ForwarderScript() string { return p.join(".local", "bin", forwarderScriptName) }

func forwarderLine() string {
	return `"$HOME/.local/bin/` + forwarderScriptName + `" 2>/dev/null   ` + forwarderMarker
}

// explicit reports whether tool was asked for by name (an empty list means "every installed tool",
// which does not include the forwarder).
func (in *installer) explicit(tool string) bool {
	for _, t := range in.o.Tools {
		if t == tool {
			return true
		}
	}
	return false
}

// rcFile is the shell rc file the line goes in: ~/.bashrc, else ~/.zshrc when only that exists.
func (in *installer) rcFile() string {
	if _, err := os.Stat(in.p.Bashrc()); err != nil {
		if _, err := os.Stat(in.p.Zshrc()); err == nil {
			return in.p.Zshrc()
		}
	}
	return in.p.Bashrc()
}

func (in *installer) forwarder() {
	if !in.explicit(ForwarderTool) {
		return
	}
	script := in.p.ForwarderScript()
	if cur, ok := readFile(script); ok && cur == forwarderScript {
		in.add("forwarder script", OK, script)
	} else {
		if !in.o.DryRun {
			if err := installFile(script, []byte(forwarderScript)); err != nil {
				in.add("forwarder script", Failed, err.Error())
				return
			}
		}
		in.add("forwarder script", Changed, script)
	}
	rc := in.rcFile()
	cur, _ := readFile(rc)
	if strings.Contains(cur, forwarderMarker) {
		in.add("forwarder start", OK, rc)
		return
	}
	next := cur
	if next != "" && !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	next += forwarderLine() + "\n"
	if !in.o.DryRun {
		in.backup(rc)
		if err := os.WriteFile(rc, []byte(next), 0o644); err != nil {
			in.add("forwarder start", Failed, err.Error())
			return
		}
	}
	in.add("forwarder start", Changed, rc+" (starts the forwarder when a shell opens, once AIVOICEBUS_WEBHOOK_URL is set; see `aivoicebus forward status`)")
}

func (in *installer) removeForwarder() {
	if len(in.o.Tools) > 0 && !in.explicit(ForwarderTool) {
		return
	}
	script := in.p.ForwarderScript()
	if cur, ok := readFile(script); ok && cur == forwarderScript { // only the file setup wrote
		if !in.o.DryRun {
			_ = os.Remove(script)
		}
		in.add("forwarder script", Changed, script)
	}
	for _, rc := range []string{in.p.Bashrc(), in.p.Zshrc()} {
		cur, ok := readFile(rc)
		if !ok || !strings.Contains(cur, forwarderMarker) {
			continue
		}
		var kept []string
		for _, l := range strings.Split(cur, "\n") {
			if !strings.Contains(l, forwarderMarker) {
				kept = append(kept, l)
			}
		}
		in.backup(rc)
		if !in.o.DryRun {
			if err := os.WriteFile(rc, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
				in.add("forwarder start", Failed, err.Error())
				continue
			}
		}
		in.add("forwarder start", Changed, rc)
	}
}
