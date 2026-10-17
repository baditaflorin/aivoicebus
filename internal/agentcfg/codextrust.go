package agentcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Codex runs a hook from ~/.codex/hooks.json only after a person trusts it, and keeps that trust in
// ~/.codex/config.toml as a trusted_hash under [hooks.state."<hooks.json path>:<event>:<group>:<handler>"]
// (event in snake case, group and handler counted from 0; Codex 0.16x). The hash is Codex's own, so
// this can tell trusted from never trusted, not whether Codex will ask again after a hook changed.

// CodexHook is one aivoicebus hook in hooks.json and whether Codex trusts it.
type CodexHook struct {
	Event   string // as hooks.json names it: SessionStart, UserPromptSubmit
	Key     string // the hooks.state key in config.toml
	Trusted bool
}

// CodexHooks lists aivoicebus's hooks in ~/.codex/hooks.json under home and whether config.toml
// trusts each. No hooks.json, or none of ours in it, is an empty list and no error.
func CodexHooks(home string) ([]CodexHook, error) {
	p := Paths{home}
	path := p.join(".codex", "hooks.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %v", path, err)
	}
	hooks, _ := doc["hooks"].(map[string]any)
	cfg, _ := readFile(p.join(".codex", "config.toml"))
	var out []CodexHook
	for _, event := range []string{"SessionStart", "UserPromptSubmit"} {
		entries, _ := hooks[event].([]any)
		for g, e := range entries {
			for h, hk := range hooksOf(e) {
				if !isAiVoiceBus(hk) {
					continue
				}
				key := fmt.Sprintf("%s:%s:%d:%d", path, snakeCase(event), g, h)
				out = append(out, CodexHook{Event: event, Key: key, Trusted: tomlTableHas(cfg, `hooks.state."`+key+`"`, "trusted_hash")})
			}
		}
	}
	return out, nil
}

// snakeCase is SessionStart -> session_start.
func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// tomlTableHas reports whether the TOML text has the table [name] with a key (a plain line scan:
// enough for the tables Codex writes, which keep one key per line).
func tomlTableHas(text, name, key string) bool {
	in := false
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			in = t == "["+name+"]"
			continue
		}
		if in {
			if k, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == key {
				return true
			}
		}
	}
	return false
}

// codexTrustCheck is Doctor's line for the Codex hooks; ok is false when Doctor has nothing to say
// (no Codex, or no aivoicebus hooks in it).
func codexTrustCheck(home string) (Check, bool) {
	hs, err := CodexHooks(home)
	if err != nil {
		return Check{"codex hooks", LevelWarn, err.Error()}, true
	}
	if len(hs) == 0 {
		return Check{}, false
	}
	var untrusted []string
	for _, h := range hs {
		if !h.Trusted {
			untrusted = append(untrusted, h.Event)
		}
	}
	if len(untrusted) > 0 {
		return Check{"codex hooks", LevelWarn, "Codex does not run them until you trust them (" + strings.Join(untrusted, ", ") + "): run `aivoicebus trust-codex` once"}, true
	}
	return Check{"codex hooks", LevelOK, fmt.Sprintf("%d trusted in ~/.codex/config.toml (Codex asks again if a hook changes)", len(hs))}, true
}
