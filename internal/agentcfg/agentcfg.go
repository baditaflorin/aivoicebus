// Package agentcfg installs and checks aivoicebus's footprint on one machine:
// the env file, the shell line, the Claude Code session-start hook, and the
// instruction blocks agent tools read. Everything it writes comes from this
// binary (the hook command and the embedded texts), so a machine is only ever
// as stale as its binary, and `setup` brings the rest back in line.
//
// Every step is idempotent and has a dry-run mode. It makes no network calls.
package agentcfg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/assets"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/version"
)

const (
	// HookCommand is the one Claude Code session-start hook entry aivoicebus
	// installs. It points at the installed binary, which does all the work, so
	// the entry itself never needs to change.
	HookCommand = `[ -x "$HOME/.local/bin/aivoicebus" ] && "$HOME/.local/bin/aivoicebus" hook --tool claude || true`
	// HookTimeout is a backstop; the hook gives up on its own well before it.
	HookTimeout = 5

	// PromptHookCommand is the per-prompt check: silent unless an urgent message
	// arrived while the agent was working. It must be cheap, so it has a short
	// budget of its own and a backstop timeout of 2 seconds.
	PromptHookCommand = `[ -x "$HOME/.local/bin/aivoicebus" ] && "$HOME/.local/bin/aivoicebus" hook --tool claude --event prompt || true`
	PromptHookTimeout = 2

	shellLine = `. "$HOME/.config/aivoicebus/env"   # aivoicebus`
)

// Tool is an agent tool whose instruction file gets the aivoicebus block.
type Tool struct{ Name, Dir, File string }

// Tools lists the supported agent tools, relative to the home directory.
var Tools = []Tool{
	{"claude", ".claude", "CLAUDE.md"},
	{"codex", ".codex", "AGENTS.md"},
	{"gemini", ".gemini", "GEMINI.md"},
	// Grok Build reads "global rules in ~/.grok/" (its docs name no file; AGENTS.md is what it reads
	// in project directories, so the same name is used here).
	{"grok", ".grok", "AGENTS.md"},
}

// Paths locates every file aivoicebus manages under one home directory.
type Paths struct{ Home string }

func (p Paths) join(parts ...string) string {
	return filepath.Join(append([]string{p.Home}, parts...)...)
}

func (p Paths) ConfigDir() string          { return p.join(".config", "aivoicebus") }
func (p Paths) Env() string                { return p.join(".config", "aivoicebus", "env") }
func (p Paths) Token() string              { return p.join(".config", "aivoicebus", "token") }
func (p Paths) State() string              { return p.join(".config", "aivoicebus", "state.json") }
func (p Paths) HookStatus() string         { return p.join(".config", "aivoicebus", config.HookStatusFile) }
func (p Paths) Settings() string           { return p.join(".claude", "settings.json") }
func (p Paths) Zshrc() string              { return p.join(".zshrc") }
func (p Paths) Binary() string             { return p.join(".local", "bin", "aivoicebus") }
func (p Paths) ToolDir(t Tool) string      { return p.join(t.Dir) }
func (p Paths) Instructions(t Tool) string { return p.join(t.Dir, t.File) }

func shortID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}

// HookID, SnippetID and UsageID identify the content of each managed part, so
// "is this machine current" is a comparison, not a hand-kept version number.
func HookID() string {
	return shortID(HookCommand, strconv.Itoa(HookTimeout), PromptHookCommand, strconv.Itoa(PromptHookTimeout))
}
func SnippetID() string { return shortID(assets.Snippet()) }
func UsageID() string   { return shortID(assets.Usage()) }

// State is what setup last applied; it is written to ~/.config/aivoicebus/state.json.
type State struct {
	Release string `json:"release"`
	Commit  string `json:"commit"`
	Hook    string `json:"hook"`
	Snippet string `json:"snippet"`
	Usage   string `json:"usage"`
}

// Current is the State this binary would apply.
func Current() State {
	return State{version.Release, version.CommitOrBuildInfo(), HookID(), SnippetID(), UsageID()}
}

// Step statuses. In a dry run, Changed means "would change".
const (
	OK      = "ok"
	Changed = "changed"
	Skipped = "skipped"
	Failed  = "failed"
)

// Step is the outcome of one part of setup.
type Step struct{ Name, Status, Detail string }

// Options configure Setup.
type Options struct {
	Home   string   // default: the user's home
	URL    string   // node URLs for the env file; "" keeps what is there
	Tools  []string // tools to configure; nil means all
	DryRun bool

	// InstallExe, when set, is a binary to copy to ~/.local/bin/aivoicebus if nothing is
	// installed there yet (the hook runs it from there). An installed copy is never replaced:
	// that is what `aivoicebus update` is for.
	InstallExe string
	// InstallExeErr is why the running binary could not be found (os.Executable failed): the
	// install step then reports it instead of silently doing nothing.
	InstallExeErr error

	// ClaudeMCP registers the MCP server with Claude Code (`claude mcp add`), which Grok Build
	// reads too; nil skips it. It returns a step status and detail. ClaudeMCPRemove undoes it.
	ClaudeMCP       func(bin string, dryRun bool) (status, detail string)
	ClaudeMCPRemove func(dryRun bool) (status, detail string)
}

type installer struct {
	o        Options
	p        Paths
	steps    []Step
	envReady bool // the env file exists, or this run creates it
}

// Setup brings this machine's aivoicebus footprint in line with this binary.
// It returns every step, and an error if any step failed.
func Setup(o Options) ([]Step, error) {
	if o.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		o.Home = h
	}
	in := &installer{o: o, p: Paths{o.Home}}
	in.binary()
	in.env()
	in.shell()
	in.tokenPerms()
	in.claudeHook()
	in.toolHooks()
	in.instructions()
	in.mcpServers()
	in.claudeStandby()
	in.state()
	for _, s := range in.steps {
		if s.Status == Failed {
			return in.steps, errors.New("setup finished with failures")
		}
	}
	return in.steps, nil
}

func (in *installer) add(name, status, detail string) {
	in.steps = append(in.steps, Step{name, status, detail})
}

func (in *installer) wants(tool string) bool {
	if len(in.o.Tools) == 0 {
		return true
	}
	for _, t := range in.o.Tools {
		if t == tool {
			return true
		}
	}
	return false
}

// write stores content at path, creating directories (dirMode for the leaf directory).
func (in *installer) write(path, content string, dirMode os.FileMode) error {
	if in.o.DryRun {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// backup copies an existing file next to itself before it is rewritten.
func (in *installer) backup(path string) {
	if in.o.DryRun {
		return
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak-aivoicebus-"+time.Now().Format("20060102-150405"), b, 0o600)
	}
}

func readFile(path string) (string, bool) {
	b, err := os.ReadFile(path)
	return string(b), err == nil
}

func (in *installer) binary() {
	if in.o.InstallExe == "" && in.o.InstallExeErr == nil {
		return
	}
	dst := in.p.Binary()
	switch err := Installed(dst); {
	case err == nil:
		in.add("install", OK, dst)
		return
	case isDir(dst):
		in.add("install", Failed, dst+" is a directory; move it away and run again")
		return
	}
	if in.o.InstallExeErr != nil {
		in.add("install", Failed, "no usable binary at "+dst+" and the running one cannot be found: "+in.o.InstallExeErr.Error())
		return
	}
	b, err := os.ReadFile(in.o.InstallExe)
	if err != nil {
		in.add("install", Failed, "cannot read the running binary: "+err.Error())
		return
	}
	if !in.o.DryRun {
		if err := installFile(dst, b); err != nil {
			in.add("install", Failed, err.Error())
			return
		}
	}
	in.add("install", Changed, "copy of "+in.o.InstallExe+" -> "+dst)
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// Installed says whether path holds something the hook could run: a non-empty regular file
// with an execute bit. A leftover empty or non-executable file counts as not installed.
func Installed(path string) error {
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return err
	case !fi.Mode().IsRegular():
		return errors.New("not a regular file")
	case fi.Size() == 0:
		return errors.New("empty file")
	case fi.Mode().Perm()&0o111 == 0:
		return errors.New("not executable")
	}
	return nil
}

// installFile writes an executable next to its destination and renames it into place, so a
// hook that runs meanwhile sees the old file or the whole new one, never half of it.
func installFile(dst string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".aivoicebus-install-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	if werr == nil {
		werr = tmp.Chmod(0o755) // on the open handle: no window where the path is something else
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), dst)
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}

func (in *installer) env() {
	path := in.p.Env()
	cur, exists := readFile(path)
	url := in.o.URL
	if url == "" && !hasLine(cur, "export AIVOICEBUS_URL=") {
		url = os.Getenv("AIVOICEBUS_URL")
	}
	if err := config.SafeNodeURLs(url); err != nil { // the file is sourced by the shell: nothing it could run
		in.add("env", Failed, "node URL refused: "+err.Error())
		return
	}
	want := cur
	if url != "" {
		want = ensureLine(want, "export PATH=", `export PATH="$HOME/.local/bin:$PATH"`)
		want = setLine(want, "export AIVOICEBUS_URL=", "export AIVOICEBUS_URL='"+url+"'")
	}
	if !hasLine(want, "export AIVOICEBUS_URL=") {
		in.add("env", Skipped, "no node URL: pass --url or set AIVOICEBUS_URL")
		return
	}
	in.envReady = true
	if exists && want == cur {
		in.add("env", OK, path)
		return
	}
	if err := in.write(path, want, 0o700); err != nil {
		in.add("env", Failed, err.Error())
		return
	}
	in.add("env", Changed, path)
}

func (in *installer) shell() {
	if !in.envReady {
		in.add("shell", Skipped, "no env file to source")
		return
	}
	path := in.p.Zshrc()
	cur, _ := readFile(path)
	if strings.Contains(cur, "aivoicebus/env") {
		in.add("shell", OK, path)
		return
	}
	next := cur
	if next != "" && !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	next += shellLine + "\n"
	if in.o.DryRun {
		in.add("shell", Changed, path)
		return
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		in.add("shell", Failed, err.Error())
		return
	}
	in.add("shell", Changed, path)
}

func (in *installer) tokenPerms() {
	path := in.p.Token()
	fi, err := os.Stat(path)
	if err != nil {
		in.add("token", Skipped, "no token file (AIVOICEBUS_TOKEN in the environment still works)")
		return
	}
	if fi.Mode().Perm()&0o077 == 0 {
		in.add("token", OK, fmt.Sprintf("%s (mode %04o)", path, fi.Mode().Perm()))
		return
	}
	if !in.o.DryRun {
		if err := os.Chmod(path, 0o600); err != nil {
			in.add("token", Failed, err.Error())
			return
		}
	}
	in.add("token", Changed, fmt.Sprintf("%s: mode %04o -> 0600", path, fi.Mode().Perm()))
}

func (in *installer) claudeHook() {
	if !in.wants("claude") {
		return
	}
	path := in.p.Settings()
	doc := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &doc); err != nil {
			in.add("claude hook", Failed, "settings.json is not valid JSON, left alone: "+err.Error())
			return
		}
		if doc == nil {
			doc = map[string]any{}
		}
	case !os.IsNotExist(err):
		in.add("claude hook", Failed, err.Error())
		return
	}
	changed, err := ensureHook(doc)
	if err != nil {
		in.add("claude hook", Failed, "settings.json left alone: "+err.Error())
		return
	}
	if !changed {
		in.add("claude hook", OK, path)
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

func (in *installer) instructions() {
	block := Block()
	for _, t := range Tools {
		if !in.wants(t.Name) {
			continue
		}
		name := t.Name + " instructions"
		if _, err := os.Stat(in.p.ToolDir(t)); err != nil && t.Name != "claude" {
			in.add(name, Skipped, "no ~/"+t.Dir+" (tool not installed); setup adds it once it exists")
			continue
		}
		path := in.p.Instructions(t)
		cur, _ := readFile(path)
		next, changed, err := ApplyBlock(cur, block)
		if err != nil {
			in.add(name, Failed, "refusing to rewrite "+path+", left untouched: "+err.Error())
			continue
		}
		if !changed {
			in.add(name, OK, path)
			continue
		}
		in.backup(path)
		if err := in.write(path, next, 0o755); err != nil {
			in.add(name, Failed, err.Error())
			continue
		}
		in.add(name, Changed, path)
	}
}

func (in *installer) state() {
	path := in.p.State()
	want := Current()
	var have State
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &have)
	}
	if have == want {
		in.add("state", OK, path)
		return
	}
	b, _ := json.MarshalIndent(want, "", "  ")
	if err := in.write(path, string(b)+"\n", 0o700); err != nil {
		in.add("state", Failed, err.Error())
		return
	}
	in.add("state", Changed, fmt.Sprintf("release %s, hook %s, snippet %s", want.Release, want.Hook, want.Snippet))
}

// --- small line helpers for the env file ---

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func hasLine(s, prefix string) bool {
	for _, l := range splitLines(s) {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			return true
		}
	}
	return false
}

// ensureLine appends line unless some line already starts with prefix.
func ensureLine(s, prefix, line string) string {
	if hasLine(s, prefix) {
		return s
	}
	return strings.Join(append(splitLines(s), line), "\n") + "\n"
}

// setLine replaces the line starting with prefix (dropping duplicates), or appends it.
func setLine(s, prefix, line string) string {
	var out []string
	done := false
	for _, l := range splitLines(s) {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			if !done {
				out = append(out, line)
				done = true
			}
			continue
		}
		out = append(out, l)
	}
	if !done {
		out = append(out, line)
	}
	return strings.Join(out, "\n") + "\n"
}
