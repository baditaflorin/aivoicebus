package msg

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Wants: what a sender says about when its message stops mattering (docs/stigmergy.md).
//
// A message may end with up to three lines, each in square brackets, that say so in words a program
// reads and nobody has to:
//
//	[done-when: release>=0.15.0]   the receiving machine settles the message once the condition is true
//	[key: update-0own]             a later message with this key from this machine to this target replaces it
//	[supersedes: 3f2a9c01de]       this message replaces that one, which this machine sent
//
// They are body text, so every node and every client of every release carries them untouched; one
// that predates them shows the lines as they are and settles nothing. They are never run: a done-when
// is parsed into one of the four conditions below, from a fixed list, and anything else is no
// condition (the line stays text, and `send --done-when` refuses it at the sender).

// MaxWantLine is the longest value a trailer line may carry.
const MaxWantLine = 200

// Condition kinds, the whole whitelist.
const (
	CondRelease = "release" // release>=X.Y.Z: this machine's installed release is X.Y.Z or later
	CondHas     = "has"     // has:KEY: a fact this machine publishes (msg.ParseFactQuery)
	CondFile    = "file"    // file:~/PATH: the path exists, under the home directory; never read
	CondService = "service" // service:LABEL: a launchd or systemd unit is loaded here
)

// Cond is one parsed condition. Arg is the release, the fact, the path (as written, with its ~/) or the unit.
type Cond struct {
	Kind, Arg string
}

// String is the condition as it is written in a trailer and after --done-when.
func (c Cond) String() string {
	if c.Kind == CondRelease {
		return "release>=" + c.Arg
	}
	return c.Kind + ":" + c.Arg
}

var serviceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,99}$`)

// ParseCond reads a condition. The error says what is allowed.
func ParseCond(s string) (Cond, error) {
	raw := s
	s = strings.TrimSpace(s)
	bad := func(why string) (Cond, error) {
		return Cond{}, fmt.Errorf("--done-when %q: %s; the conditions are release>=X.Y.Z, has:FACT (as `who --has`), file:~/PATH and service:LABEL", raw, why)
	}
	if len(s) > MaxWantLine || hasControl(s) {
		return bad("too long, or it holds a control character")
	}
	if v, ok := strings.CutPrefix(s, "release>="); ok {
		if !ValidRelease(v) || strings.Contains(v, "-") {
			return bad("a release is three numbers, such as 0.15.0")
		}
		return Cond{CondRelease, v}, nil
	}
	kind, arg, ok := strings.Cut(s, ":")
	if !ok || arg == "" {
		return bad("not one of them")
	}
	switch strings.ToLower(kind) {
	case CondHas:
		if _, err := ParseFactQuery(arg); err != nil {
			return bad("not a fact this client knows")
		}
		return Cond{CondHas, strings.ToLower(arg)}, nil
	case CondFile:
		if !strings.HasPrefix(arg, "~/") || len(arg) < 3 {
			return bad("a file is written ~/PATH, under the home directory of whichever machine reads it")
		}
		for _, seg := range strings.Split(arg[2:], "/") {
			if seg == ".." || seg == "" {
				return bad("a path has no .. and no empty part")
			}
		}
		return Cond{CondFile, arg}, nil
	case CondService:
		if !serviceRe.MatchString(arg) {
			return bad("a service label is letters, digits and . _ @ - only")
		}
		return Cond{CondService, arg}, nil
	}
	return bad("not one of them")
}

// Wants is the trailer of a message body.
type Wants struct {
	DoneWhen   string // a condition, as ParseCond accepts it
	Key        string // names the message's purpose; see ValidWantKey
	Supersedes string // the id of a message this one replaces
}

// Empty reports whether w says nothing.
func (w Wants) Empty() bool { return w == Wants{} }

var wantKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,63}$`)

// ValidWantKey reports whether k can be a key.
func ValidWantKey(k string) bool { return wantKeyRe.MatchString(k) }

// Validate checks every part of w, with errors in the words of the flags.
func (w Wants) Validate() error {
	if w.DoneWhen != "" {
		if _, err := ParseCond(w.DoneWhen); err != nil {
			return err
		}
	}
	if w.Key != "" && !ValidWantKey(w.Key) {
		return errors.New("--key is letters, digits and . _ : @ - only, at most 64 characters")
	}
	if w.Supersedes != "" && !ValidID(w.Supersedes) {
		return errors.New("--supersedes takes a message id (ten lowercase hex digits), as `aivoicebus inbox` shows it")
	}
	return nil
}

var wantLineRe = regexp.MustCompile(`^\[(done-when|key|supersedes): ([^\[\]\n]{1,200})\]$`)

// SplitWants takes the trailer off a body: the text without it, and what it said. A line that is
// not a well-formed want (a condition that is not on the list, a key or id of the wrong shape) ends
// the trailer and stays text, as does a want named twice.
func SplitWants(body string) (string, Wants) {
	var w Wants
	lines := strings.Split(strings.TrimRight(body, " \t\r\n"), "\n")
	for len(lines) > 1 {
		g := wantLineRe.FindStringSubmatch(strings.TrimSpace(lines[len(lines)-1]))
		if g == nil {
			break
		}
		val := g[2]
		switch g[1] {
		case "done-when":
			c, err := ParseCond(val)
			if err != nil || w.DoneWhen != "" {
				return strings.TrimRight(strings.Join(lines, "\n"), " \t\r\n"), w
			}
			w.DoneWhen = c.String()
		case "key":
			if !ValidWantKey(val) || w.Key != "" {
				return strings.TrimRight(strings.Join(lines, "\n"), " \t\r\n"), w
			}
			w.Key = val
		case "supersedes":
			if !ValidID(val) || w.Supersedes != "" {
				return strings.TrimRight(strings.Join(lines, "\n"), " \t\r\n"), w
			}
			w.Supersedes = val
		}
		lines = lines[:len(lines)-1]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \t\r\n"), w
}

// WantsOf is what m's body says it wants.
func WantsOf(m Message) Wants {
	_, w := SplitWants(m.Body)
	return w
}

// AppendWants writes the trailer after body: one line each, done-when, key, supersedes.
func AppendWants(body string, w Wants) string {
	body = strings.TrimRight(body, " \t\r\n")
	if c, err := ParseCond(w.DoneWhen); err == nil && w.DoneWhen != "" {
		body += "\n[done-when: " + c.String() + "]"
	}
	if w.Key != "" {
		body += "\n[key: " + w.Key + "]"
	}
	if w.Supersedes != "" {
		body += "\n[supersedes: " + w.Supersedes + "]"
	}
	return body
}

// FeatureWants is what a node advertises in Health.Features when it applies keys and supersedes
// (a done-when is applied by the machine that reads the message, not by a node).
const FeatureWants = "wants"

// WantsSince is the release whose nodes apply keys and supersedes (and whose hooks apply a done-when).
const WantsSince = "0.15.0"

// KeyIdent names the purpose a keyed message serves: the machine that sent it, the target and the key.
// A later message with the same KeyIdent replaces an earlier one. The sender's machine, not its exact
// session, so a restarted session can take over what an earlier one asked.
func KeyIdent(m Message, key string) string { return HostOf(m.From) + "|" + m.To + "|" + key }

// Replaces reports whether the later message b replaces m by what b says of itself: b names m in
// [supersedes: ID], or both carry the same [key: K] to the same target; they come from the same
// machine, and m is not one that must be answered. Replaced means no session that has not read m is
// shown it; m stays in the log, in `inbox --all` and in `thread`. It says nothing of retractions: a
// node takes b back out of it (a retracted b replaces nothing).
func Replaces(b, m Message) bool {
	if b.ID == m.ID || !Settleable(m) || HostOf(b.From) != HostOf(m.From) || !later(b, m) {
		return false
	}
	wb := WantsOf(b)
	if wb.Supersedes == m.ID {
		return true
	}
	return wb.Key != "" && b.To == m.To && WantsOf(m).Key == wb.Key
}

// later reports whether b came after m: by time, then by id.
func later(b, m Message) bool {
	return b.At.After(m.At) || (b.At.Equal(m.At) && b.ID > m.ID)
}
