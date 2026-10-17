// Package worker runs a command for tasks sent to this machine while no agent session is open:
// `aivoicebus worker --exec '...'`. It takes a task only from a sender the user allowed, runs the
// command once with the task text on stdin, and answers with what the command printed.
//
// This runs an agent on input from a peer. Everything here exists to keep that narrow: an empty
// allowlist (nothing runs until the user names a sender), one run at a time, a time limit, an
// output cap, the credential check on everything that goes back, and a local log of every run.
package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// AllowFile is the allowlist's name in the config directory (~/.config/aivoicebus).
const AllowFile = "worker-allow"

// Allowlist is the senders whose tasks the worker may run. An entry is a machine (laptop-b: any
// session there) or a session or tool on one (laptop-b/claude: that tool's sessions,
// laptop-b/claude.a1b2: that one session). Senders are self-declared, so an entry is only as strong
// as the token the sender holds: see docs/reference.md.
type Allowlist struct{ entries []string }

// Empty reports whether nobody is allowed.
func (a Allowlist) Empty() bool { return len(a.entries) == 0 }

// Entries lists the entries in file order.
func (a Allowlist) Entries() []string { return slices.Clone(a.entries) }

// Allows reports whether from may hand this worker a task, and the entry that lets it.
func (a Allowlist) Allows(from string) (string, bool) {
	from = strings.ToLower(from)
	if !msg.ValidIdentity(from) || !strings.Contains(from, "/") {
		return "", false
	}
	for _, e := range a.entries {
		if strings.Contains(e, "/") {
			if from == e || strings.HasPrefix(from, e+".") {
				return e, true
			}
			continue
		}
		if msg.HostOf(from) == e {
			return e, true
		}
	}
	return "", false
}

// CheckEntry refuses what cannot be an allowlist entry: anything but lower-case letters, digits
// and . _ - with at most one /, an empty part, and every wildcard or broadcast name (all, *, @...),
// so no entry can stand for "everyone".
func CheckEntry(e string) error {
	if e == "" || len(e) > msg.MaxIdentity {
		return errors.New("an entry is a machine (laptop-b) or a session on one (laptop-b/claude)")
	}
	for _, r := range e {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/", r)) {
			return fmt.Errorf("%q: an entry holds only lower-case letters, digits and . _ - / (no wildcards)", e)
		}
	}
	host, agent, hasAgent := strings.Cut(e, "/")
	switch {
	case host == "" || hasAgent && (agent == "" || strings.Contains(agent, "/")):
		return fmt.Errorf("%q: write a machine (laptop-b) or machine/session (laptop-b/claude)", e)
	case host == "all" || host == msg.SystemHost:
		return fmt.Errorf("%q names everyone or the nodes themselves, not a sender", e)
	}
	return nil
}

// ParseAllow reads the allowlist file's text: one entry per line, # starts a comment. Lines that
// are not entries are returned as problems and allow nobody.
func ParseAllow(text string) (Allowlist, []string) {
	var a Allowlist
	var bad []string
	for _, l := range strings.Split(text, "\n") {
		l, _, _ = strings.Cut(l, "#")
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "" {
			continue
		}
		if err := CheckEntry(l); err != nil {
			bad = append(bad, err.Error())
			continue
		}
		if !slices.Contains(a.entries, l) {
			a.entries = append(a.entries, l)
		}
	}
	return a, bad
}

// AllowPath is where the allowlist lives.
func AllowPath() string {
	if d := config.Dir(); d != "" {
		return filepath.Join(d, AllowFile)
	}
	return ""
}

// LoadAllow reads the allowlist. A missing file is an empty list. A file that others can read or
// write (a looser mode than 0600), a symlink, or a line that is not an entry is an error: the
// worker must not start on a list it cannot trust.
func LoadAllow() (Allowlist, error) {
	p := AllowPath()
	if p == "" {
		return Allowlist{}, errors.New("no home directory")
	}
	if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o077 != 0 {
		return Allowlist{}, fmt.Errorf("%s is accessible to other users (mode %04o); chmod 600 it", p, fi.Mode().Perm())
	}
	text, err := config.ReadSmall(p)
	if err != nil {
		return Allowlist{}, err
	}
	a, bad := ParseAllow(text)
	if len(bad) > 0 {
		return Allowlist{}, fmt.Errorf("%s: %s", p, strings.Join(bad, "; "))
	}
	return a, nil
}

// AddAllow adds an entry (written with mode 0600).
func AddAllow(e string) error {
	e = strings.ToLower(strings.TrimSpace(e))
	if err := CheckEntry(e); err != nil {
		return err
	}
	return config.UpdateSmall(AllowFile, func(old string) (string, error) {
		a, _ := ParseAllow(old)
		if slices.Contains(a.entries, e) {
			return old, nil
		}
		if old != "" && !strings.HasSuffix(old, "\n") {
			old += "\n"
		}
		if old == "" {
			old = "# Senders whose tasks `aivoicebus worker` may run (a machine, or machine/session). One per line.\n"
		}
		return old + e + "\n", nil
	})
}

// RemoveAllow removes an entry; false when it was not there.
func RemoveAllow(e string) (bool, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	found := false
	err := config.UpdateSmall(AllowFile, func(old string) (string, error) {
		var keep []string
		for _, l := range strings.SplitAfter(old, "\n") {
			entry, _, _ := strings.Cut(l, "#")
			if strings.ToLower(strings.TrimSpace(entry)) == e {
				found = true
				continue
			}
			keep = append(keep, l)
		}
		return strings.Join(keep, ""), nil
	})
	return found, err
}
