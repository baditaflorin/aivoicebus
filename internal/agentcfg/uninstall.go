package agentcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RemoveOptions configure Uninstall.
type RemoveOptions struct {
	Home   string   // default: the user's home
	Tools  []string // tools to clean; nil means all
	DryRun bool
	// Purge also deletes the whole config directory (token, saved credentials, sent-message log,
	// trusted keys) and the installed binary. Without it those stay: only what setup added goes.
	Purge bool
	// ClaudeMCPRemove takes the MCP server out of Claude Code's settings (see Options.ClaudeMCP).
	ClaudeMCPRemove func(dryRun bool) (status, detail string)
}

// Uninstall removes what Setup added and nothing else: the session hooks in
// ~/.claude/settings.json, the marked block in each tool's instruction file, the line in
// ~/.zshrc, and the env, state and hook-status files. Everything it edits is backed up first.
// A file it cannot vouch for (settings.json that is not valid JSON, a block with no end marker)
// is left alone and reported. It is idempotent, and with DryRun it writes nothing.
func Uninstall(o RemoveOptions) ([]Step, error) {
	if o.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		o.Home = h
	}
	in := &installer{o: Options{Home: o.Home, Tools: o.Tools, DryRun: o.DryRun, ClaudeMCPRemove: o.ClaudeMCPRemove}, p: Paths{o.Home}}
	in.removeHook()
	in.removeToolHooks()
	in.removeMCPServers()
	in.removeInstructions()
	in.removeShell()
	in.removeConfigFiles(o.Purge)
	if o.Purge {
		in.removeBinary()
	}
	for _, s := range in.steps {
		if s.Status == Failed {
			return in.steps, fmt.Errorf("uninstall finished with failures")
		}
	}
	return in.steps, nil
}

// removeHook takes every aivoicebus hook out of doc and leaves the rest. It reports whether
// doc changed.
func removeHook(doc map[string]any) (bool, error) {
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return false, nil
	}
	changed := false
	for event, v := range hooks {
		entries, ok := v.([]any)
		if !ok {
			continue
		}
		var kept []any
		evChanged := false
		for _, e := range entries {
			em, isMap := e.(map[string]any)
			hs := hooksOf(e)
			if !isMap || hs == nil {
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
			case len(rest) == len(hs):
				kept = append(kept, e)
			case len(rest) > 0:
				em["hooks"] = rest
				kept = append(kept, em)
				evChanged = true
			default: // the entry held only aivoicebus hooks
				evChanged = true
			}
		}
		switch {
		case !evChanged:
		case len(kept) == 0:
			delete(hooks, event)
		default:
			hooks[event] = kept
		}
		changed = changed || evChanged
	}
	if len(hooks) == 0 && changed {
		delete(doc, "hooks")
	}
	return changed, nil
}

func (in *installer) removeHook() {
	if !in.wants("claude") {
		return
	}
	path := in.p.Settings()
	raw, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		in.add("claude hook", Skipped, "no "+path)
		return
	case err != nil:
		in.add("claude hook", Failed, err.Error())
		return
	}
	doc := map[string]any{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		in.add("claude hook", Failed, "settings.json is not valid JSON, left alone: "+err.Error())
		return
	}
	changed, err := removeHook(doc)
	if err != nil {
		in.add("claude hook", Failed, "settings.json left alone: "+err.Error())
		return
	}
	if !changed {
		in.add("claude hook", OK, "none in "+path)
		return
	}
	out, _ := json.MarshalIndent(doc, "", "  ")
	in.backup(path)
	if err := in.write(path, string(out)+"\n", 0o755); err != nil {
		in.add("claude hook", Failed, err.Error())
		return
	}
	in.add("claude hook", Changed, path)
}

// removeBlock takes the marked block (or the original installer's one-paragraph section) out of
// an instruction file's content. A block whose extent is unknown is refused, never guessed.
func removeBlock(cur string) (string, bool, error) {
	var out string
	switch i := strings.Index(cur, beginMark); {
	case i >= 0:
		j := strings.Index(cur[i:], endMark)
		if j < 0 {
			return cur, false, fmt.Errorf("it has a aivoicebus begin marker with no end marker after it, so the block's extent is unknown")
		}
		end := i + j + len(endMark)
		if strings.HasPrefix(cur[end:], "\n") {
			end++
		}
		out = cur[:i] + cur[end:]
	case legacyStart(cur) >= 0:
		i := legacyStart(cur)
		end := len(cur)
		if k := strings.Index(cur[i+1:], "\n## "); k >= 0 {
			end = i + 1 + k + 1
		}
		if !looksInstalled(cur[i:end]) {
			return cur, false, fmt.Errorf("its \"## aivoicebus:\" section holds more than the installer's one paragraph, so removing it could delete your own text")
		}
		out = cur[:i] + cur[end:]
	default:
		return cur, false, nil
	}
	// the block was appended after a blank line: take the blank line with it
	out = strings.TrimRight(out, "\n")
	if strings.TrimSpace(out) != "" {
		out += "\n"
	} else {
		out = ""
	}
	return out, true, nil
}

func (in *installer) removeInstructions() {
	for _, t := range Tools {
		if !in.wants(t.Name) {
			continue
		}
		name := t.Name + " instructions"
		path := in.p.Instructions(t)
		cur, ok := readFile(path)
		if !ok {
			in.add(name, Skipped, "no "+path)
			continue
		}
		next, changed, err := removeBlock(cur)
		if err != nil {
			in.add(name, Failed, "refusing to rewrite "+path+", left untouched: "+err.Error())
			continue
		}
		if !changed {
			in.add(name, OK, "no block in "+path)
			continue
		}
		in.backup(path)
		if !in.o.DryRun {
			if next == "" { // the file held only our block
				if err := os.Remove(path); err != nil {
					in.add(name, Failed, err.Error())
					continue
				}
			} else if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
				in.add(name, Failed, err.Error())
				continue
			}
		}
		in.add(name, Changed, path)
	}
}

func (in *installer) removeShell() {
	path := in.p.Zshrc()
	cur, ok := readFile(path)
	if !ok {
		in.add("shell", Skipped, "no "+path)
		return
	}
	var kept []string
	removed := false
	for _, l := range strings.Split(cur, "\n") {
		if strings.Contains(l, "aivoicebus/env") {
			removed = true
			continue
		}
		kept = append(kept, l)
	}
	if !removed {
		in.add("shell", OK, "no aivoicebus line in "+path)
		return
	}
	in.backup(path)
	if !in.o.DryRun {
		if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
			in.add("shell", Failed, err.Error())
			return
		}
	}
	in.add("shell", Changed, path)
}

func (in *installer) removeConfigFiles(purge bool) {
	if purge {
		dir := in.p.ConfigDir()
		if _, err := os.Stat(dir); err != nil {
			in.add("config dir", Skipped, "no "+dir)
			return
		}
		if !in.o.DryRun {
			if err := os.RemoveAll(dir); err != nil {
				in.add("config dir", Failed, err.Error())
				return
			}
		}
		in.add("config dir", Changed, dir+" (token, saved credentials, sent-message log and trusted keys included)")
		return
	}
	var gone []string
	for _, f := range []string{in.p.Env(), in.p.State(), in.p.HookStatus()} {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		if !in.o.DryRun {
			if err := os.Remove(f); err != nil {
				in.add("config files", Failed, err.Error())
				return
			}
		}
		gone = append(gone, filepath.Base(f))
	}
	titles := filepath.Join(in.p.ConfigDir(), "titles") // the session titles the hook saved, a cache like hook-status.json
	if _, err := os.Stat(titles); err == nil {
		if !in.o.DryRun {
			if err := os.RemoveAll(titles); err != nil {
				in.add("config files", Failed, err.Error())
				return
			}
		}
		gone = append(gone, "titles/")
	}
	if len(gone) == 0 {
		in.add("config files", OK, "none in "+in.p.ConfigDir())
		return
	}
	in.add("config files", Changed, strings.Join(gone, ", ")+" in "+in.p.ConfigDir()+" (the token and anything else there stays; --purge removes it)")
}

func (in *installer) removeBinary() {
	var gone []string
	for _, f := range []string{in.p.Binary(), in.p.Binary() + ".prev"} {
		if _, err := os.Lstat(f); err != nil {
			continue
		}
		if !in.o.DryRun {
			if err := os.Remove(f); err != nil {
				in.add("install", Failed, err.Error())
				return
			}
		}
		gone = append(gone, f)
	}
	if len(gone) == 0 {
		in.add("install", Skipped, "no binary at "+in.p.Binary())
		return
	}
	in.add("install", Changed, strings.Join(gone, ", "))
}
