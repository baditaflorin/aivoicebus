package agentcfg

import (
	"errors"
	"strings"
)

func commandOf(h any) string {
	m, _ := h.(map[string]any)
	s, _ := m["command"].(string)
	return s
}

func hooksOf(entry any) []any {
	m, _ := entry.(map[string]any)
	hs, _ := m["hooks"].([]any)
	return hs
}

// hookSpec is one Claude Code hook event aivoicebus installs an entry for.
type hookSpec struct {
	event   string
	command string
	timeout int
}

// hookSpecs: a session start reads and shows the inbox; a prompt submit is a silent,
// very short check that speaks only if something urgent arrived while the agent worked.
var hookSpecs = []hookSpec{
	{"SessionStart", HookCommand, HookTimeout},
	{"UserPromptSubmit", PromptHookCommand, PromptHookTimeout},
}

func (sp hookSpec) isCanonical(h any) bool {
	m, _ := h.(map[string]any)
	return m["type"] == "command" && commandOf(h) == sp.command && secondsOf(m["timeout"]) == sp.timeout
}

// secondsOf reads a timeout as JSON decoding gives it (float64) or as this package
// writes it (int), so a document is judged the same before and after a round trip.
func secondsOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func isAiVoiceBus(h any) bool { return strings.Contains(commandOf(h), "aivoicebus") }

// ensureHook makes doc contain exactly one aivoicebus entry, the canonical one, for
// each event in hookSpecs, and leaves every other hook and setting alone. Old
// forms (the original installer's, a wrapper script) are replaced, not
// duplicated. It reports whether doc changed.
func ensureHook(doc map[string]any) (bool, error) {
	var hooks map[string]any
	switch v := doc["hooks"].(type) {
	case nil:
		hooks = map[string]any{}
	case map[string]any:
		hooks = v
	default:
		return false, errors.New(`"hooks" is not an object`)
	}
	changed := false
	for _, sp := range hookSpecs {
		c, err := sp.ensure(hooks)
		if err != nil {
			return false, err
		}
		changed = changed || c
	}
	if changed {
		doc["hooks"] = hooks
	}
	return changed, nil
}

func (sp hookSpec) ensure(hooks map[string]any) (bool, error) {
	var entries []any
	switch v := hooks[sp.event].(type) {
	case nil:
	case []any:
		entries = v
	default:
		return false, errors.New(`"hooks.` + sp.event + `" is not a list`)
	}

	found, canonical := 0, 0
	for _, e := range entries {
		for _, h := range hooksOf(e) {
			if isAiVoiceBus(h) {
				found++
				if sp.isCanonical(h) {
					canonical++
				}
			}
		}
	}
	if found == 1 && canonical == 1 {
		return false, nil
	}

	var kept []any
	for _, e := range entries {
		em, ok := e.(map[string]any)
		hs := hooksOf(e)
		if !ok || hs == nil {
			kept = append(kept, e)
			continue
		}
		var rest []any
		for _, h := range hs {
			if !isAiVoiceBus(h) {
				rest = append(rest, h)
			}
		}
		switch {
		case len(rest) == len(hs): // nothing of ours in this entry
			kept = append(kept, e)
		case len(rest) > 0: // other hooks share the entry: keep them
			em["hooks"] = rest
			kept = append(kept, em)
		} // else the entry held only aivoicebus hooks: drop it
	}
	kept = append(kept, map[string]any{"hooks": []any{
		map[string]any{"type": "command", "command": sp.command, "timeout": sp.timeout},
	}})
	hooks[sp.event] = kept
	return true, nil
}

// hookStatus describes the aivoicebus hooks in doc: "current" when every event has
// exactly the canonical entry, "missing" when there is none at all, else "stale".
func hookStatus(doc map[string]any) string {
	hooks, _ := doc["hooks"].(map[string]any)
	current, missing := 0, 0
	for _, sp := range hookSpecs {
		entries, _ := hooks[sp.event].([]any)
		found, canonical := 0, 0
		for _, e := range entries {
			for _, h := range hooksOf(e) {
				if isAiVoiceBus(h) {
					found++
					if sp.isCanonical(h) {
						canonical++
					}
				}
			}
		}
		switch {
		case found == 0:
			missing++
		case found == 1 && canonical == 1:
			current++
		}
	}
	switch {
	case current == len(hookSpecs):
		return "current"
	case missing == len(hookSpecs):
		return "missing"
	default:
		return "stale"
	}
}
