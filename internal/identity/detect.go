package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Which agent tool is this process running under? A session of any tool should show up as
// claude.xxxx, codex.xxxx, gemini.xxxx... without per-machine setup, so the answer comes from
// what is already there: the processes above this one, then environment markers the tools put
// into the shells they spawn. Only the Claude Code hook exports AIVOICEBUS_AGENT; the rest of
// this file is for tools without one.
//
// Status of each marker (checked on one Mac; "best-effort" means taken from the tool's
// documentation or memory and not seen on a live process):
//
//	claude   env CLAUDECODE, CLAUDE_CODE_SESSION_ID, CLAUDE_CODE_ENTRYPOINT    verified (live session)
//	codex    env CODEX_THREAD_ID, CODEX_SANDBOX(_NETWORK_DISABLED), CODEX_CI    names found in the codex binary; not seen in a live child shell
//	         process "codex", MCP client "codex-mcp-client"                     verified (process table, binary)
//	cursor   env CURSOR_AGENT=1 (set for the commands its agent runs)           found in the cursor-agent bundle
//	gemini   env GEMINI_CLI, GEMINI_SESSION_ID; process gemini (a node script)  best-effort (not installed here)
//	grok, opencode, aider, goose: process names only (and goose/opencode env    best-effort; grok has no env marker
//	         below, also best-effort)                                            that could be supported

// Proc is one process in the chain above this one.
type Proc struct {
	PID    int
	Start  string // any string that is stable for the life of the process (its start time)
	Exe    string // the executable's path
	Script string // for an interpreter (node, python...): the script it runs
}

// Detection is the answer to "who is this".
type Detection struct {
	Name    string // an explicit AIVOICEBUS_AGENT kept as it is; "" otherwise
	Tool    string // "" when unknown
	Session string // short per-session suffix; "" when none could be found
	Source  string // why Tool was chosen, for whoami --why and doctor
}

// Agent is the agent part of an identity: tool.session, tool alone, or "agent".
func (d Detection) Agent() string {
	switch {
	case d.Name != "":
		return strings.ToLower(d.Name)
	case d.Tool == "":
		return "agent"
	case d.Session == "":
		return d.Tool
	}
	return d.Tool + "." + d.Session
}

// Why is one line saying how the tool was found.
func (d Detection) Why() string {
	if d.Tool == "" {
		if d.Source == "" {
			return "no known agent tool found above this process or in the environment"
		}
		return d.Source
	}
	return "tool " + d.Tool + ", from " + d.Source
}

type toolSpec struct {
	name    string
	exes    []string // executable basenames (lower case, no extension)
	scripts []string // path fragments of the script an interpreter runs, for tools shipped as node packages
	env     []string // variables only this tool sets in the shells it spawns
	session string   // variable holding the tool's own session id ("" if none)
	prefix  bool     // take the first four letters/digits of the id (as the Claude hook does) instead of hashing it
	clients []string // substrings of the MCP clientInfo.name
}

// The order matters twice: the first match wins among environment markers, so a tool that is
// started from another one (Codex inside Claude Code inherits CLAUDECODE) comes before the
// launcher. The process chain does not need this: the nearest ancestor wins there.
var tools = []toolSpec{
	{name: "codex", exes: []string{"codex"}, scripts: []string{"/@openai/codex/"},
		env: []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", "CODEX_CI"},
		// A Codex thread id is a time-ordered UUID: its first characters are the same for hours,
		// so the id is hashed, not cut.
		session: "CODEX_THREAD_ID", clients: []string{"codex"}},
	{name: "gemini", exes: []string{"gemini"}, scripts: []string{"/@google/gemini-cli/"},
		env: []string{"GEMINI_CLI", "GEMINI_SESSION_ID"}, session: "GEMINI_SESSION_ID", clients: []string{"gemini"}},
	// Grok Build (xAI) is the `grok` binary, also shipped as xai-grok-shell; its hooks get GROK_SESSION_ID,
	// which may reach the shells it spawns too (best-effort, from its docs).
	{name: "grok", exes: []string{"grok", "grok-cli", "xai-grok-shell"}, scripts: []string{"/@xai-official/grok/"},
		env: []string{"GROK_SESSION_ID"}, session: "GROK_SESSION_ID", clients: []string{"grok"}},
	{name: "cursor-agent", exes: []string{"cursor-agent"}, env: []string{"CURSOR_AGENT"}, clients: []string{"cursor"}},
	{name: "opencode", exes: []string{"opencode"}, env: []string{"OPENCODE"}, clients: []string{"opencode"}},
	{name: "aider", exes: []string{"aider"}, clients: []string{"aider"}},
	{name: "goose", exes: []string{"goose"}, env: []string{"GOOSE_TERMINAL"}, clients: []string{"goose"}},
	{name: "claude", exes: []string{"claude", "claude-code"}, scripts: []string{"/@anthropic-ai/claude-code/"},
		env:     []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT"},
		session: "CLAUDE_CODE_SESSION_ID", prefix: true, clients: []string{"claude"}},
}

// interpreters run a script whose name says what the tool is (gemini is `node .../gemini`).
var interpreters = map[string]bool{"node": true, "nodejs": true, "bun": true, "deno": true, "python": true, "python3": true, "ruby": true}

// ToolFromClient maps an MCP clientInfo.name ("codex-mcp-client", "claude-code") to a tool, "" if unknown.
func ToolFromClient(name string) string {
	name = strings.ToLower(name)
	for _, t := range tools {
		for _, c := range t.clients {
			if strings.Contains(name, c) {
				return t.name
			}
		}
	}
	return ""
}

// ToolOf is the tool an agent name such as "codex.q1w2" belongs to, "" if it is none we know.
func ToolOf(agent string) string {
	head, _, _ := strings.Cut(strings.ToLower(agent), ".")
	for _, t := range tools {
		if t.name == head {
			return t.name
		}
	}
	return ""
}

func baseName(p string) string {
	b := strings.ToLower(filepath.Base(p))
	for _, ext := range []string{".exe", ".js", ".mjs", ".cjs", ".py"} {
		b = strings.TrimSuffix(b, ext)
	}
	return b
}

// procTool says which tool a process is, "" if none. A native executable must be named exactly
// like the tool, in lower case: that leaves out the Claude Desktop app ("Claude"), which is not a
// Claude Code session. An interpreter (node, python...) counts only when it runs the tool's
// package (.../@google/gemini-cli/...) or its launcher shim, a file named exactly like the tool
// in a bin directory (/usr/local/bin/gemini); `node /opt/x/goose.js` is somebody's own script.
func procTool(p Proc) string {
	exe := strings.TrimSuffix(filepath.Base(p.Exe), ".exe")
	script := filepath.ToSlash(p.Script)
	shim := filepath.Base(script)
	shimDir := filepath.Base(filepath.Dir(p.Script))
	for _, t := range tools {
		for _, e := range t.exes {
			if exe == e {
				return t.name
			}
			if interpreters[strings.ToLower(exe)] && shim == e && shimDir == "bin" {
				return t.name
			}
		}
		if interpreters[strings.ToLower(exe)] {
			for _, s := range t.scripts {
				if strings.Contains(script, s) {
					return t.name
				}
			}
		}
	}
	return ""
}

func (t toolSpec) sessionFromEnv(getenv func(string) string) string {
	if t.session == "" {
		return ""
	}
	id := getenv(t.session)
	if id == "" {
		return ""
	}
	if t.prefix {
		return shortAlnum(id)
	}
	return hash4(id)
}

// shortAlnum is the first four letters or digits of id, lower-cased: how the Claude hook
// turns a session id into the suffix, so detection and the hook agree on one session.
func shortAlnum(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			if b.Len() == 4 {
				break
			}
		}
	}
	return b.String()
}

func hash4(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:4]
}

// Detect works out the tool of a process from its environment and the chain of processes above
// it (nearest first). It is pure: Me() feeds it the real environment and process table.
// Explicit settings win: AIVOICEBUS_ME, then AIVOICEBUS_AGENT, then a registered long-lived
// AIVOICEBUS_NAME (names.go; AIVOICEBUS_DETECT=off skips the rest, for a machine that wants the plain
// "agent"). Then the nearest ancestor that is
// a known tool, then environment markers; an unknown tool stays "agent".
func Detect(getenv func(string) string, chain []Proc) Detection {
	if v := getenv("AIVOICEBUS_ME"); v != "" {
		return Detection{Source: "AIVOICEBUS_ME is set; it overrides the whole identity"}
	}
	if v := getenv("AIVOICEBUS_AGENT"); v != "" {
		hook := getenv("AIVOICEBUS_AGENT_SRC") == "hook"
		d := Detection{Name: v, Tool: ToolOf(v), Session: sessionOf(v), Source: "AIVOICEBUS_AGENT=" + clean(v)}
		if !hook {
			d.Source += " (set by you)"
			return d
		}
		d.Source += " (exported by the session-start hook)"
		// The hook exported its tool for the shell commands of its own session. A different tool
		// started from one of them (Codex from a Claude Bash command) inherits that variable but
		// is not that session: when it can be seen to be another tool, detect it instead.
		if d.Tool != "" {
			if imp := detectImplicit(getenv, chain); imp.Tool != "" && imp.Tool != d.Tool {
				imp.Source += "; the hook's " + d.Tool + " identity belongs to the tool above, not this one"
				return imp
			}
		}
		return d
	}
	imp := detectImplicit(getenv, chain)
	d, why := namedDetection(getenv, imp)
	if d.Name != "" {
		return d
	}
	if why != "" {
		imp.Source += "; " + why
	}
	return imp
}

// clean makes an environment value safe to print: no control characters (an ESC could
// rewrite the terminal), at most 200 characters (the identity limit).
func clean(v string) string {
	var b strings.Builder
	n := 0
	for _, r := range v {
		if n == 200 {
			b.WriteString("...")
			break
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

func sessionOf(agent string) string {
	_, s, _ := strings.Cut(strings.ToLower(agent), ".")
	return s
}

func detectImplicit(getenv func(string) string, chain []Proc) Detection {
	switch strings.ToLower(getenv("AIVOICEBUS_DETECT")) {
	case "off", "0", "no", "false":
		return Detection{Source: "AIVOICEBUS_DETECT turns tool detection off"}
	}
	for _, p := range chain {
		name := procTool(p)
		if name == "" {
			continue
		}
		t := specOf(name)
		sess := t.sessionFromEnv(getenv)
		if sess == "" && p.PID > 0 {
			// One process, one session: the tool's own pid and start time do not change
			// between the commands of a session, and differ between sessions.
			sess = hash4(name, fmt.Sprint(p.PID), p.Start)
		}
		return Detection{Tool: name, Session: sess,
			Source: fmt.Sprintf("the %s process above this one (pid %d)", filepath.Base(p.Exe), p.PID)}
	}
	// Environment markers alone cannot say which tool is nearest when several are set (claude -p
	// run by Codex inherits Codex's variables and adds its own, and the reverse). The tool with
	// the most of its own markers present is taken as the most specific; a tie goes to the
	// earlier table entry, the inner tools. The doubt is reported, never hidden.
	best, bestN, bestVar := "", 0, ""
	var seen []string
	for _, t := range tools {
		n, first := 0, ""
		for _, v := range t.env {
			if getenv(v) != "" {
				if n == 0 {
					first = v
				}
				n++
			}
		}
		if n == 0 {
			continue
		}
		seen = append(seen, t.name)
		if n > bestN {
			best, bestN, bestVar = t.name, n, first
		}
	}
	if best == "" {
		return Detection{}
	}
	d := Detection{Tool: best, Session: specOf(best).sessionFromEnv(getenv), Source: "environment variable " + bestVar}
	if len(seen) > 1 {
		d.Source += fmt.Sprintf(" (markers of %s were set too; with no process table the nearest tool cannot be told: set AIVOICEBUS_AGENT if this is wrong)", strings.Join(seen, ", "))
	}
	return d
}

func specOf(name string) toolSpec {
	for _, t := range tools {
		if t.name == name {
			return t
		}
	}
	return toolSpec{name: name}
}

type psRow struct {
	ppid  int
	start string
	exe   string
}

// parsePS reads `ps -axo pid=,ppid=,lstart=,comm=` (the macOS probe): lstart is five words
// ("Tue Oct 13 20:40:42 2026"), and comm is the rest of the line, which may hold spaces
// ("Application Support"). Lines that do not fit are skipped.
func parsePS(out string) map[int]psRow {
	rows := map[int]psRow{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 8 { // pid ppid lstart(5 words) comm
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			continue
		}
		rest := strings.TrimSpace(line)
		for i := 0; i < 7; i++ {
			j := strings.IndexAny(rest, " \t")
			rest = strings.TrimLeft(rest[j:], " \t")
		}
		rows[pid] = psRow{ppid, strings.Join(f[2:7], " "), rest}
	}
	return rows
}

// scriptFromArgv is the script an interpreter runs: the first argument that is not a flag.
func scriptFromArgv(argv []string) string {
	for _, a := range argv[min(1, len(argv)):] {
		if a != "" && !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

var (
	implicitOnce sync.Once
	implicitDet  Detection
	chainRead    sync.Once
	chainCache   []Proc
	// chainProbe reads the process table; tests replace it.
	chainProbe = platformChain
)

// Implicit is the detection from processes and environment alone, ignoring AIVOICEBUS_ME and
// AIVOICEBUS_AGENT. It costs one process-table read (about 20 ms on a Mac), done once.
func Implicit() Detection {
	if DetectOff() { // read every time: it is a setting, not something found in the process table
		return detectImplicit(osGetenv, nil)
	}
	implicitOnce.Do(func() { implicitDet = detectImplicit(osGetenv, chainOnce()) })
	return implicitDet
}

// chainOnce reads the process table once per process.
func chainOnce() []Proc {
	chainRead.Do(func() { chainCache = chainProbe() })
	return chainCache
}

// Explain is what Me() decided and why, including the explicit settings. The process table is
// read only when it can matter: no explicit identity, or one exported by the hook.
func Explain() Detection {
	if osGetenv("AIVOICEBUS_ME") != "" || (osGetenv("AIVOICEBUS_AGENT") != "" && osGetenv("AIVOICEBUS_AGENT_SRC") != "hook") {
		return Detect(osGetenv, nil)
	}
	if osGetenv("AIVOICEBUS_AGENT") != "" || osGetenv("AIVOICEBUS_NAME") != "" {
		return Detect(osGetenv, chainOnce())
	}
	return Implicit()
}

// Printable makes an environment-derived string safe to echo to a terminal.
func Printable(v string) string { return clean(v) }

// DetectOff reports whether AIVOICEBUS_DETECT turns tool detection off.
func DetectOff() bool {
	switch strings.ToLower(osGetenv("AIVOICEBUS_DETECT")) {
	case "off", "0", "no", "false":
		return true
	}
	return false
}
