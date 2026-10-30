// Package wants decides, on the machine that reads a message and with no network and no model,
// whether the condition its sender gave has come true (msg.Wants): then the message is settled and
// costs its readers nothing. The conditions are a fixed list (msg.ParseCond); each reads only local
// state, never a file's contents, and nothing from a message is ever run or put in a command line.
package wants

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/version"
)

// ProbeTimeout bounds the one local probe a service condition makes.
const ProbeTimeout = 400 * time.Millisecond

// Env is the local state the conditions read. Local is this machine's; tests fill in their own.
type Env struct {
	Release string                  // the installed release ("dev" for a build from source: no release condition is met)
	Facts   func() msg.Facts        // what this machine publishes (msg.FactsOf of its labels)
	Home    string                  // the home directory; "" meets no file condition
	Exists  func(path string) bool  // whether a path exists (never opened, never read)
	Loaded  func(label string) bool // whether a launchd or systemd unit is loaded
}

// Local is this machine's environment, worked out when used.
func Local() Env {
	home, _ := os.UserHomeDir()
	return Env{
		Release: version.Release,
		Facts: func() msg.Facts {
			if identity.FactsOff() {
				return msg.Facts{}
			}
			has, ssh := identity.MachineFacts()
			return msg.FactsOf(msg.Labels{msg.HasLabel: has, msg.SSHLabel: ssh})
		},
		Home:   home,
		Exists: func(p string) bool { _, err := os.Lstat(p); return err == nil },
		Loaded: unitLoaded,
	}
}

// Met reports whether c is true here. A condition that cannot be worked out is not met.
func (e Env) Met(c msg.Cond) bool {
	switch c.Kind {
	case msg.CondRelease:
		return release.ValidVersion(e.Release) && release.ValidVersion(c.Arg) && release.Compare(e.Release, c.Arg) >= 0
	case msg.CondHas:
		q, err := msg.ParseFactQuery(c.Arg)
		if err != nil || e.Facts == nil {
			return false
		}
		f := e.Facts()
		if !f.Known {
			return false
		}
		ok, _ := q.Match(f)
		return ok
	case msg.CondFile:
		p, ok := e.path(c.Arg)
		return ok && e.Exists != nil && e.Exists(p)
	case msg.CondService:
		return e.Loaded != nil && e.Loaded(c.Arg)
	}
	return false
}

// sensitive are the directories under the home directory whose contents (and so whose names) are
// never a condition: a sender must not learn what is in them from whether its message settled.
var sensitive = []string{".ssh", ".gnupg", ".aws", ".kube", ".docker", ".netrc", ".config/gcloud", ".config/gh", ".config/aivoicebus", "Library/Keychains", ".password-store"}

// path turns ~/PATH into a path under the home directory, or says it is none: a path that leaves it
// (after symlinks), or one in a sensitive directory, is not a condition.
func (e Env) path(arg string) (string, bool) {
	rest, ok := strings.CutPrefix(arg, "~/")
	if !ok || e.Home == "" || rest == "" {
		return "", false
	}
	home := filepath.Clean(e.Home)
	p := filepath.Join(home, filepath.FromSlash(rest))
	rel, err := filepath.Rel(home, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	for _, s := range sensitive {
		if rel == s || strings.HasPrefix(rel, s+string(filepath.Separator)) {
			return "", false
		}
	}
	if real, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		rh, herr := filepath.EvalSymlinks(home)
		if herr != nil {
			rh = home
		}
		if r, err := filepath.Rel(rh, real); err != nil || strings.HasPrefix(r, "..") {
			return "", false
		}
	}
	return p, true
}

// unitLoaded asks the machine's service manager, with a fixed command and the label as one argument
// (msg.ParseCond only lets a label start with a letter or digit), within ProbeTimeout.
func unitLoaded(label string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		return exec.CommandContext(ctx, "/bin/launchctl", "list", label).Run() == nil
	case "linux":
		for _, args := range [][]string{{"show", "-p", "LoadState", "--value", label}, {"--user", "show", "-p", "LoadState", "--value", label}} {
			out, err := exec.CommandContext(ctx, "/usr/bin/systemctl", args...).Output()
			if err == nil && strings.TrimSpace(string(out)) == "loaded" {
				return true
			}
		}
	}
	return false
}

// Met is one message whose condition is true.
type Met struct {
	Msg  msg.Message
	Cond msg.Cond
}

// Split takes the messages whose done-when is met here out of ms: what is left, and what was met. It
// also leaves out a message another one in ms replaces (msg.Replaces), which a node that applies keys
// and supersedes has already done and an older one has not. Never settles a message that must be
// answered (msg.Settleable), whatever it says.
func Split(e Env, ms []msg.Message) (left []msg.Message, met []Met) {
	left, met = split(e, ms)
	return dropReplaced(left), met
}

// dropReplaced leaves out the messages in ms that a later one in ms replaces.
func dropReplaced(ms []msg.Message) []msg.Message {
	var bs []msg.Message
	for _, m := range ms {
		if m.Retracted == nil && (strings.Contains(m.Body, "\n[supersedes: ") || strings.Contains(m.Body, "\n[key: ")) {
			bs = append(bs, m)
		}
	}
	if len(bs) == 0 {
		return ms
	}
	out := make([]msg.Message, 0, len(ms))
next:
	for _, m := range ms {
		for _, b := range bs {
			if msg.Replaces(b, m) {
				continue next
			}
		}
		out = append(out, m)
	}
	return out
}

func split(e Env, ms []msg.Message) (left []msg.Message, met []Met) {
	for _, m := range ms {
		if w := msg.WantsOf(m); w.DoneWhen != "" && msg.Settleable(m) {
			if c, err := msg.ParseCond(w.DoneWhen); err == nil && e.Met(c) {
				met = append(met, Met{m, c})
				continue
			}
		}
		left = append(left, m)
	}
	return left, met
}
