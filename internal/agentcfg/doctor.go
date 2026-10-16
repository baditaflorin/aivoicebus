package agentcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/version"
)

// ErrTokenRejected is what a Mesh probe wraps (errors.Is) when a node refused this
// machine's token, so Doctor can tell "refused" from "nobody answered".
var ErrTokenRejected = errors.New("the mesh rejected this machine's token")

// Check levels.
const (
	LevelOK   = "ok"
	LevelWarn = "warn"
	LevelFail = "fail"
)

// Check is one finding of Doctor.
type Check struct{ Name, Level, Detail string }

// DoctorOptions configure Doctor.
type DoctorOptions struct {
	Home     string           // default: the user's home
	Identity string           // what `whoami` prints, shown for reference
	Exe      string           // the running binary; default os.Executable()
	Mesh     func() error     // reachability probe; nil skips the check (offline); wrap ErrTokenRejected for a refused token
	Now      func() time.Time // default time.Now
}

// Doctor compares this machine with what this binary would set up. It reads
// and reports; `Setup` is what fixes. It never reads the token, only its mode.
// A mesh that refuses the token is a failure (the machine receives nothing); one
// that does not answer is only a warning, since the network may simply be away.
func Doctor(o DoctorOptions) []Check {
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	p := Paths{o.Home}
	var cs []Check
	add := func(name, level, detail string) { cs = append(cs, Check{name, level, detail}) }

	add("binary", LevelOK, version.String())
	cs = append(cs, binaryCheck(p, o.Exe))
	if o.Identity != "" {
		add("identity", LevelOK, o.Identity)
	}

	// node URLs
	envText, _ := readFile(p.Env())
	switch {
	case hasLine(envText, "export AIVOICEBUS_URL="):
		add("env", LevelOK, p.Env())
	case os.Getenv("AIVOICEBUS_URL") != "":
		add("env", LevelWarn, "AIVOICEBUS_URL is set in this shell only; run `aivoicebus setup` to persist it")
	default:
		add("env", LevelFail, "no node URL configured: run `aivoicebus setup --url http://node1:7777,http://node2:7777`")
	}
	if text, _ := readFile(p.Zshrc()); strings.Contains(text, "aivoicebus/env") {
		add("shell", LevelOK, p.Zshrc())
	} else {
		add("shell", LevelWarn, "~/.zshrc does not source the env file (the hook still works; a plain shell lacks PATH)")
	}

	// token: mode and size only
	switch fi, err := os.Stat(p.Token()); {
	case err != nil && os.Getenv("AIVOICEBUS_TOKEN") != "":
		add("token", LevelOK, "from AIVOICEBUS_TOKEN")
	case err != nil:
		add("token", LevelFail, "no token at "+p.Token())
	case fi.Mode().Perm()&0o077 != 0:
		add("token", LevelFail, fmt.Sprintf("mode %04o lets other users read it; `aivoicebus doctor --fix` sets 0600", fi.Mode().Perm()))
	case fi.Size() < 24:
		add("token", LevelFail, "token file is shorter than 24 characters")
	default:
		add("token", LevelOK, fmt.Sprintf("%s (mode %04o)", p.Token(), fi.Mode().Perm()))
	}

	// Claude Code hook
	if raw, err := os.ReadFile(p.Settings()); err != nil {
		add("claude hook", LevelFail, "no "+p.Settings())
	} else {
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			add("claude hook", LevelFail, "settings.json is not valid JSON")
		} else if st := hookStatus(doc); st == "current" {
			add("claude hook", LevelOK, "session-start and per-prompt hooks "+HookID())
		} else {
			add("claude hook", LevelFail, "the hooks are "+st+"; `aivoicebus doctor --fix` installs the current ones")
		}
	}

	// what the hooks last found when they asked the mesh (they are silent, so this is the
	// only trace a deaf machine leaves)
	if st, ok := config.LoadHookStatus(p.HookStatus()); ok {
		cs = append(cs, hookRunCheck(st, o.Now()))
	}

	// instruction blocks, for tools that are installed
	for _, t := range Tools {
		if _, err := os.Stat(p.ToolDir(t)); err != nil && t.Name != "claude" {
			continue
		}
		text, _ := readFile(p.Instructions(t))
		if st := blockStatus(text); st == "current" {
			add(t.Name+" instructions", LevelOK, "snippet "+SnippetID())
		} else {
			add(t.Name+" instructions", LevelFail, "snippet is "+st+" in "+p.Instructions(t))
		}
	}

	// what was last applied
	var have State
	if b, err := os.ReadFile(p.State()); err == nil {
		_ = json.Unmarshal(b, &have)
	}
	switch want := Current(); {
	case have == State{}:
		add("state", LevelWarn, "setup has not run on this machine")
	case have.Hook != want.Hook || have.Snippet != want.Snippet:
		add("state", LevelWarn, fmt.Sprintf("applied by release %s; this binary is %s", have.Release, want.Release))
	default:
		add("state", LevelOK, "applied by release "+have.Release)
	}

	if o.Mesh != nil {
		switch err := o.Mesh(); {
		case errors.Is(err, ErrTokenRejected):
			hint := "ask whoever runs the mesh for the current token and save it as " + p.Token() + " (mode 0600)"
			if os.Getenv("AIVOICEBUS_TOKEN") != "" {
				hint += "; AIVOICEBUS_TOKEN is set in this shell and takes precedence"
			}
			add("mesh", LevelFail, err.Error()+"; "+hint)
		case err != nil:
			add("mesh", LevelWarn, "unreachable: "+err.Error())
		default:
			add("mesh", LevelOK, "reachable")
		}
	}
	return cs
}

// hookRunCheck reports the hooks' own record. It only warns: whether the mesh accepts the
// token now is the live probe's call (a record of a refusal may predate a fix, and the
// next hook run clears it).
func hookRunCheck(st config.HookStatus, now time.Time) Check {
	const name = "hook runs"
	switch st.Outcome {
	case config.HookOK:
		d := "last run ok"
		if now.Sub(st.LastRun) >= 24*time.Hour {
			d += ", " + ago(now, st.LastRun)
		}
		return Check{name, LevelOK, d}
	case config.HookRejected:
		return Check{name, LevelWarn, fmt.Sprintf("the last run (%s) was refused by the mesh%s; nothing is received until the token is fixed; last success %s",
			ago(now, st.LastRun), streak(st.Failures), ago(now, st.LastOK))}
	case config.HookUnreachable:
		return Check{name, LevelWarn, fmt.Sprintf("the last run (%s) reached no node%s; last success %s",
			ago(now, st.LastRun), streak(st.Failures), ago(now, st.LastOK))}
	}
	return Check{name, LevelWarn, fmt.Sprintf("unknown outcome %q in %s", st.Outcome, config.HookStatusFile)}
}

// streak says how many runs in a row failed, when it is more than the one being reported.
func streak(n int) string {
	if n <= 1 {
		return ""
	}
	return fmt.Sprintf(", %d failed in a row", n)
}

// ago renders how long before now t was, coarsely ("never" for the zero time).
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case t.IsZero():
		return "never"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func binaryCheck(p Paths, exe string) Check {
	if exe == "" {
		exe, _ = os.Executable()
	}
	want := p.Binary()
	if err := Installed(want); err != nil {
		if _, serr := os.Stat(want); serr != nil {
			return Check{"install", LevelFail, "no binary at " + want + " (the hook runs it from there)"}
		}
		return Check{"install", LevelFail, "no usable binary at " + want + " (" + err.Error() + "; the hook runs it from there)"}
	}
	resolve := func(path string) string {
		if r, err := filepath.EvalSymlinks(path); err == nil {
			return r
		}
		return filepath.Clean(path)
	}
	if resolve(exe) != resolve(want) {
		return Check{"install", LevelWarn, "running " + exe + "; the installed copy is " + want}
	}
	return Check{"install", LevelOK, want}
}

// HasFailures reports whether any check failed (warnings do not count).
func HasFailures(cs []Check) bool {
	for _, c := range cs {
		if c.Level == LevelFail {
			return true
		}
	}
	return false
}
