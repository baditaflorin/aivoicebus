package identity

import (
	"fmt"
	"slices"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Long-lived agent names. An automatic identity (claude.a1b2) lasts one session: restart the tool
// and it is someone new, so a role or a person cannot keep talking to "the codex that does
// builds". A name chosen once, registered on this machine and asked for with AIVOICEBUS_NAME
// (codex.build1) is the same identity after every restart, and cannot be an automatic one: its
// label is at least msg.MinPersistentSuffix characters and every automatic suffix is at most four.
//
// Opt-in twice, whatever the name's tool (a name for no known tool, mybot.nightly, needs the
// registry as much as codex.build1): the name must be in this machine's registry (~/.config/aivoicebus/names, written by
// `aivoicebus name add`) and the environment must ask for it, so a stray variable or a name
// someone else's tool inherited names nothing. It never overrides AIVOICEBUS_ME or AIVOICEBUS_AGENT,
// and it is ignored (with the reason in `whoami --why`) when it names a different tool than the
// one running. The registry only says which names this machine may use: one name should be worn by
// one live session at a time, as with any AIVOICEBUS_AGENT.

// nameRegistered says whether name is in this machine's registry; tests replace it.
var nameRegistered = func(name string) bool { return slices.Contains(RegisteredNames(), name) }

// namesFile is the registry's name in the config directory.
const namesFile = "names"

// ParseNames reads a registry: one name per line, "#" starts a comment. Lines that are not valid
// persistent names are skipped, so a hand-edit typo does not hide the other names.
func ParseNames(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		name := strings.ToLower(strings.TrimSpace(line))
		if msg.ValidPersistentName(name) == nil && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// RegisteredNames are the names this machine has registered, in file order. A registry that cannot
// be read safely (a symlink, over 64 KiB) registers nothing.
func RegisteredNames() []string {
	d := config.Dir()
	if d == "" {
		return nil
	}
	text, err := config.ReadSmall(d + "/" + namesFile)
	if err != nil {
		return nil
	}
	return ParseNames(text)
}

func renderNames(names []string) string {
	text := "# long-lived agent names this machine may use (aivoicebus name add); one per line\n"
	if len(names) > 0 {
		text += strings.Join(names, "\n") + "\n"
	}
	return text
}

// AddName registers name on this machine. Registering a name twice is not an error.
func AddName(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := msg.ValidPersistentName(name); err != nil {
		return err
	}
	return config.UpdateSmall(namesFile, func(old string) (string, error) {
		names := ParseNames(old)
		if slices.Contains(names, name) {
			return old, nil
		}
		return renderNames(append(names, name)), nil
	})
}

// RemoveName drops name from the registry; a session still asking for it falls back to its
// automatic identity.
func RemoveName(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	return config.UpdateSmall(namesFile, func(old string) (string, error) {
		names := ParseNames(old)
		i := slices.Index(names, name)
		if i < 0 {
			return old, fmt.Errorf("%q is not registered on this machine", name)
		}
		return renderNames(slices.Delete(names, i, i+1)), nil
	})
}

// namedDetection is the long-lived name AIVOICEBUS_NAME asks for, as a detection, and why it
// is or is not used. implicit is what detection found without it: a name for another tool than
// the one running is refused, so a Codex name cannot label a Claude session.
func namedDetection(getenv func(string) string, implicit Detection) (Detection, string) {
	v := getenv("AIVOICEBUS_NAME")
	if v == "" {
		return Detection{}, ""
	}
	name := strings.ToLower(strings.TrimSpace(v))
	switch {
	case msg.ValidPersistentName(name) != nil:
		return Detection{}, "AIVOICEBUS_NAME=" + clean(v) + " ignored: " + clean(msg.ValidPersistentName(name).Error())
	case !nameRegistered(name):
		return Detection{}, "AIVOICEBUS_NAME=" + clean(v) + " ignored: not registered on this machine (aivoicebus name add " + clean(name) + ")"
	case implicit.Tool != "" && implicit.Tool != ToolOf(name):
		return Detection{}, "AIVOICEBUS_NAME=" + clean(v) + " ignored: it names a different tool than the " + implicit.Tool + " running here"
	}
	return Detection{Name: name, Tool: ToolOf(name), Session: sessionOf(name), Source: "AIVOICEBUS_NAME=" + name + " (a long-lived name registered on this machine)"}, ""
}

// NamedAgent is the registered long-lived name AIVOICEBUS_NAME asks for when it belongs to tool
// (the hooks know their tool by name, not from a process table), else "".
func NamedAgent(tool string) string {
	d, _ := namedDetection(osGetenv, Detection{Tool: tool})
	return d.Name
}
