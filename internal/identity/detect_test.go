package identity

import (
	"strings"
	"sync"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func envOf(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

func TestDetect(t *testing.T) {
	claudeApp := "/Users/x/Library/Application Support/Claude/claude-code/2.1.295/9d64/claude.app/Contents/MacOS/claude"
	zsh := Proc{PID: 5, Start: "t0", Exe: "/bin/zsh"}
	cases := []struct {
		name       string
		env        []string
		chain      []Proc
		tool, sess string // sess "" = none; "*" = any 4 hex chars; else exact
		sourceHas  string
	}{
		{"nothing known stays agent", nil, []Proc{zsh, {PID: 1000, Exe: "/usr/bin/tmux"}}, "", "", ""},
		{"CODEX_HOME alone is configuration, not a marker", []string{"CODEX_HOME", "/h"}, nil, "", "", ""},
		{"AIVOICEBUS_ME wins over everything", []string{"AIVOICEBUS_ME", "a/b", "CLAUDECODE", "1"}, []Proc{{PID: 9, Exe: "/bin/codex"}}, "", "", "AIVOICEBUS_ME"},
		{"AIVOICEBUS_AGENT wins over markers", []string{"AIVOICEBUS_AGENT", "Claude.A1B2", "CODEX_THREAD_ID", "x"}, []Proc{{PID: 9, Exe: "/bin/codex"}}, "claude", "a1b2", "AIVOICEBUS_AGENT"},
		{"explicit free-text agent has no known tool", []string{"AIVOICEBUS_AGENT", "build-bot"}, nil, "", "", "AIVOICEBUS_AGENT"},

		{"claude from env, session like the hook", []string{"CLAUDECODE", "1", "CLAUDE_CODE_SESSION_ID", "9058aa53-5065-4bc2"}, nil, "claude", "9058", "CLAUDE"},
		{"claude entrypoint alone", []string{"CLAUDE_CODE_ENTRYPOINT", "cli"}, nil, "claude", "", "CLAUDE_CODE_ENTRYPOINT"},
		{"codex from thread id (hashed, not cut)", []string{"CODEX_THREAD_ID", "0199-aaaa-bbbb"}, nil, "codex", "*", "CODEX_THREAD_ID"},
		{"codex sandbox marker", []string{"CODEX_SANDBOX", "seatbelt"}, nil, "codex", "", "CODEX_SANDBOX"},
		{"gemini from env", []string{"GEMINI_CLI", "1"}, nil, "gemini", "", "GEMINI_CLI"},
		{"gemini from antigravity env", []string{"ANTIGRAVITY_AGENT", "1", "ANTIGRAVITY_CONVERSATION_ID", "f40f54b5-5b43-4d64-9db6-3457637698f3"}, nil, "gemini", "f40f", "ANTIGRAVITY_"},
		{"cursor agent from env", []string{"CURSOR_AGENT", "1"}, nil, "cursor-agent", "", "CURSOR_AGENT"},
		{"opencode from env", []string{"OPENCODE", "1"}, nil, "opencode", "", "OPENCODE"},
		{"goose from env", []string{"GOOSE_TERMINAL", "1"}, nil, "goose", "", "GOOSE_TERMINAL"},
		{"codex started inside claude: the inner tool wins among markers", []string{"CLAUDECODE", "1", "CODEX_CI", "1"}, nil, "codex", "", "CODEX_CI"},

		{"codex process, session from pid+start", nil, []Proc{zsh, {PID: 77, Start: "Mon 10:00", Exe: "/Applications/ChatGPT.app/Contents/MacOS/codex"}}, "codex", "*", "pid 77"},
		{"claude process with spaces in its path", nil, []Proc{zsh, {PID: 88, Start: "s", Exe: claudeApp}}, "claude", "*", "pid 88"},
		{"claude process: the session id env keeps the hook's suffix", []string{"CLAUDE_CODE_SESSION_ID", "abcd1234"}, []Proc{{PID: 88, Start: "s", Exe: claudeApp}}, "claude", "abcd", "pid 88"},
		{"gemini is a node script", nil, []Proc{{PID: 3, Start: "a", Exe: "/usr/bin/node", Script: "/usr/local/bin/gemini"}}, "gemini", "*", "node"},
		{"gemini by package path", nil, []Proc{{PID: 3, Start: "a", Exe: "/usr/bin/node", Script: "/n/lib/node_modules/@google/gemini-cli/dist/index.js"}}, "gemini", "*", "node"},
		{"antigravity agy process with conversation id", []string{"ANTIGRAVITY_CONVERSATION_ID", "f40f54b5-5b43-4d64"}, []Proc{zsh, {PID: 99, Start: "s", Exe: "/Applications/Gemini.app/Contents/Helpers/agy"}}, "gemini", "f40f", "pid 99"},
		{"antigravity agy process without env falls back to pid+start", nil, []Proc{zsh, {PID: 99, Start: "s", Exe: "/Applications/Gemini.app/Contents/Helpers/agy"}}, "gemini", "*", "pid 99"},
		{"an unrelated node script is nothing", nil, []Proc{{PID: 3, Exe: "/usr/bin/node", Script: "/srv/app/server.js"}}, "", "", ""},
		{"grok by process name", nil, []Proc{{PID: 4, Start: "b", Exe: "/usr/local/bin/grok"}}, "grok", "*", "grok"},
		{"aider as a python script", nil, []Proc{{PID: 4, Start: "b", Exe: "/usr/bin/python3", Script: "/home/u/.local/bin/aider"}}, "aider", "*", "python3"},
		{"cursor-agent by name", nil, []Proc{{PID: 4, Start: "b", Exe: "/home/u/.local/bin/cursor-agent"}}, "cursor-agent", "*", "cursor-agent"},
		{"a helper that merely starts with codex is not codex", nil, []Proc{{PID: 4, Exe: "/x/codex-code-mode-host"}}, "", "", ""},
		{"nearest ancestor wins (codex under claude)", nil, []Proc{{PID: 1, Start: "a", Exe: "/b/codex"}, {PID: 2, Start: "b", Exe: claudeApp}}, "codex", "*", "pid 1"},
		{"nearest ancestor wins (claude under codex)", nil, []Proc{{PID: 2, Start: "b", Exe: claudeApp}, {PID: 1, Start: "a", Exe: "/b/codex"}}, "claude", "*", "pid 2"},
		{"the process chain beats stray markers", []string{"CLAUDECODE", "1"}, []Proc{{PID: 1, Start: "a", Exe: "/b/codex"}}, "codex", "*", "pid 1"},
		{"AIVOICEBUS_DETECT=off keeps agent", []string{"AIVOICEBUS_DETECT", "off", "CLAUDECODE", "1"}, []Proc{{PID: 1, Exe: "/b/codex"}}, "", "", "AIVOICEBUS_DETECT"},
		{"node script named like a tool but not its package or shim", nil, []Proc{{PID: 3, Exe: "/usr/bin/node", Script: "/opt/x/goose.js"}}, "", "", ""},
		{"a script in a directory that is not bin", nil, []Proc{{PID: 3, Exe: "/usr/bin/node", Script: "/srv/gemini"}}, "", "", ""},
		{"python script named like a tool outside bin", nil, []Proc{{PID: 3, Exe: "/usr/bin/python3", Script: "/home/u/aider.py"}}, "", "", ""},
		{"the Claude Desktop app is not a Claude Code session", nil, []Proc{{PID: 3, Exe: "/Applications/Claude.app/Contents/MacOS/Claude"}}, "", "", ""},
		{"claude -p inside codex, no process table: the tool with more markers of its own", []string{"CODEX_THREAD_ID", "t", "CLAUDECODE", "1", "CLAUDE_CODE_SESSION_ID", "abcd1234", "CLAUDE_CODE_ENTRYPOINT", "sdk-cli"}, nil, "claude", "abcd", "markers of codex, claude"},
		{"codex inside claude, no process table", []string{"CODEX_THREAD_ID", "t", "CODEX_SANDBOX", "seatbelt", "CLAUDECODE", "1"}, nil, "codex", "*", "markers of codex, claude"},
		{"no chain: markers still work", []string{"CODEX_THREAD_ID", "t1"}, []Proc{}, "codex", "*", ""},
	}
	for _, c := range cases {
		d := Detect(envOf(c.env...), c.chain)
		if d.Tool != c.tool {
			t.Errorf("%s: tool %q, want %q (%s)", c.name, d.Tool, c.tool, d.Why())
		}
		switch c.sess {
		case "":
			if d.Session != "" {
				t.Errorf("%s: session %q, want none", c.name, d.Session)
			}
		case "*":
			if len(d.Session) != 4 || strings.Trim(d.Session, "0123456789abcdef") != "" {
				t.Errorf("%s: session %q, want 4 hex chars", c.name, d.Session)
			}
		default:
			if d.Session != c.sess {
				t.Errorf("%s: session %q, want %q", c.name, d.Session, c.sess)
			}
		}
		if !strings.Contains(d.Why(), c.sourceHas) {
			t.Errorf("%s: why %q should mention %q", c.name, d.Why(), c.sourceHas)
		}
		if a := d.Agent(); !msg.ValidIdentity("host/"+a) || a != strings.ToLower(a) || strings.ContainsAny(a, " /") {
			t.Errorf("%s: agent %q is not a clean identity part", c.name, a)
		}
	}
}

func TestAgentShape(t *testing.T) {
	for d, want := range map[Detection]string{
		{}:                               "agent",
		{Tool: "codex"}:                  "codex",
		{Tool: "codex", Session: "q1w2"}: "codex.q1w2",
	} {
		if got := d.Agent(); got != want {
			t.Errorf("%+v: %q, want %q", d, got, want)
		}
	}
}

// One session must keep one identity: the same process gives the same suffix every time,
// another session (another pid or start time) gets another.
func TestSessionSuffixIsStablePerSession(t *testing.T) {
	chain := func(pid int, start string) []Proc {
		return []Proc{{PID: 1, Start: "z", Exe: "/bin/zsh"}, {PID: pid, Start: start, Exe: "/usr/bin/codex"}}
	}
	a := Detect(envOf(), chain(4242, "Tue Oct 14 09:00:00 2026")).Agent()
	for i := 0; i < 3; i++ {
		if again := Detect(envOf(), chain(4242, "Tue Oct 14 09:00:00 2026")).Agent(); again != a {
			t.Fatalf("same session, different identity: %q then %q", a, again)
		}
	}
	if other := Detect(envOf(), chain(4243, "Tue Oct 14 09:00:00 2026")).Agent(); other == a {
		t.Errorf("another pid kept the identity %q", a)
	}
	if other := Detect(envOf(), chain(4242, "Wed Oct 15 09:00:00 2026")).Agent(); other == a {
		t.Errorf("a reused pid at another start time kept the identity %q", a)
	}
	// with the tool's own thread id the suffix follows the id, whichever process asks
	x := Detect(envOf("CODEX_THREAD_ID", "t-1"), chain(1, "a")).Agent()
	y := Detect(envOf("CODEX_THREAD_ID", "t-1"), chain(2, "b")).Agent()
	z := Detect(envOf("CODEX_THREAD_ID", "t-2"), chain(1, "a")).Agent()
	if x != y || x == z {
		t.Errorf("thread id should decide the suffix: %q %q %q", x, y, z)
	}
}

func TestToolFromClient(t *testing.T) {
	for in, want := range map[string]string{
		"codex-mcp-client":      "codex",
		"claude-code":           "claude",
		"Claude Desktop":        "claude",
		"gemini-cli-mcp-client": "gemini",
		"antigravity":           "gemini",
		"agy-mcp":               "gemini",
		"cursor-vscode":         "cursor-agent",
		"opencode":              "opencode",
		"Grok":                  "grok",
		"inspector":             "",
		"":                      "",
	} {
		if got := ToolFromClient(in); got != want {
			t.Errorf("ToolFromClient(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToolOf(t *testing.T) {
	for in, want := range map[string]string{"claude.a1b2": "claude", "Codex": "codex", "agent": "", "build.bot": "", "mcp.x": ""} {
		if got := ToolOf(in); got != want {
			t.Errorf("ToolOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScriptFromArgv(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"node", "/x/gemini", "--flag"}, "/x/gemini"},
		{[]string{"node", "--inspect", "--no-warnings", "/x/gemini"}, "/x/gemini"},
		{[]string{"node"}, ""},
		{nil, ""},
	} {
		if got := scriptFromArgv(c.argv); got != c.want {
			t.Errorf("%v: %q, want %q", c.argv, got, c.want)
		}
	}
}

// resetDetection makes Me() see a fake process chain and none of the real markers
// (this test run may itself be inside Claude Code or Codex).
func resetDetection(t *testing.T, chain []Proc, env ...string) {
	t.Helper()
	for _, v := range []string{"AIVOICEBUS_DETECT", "AIVOICEBUS_AGENT_SRC", "AIVOICEBUS_ME", "AIVOICEBUS_AGENT", "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT",
		"CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", "CODEX_CI",
		"GEMINI_CLI", "GEMINI_SESSION_ID", "ANTIGRAVITY_CONVERSATION_ID", "ANTIGRAVITY_AGENT", "CURSOR_AGENT", "OPENCODE", "GOOSE_TERMINAL"} {
		t.Setenv(v, "")
	}
	for i := 0; i+1 < len(env); i += 2 {
		t.Setenv(env[i], env[i+1])
	}
	oldProbe := chainProbe
	chainProbe = func() []Proc { return chain }
	reset := func() { implicitOnce, implicitDet, chainRead, chainCache = sync.Once{}, Detection{}, sync.Once{}, nil }
	reset()
	t.Cleanup(func() { chainProbe = oldProbe; reset() })
}

func TestMeUsesTheDetectedTool(t *testing.T) {
	resetDetection(t, []Proc{{PID: 50, Start: "s", Exe: "/opt/bin/codex"}})
	got := Me()
	want := Label() + "/" + Detect(envOf(), []Proc{{PID: 50, Start: "s", Exe: "/opt/bin/codex"}}).Agent()
	if got != want || !strings.Contains(got, "/codex.") {
		t.Errorf("Me() = %q, want %q", got, want)
	}
	if !msg.ValidIdentity(got) {
		t.Errorf("not a valid identity: %q", got)
	}
	if Explain().Tool != "codex" {
		t.Errorf("Explain: %+v", Explain())
	}
	// AIVOICEBUS_AGENT still wins, and Explain says so
	t.Setenv("AIVOICEBUS_AGENT", "custom.1")
	if got := Me(); got != Label()+"/custom.1" {
		t.Errorf("explicit agent: %q", got)
	}
	if !strings.Contains(Explain().Why(), "AIVOICEBUS_AGENT") {
		t.Errorf("Explain: %q", Explain().Why())
	}
}

func TestMeUnknownToolStaysAgent(t *testing.T) {
	resetDetection(t, []Proc{{PID: 2, Exe: "/bin/zsh"}})
	if got := Me(); got != Label()+"/agent" {
		t.Errorf("Me() = %q", got)
	}
	if !strings.Contains(Explain().Why(), "no known agent tool") {
		t.Errorf("why: %q", Explain().Why())
	}
}

// The real probe must never fail or hang; what it finds depends on where the tests run.
func TestPlatformChainIsSane(t *testing.T) {
	ch := platformChain()
	if len(ch) > maxChain {
		t.Errorf("chain too long: %d", len(ch))
	}
	seen := map[int]bool{}
	for _, p := range ch {
		if p.PID <= 1 || seen[p.PID] {
			t.Errorf("bad or repeated pid in %+v", ch)
		}
		seen[p.PID] = true
	}
}

func TestParsePS(t *testing.T) {
	out := "    1     0 Mon Oct 13 08:00:01 2026     /sbin/launchd\n" +
		"87037 87036 Tue Oct 13 20:40:42 2026     /Users/x/Library/Application Support/Claude/claude-code/2.1.295/9d64/claude.app/Contents/MacOS/claude\n" +
		" 6973 87037 Wed Oct 14  9:05:07 2026     /bin/zsh\n" +
		"garbage line\n" +
		"  12 x Wed Oct 14  9:05:07 2026 /bin/bad\n"
	rows := parsePS(out)
	if len(rows) != 3 {
		t.Fatalf("rows: %+v", rows)
	}
	c := rows[87037]
	if c.ppid != 87036 || c.start != "Tue Oct 13 20:40:42 2026" || !strings.HasSuffix(c.exe, "/claude.app/Contents/MacOS/claude") || !strings.Contains(c.exe, "Application Support") {
		t.Errorf("claude row: %+v", c)
	}
	if z := rows[6973]; z.exe != "/bin/zsh" || z.start != "Wed Oct 14 9:05:07 2026" {
		t.Errorf("zsh row: %+v", z)
	}
	if procTool(Proc{Exe: c.exe}) != "claude" {
		t.Errorf("the real claude path is not recognised: %q", c.exe)
	}
}

func TestHookIdentityDoesNotLeakIntoAnotherTool(t *testing.T) {
	hook := []string{"AIVOICEBUS_AGENT", "claude.ab12", "AIVOICEBUS_AGENT_SRC", "hook"}
	claude := Proc{PID: 10, Start: "a", Exe: "/x/claude.app/Contents/MacOS/claude"}
	codex := Proc{PID: 20, Start: "b", Exe: "/x/codex"}
	cases := []struct {
		name, want string // want: the agent, "*codex" = codex.<4 hex>
		env        []string
		chain      []Proc
	}{
		{"claude's own commands keep the hook's identity", "claude.ab12", hook, []Proc{claude}},
		{"no tool visible at all: the hook's identity stays", "claude.ab12", hook, nil},
		{"codex started from a Claude command is codex", "*codex", hook, []Proc{codex, claude}},
		{"claude above a codex that is not nearest still: nearest wins", "claude.ab12", hook, []Proc{claude, codex}},
		{"markers only, ps blocked: codex markers beat the hook's claude", "*codex", append([]string{"CODEX_THREAD_ID", "t1"}, hook...), nil},
		{"only claude's own markers: the hook's identity stays", "claude.ab12", append([]string{"CLAUDECODE", "1"}, hook...), nil},
		{"detection off: the hook's identity stays", "claude.ab12", append([]string{"AIVOICEBUS_DETECT", "off"}, append(hook, "CODEX_CI", "1")...), []Proc{codex}},
		{"an agent the user set is never replaced", "claude.ab12", []string{"AIVOICEBUS_AGENT", "claude.ab12"}, []Proc{codex}},
		{"a hook agent of no known tool is kept", "build-bot", []string{"AIVOICEBUS_AGENT", "build-bot", "AIVOICEBUS_AGENT_SRC", "hook"}, []Proc{codex}},
	}
	for _, c := range cases {
		got := Detect(envOf(c.env...), c.chain).Agent()
		if c.want == "*codex" {
			if !strings.HasPrefix(got, "codex.") || len(got) != len("codex.")+4 {
				t.Errorf("%s: %q, want codex.<4 hex>", c.name, got)
			}
		} else if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	d := Detect(envOf(append([]string{}, hook...)...), []Proc{codex, claude})
	if !strings.Contains(d.Why(), "belongs to the tool above") {
		t.Errorf("why: %q", d.Why())
	}
}

// With a session id in the tool's environment, every command of the session agrees whether or
// not the process table could be read (a sandbox may block ps for some commands only).
func TestSessionIdGivesTheSameIdentityWithAndWithoutProcessTable(t *testing.T) {
	env := envOf("CODEX_THREAD_ID", "019a-thread", "CODEX_SANDBOX", "seatbelt")
	withPS := Detect(env, []Proc{{PID: 5, Start: "s", Exe: "/usr/bin/codex"}}).Agent()
	without := Detect(env, nil).Agent()
	if withPS != without || withPS == "codex" {
		t.Errorf("with ps %q, without %q", withPS, without)
	}
	// without any session id the two paths can differ: that is the documented limit
	bare := Detect(envOf("CODEX_SANDBOX", "seatbelt"), nil).Agent()
	if bare != "codex" {
		t.Errorf("no id, no table: %q", bare)
	}
}

func TestPrintableStripsControlCharacters(t *testing.T) {
	got := Detect(envOf("AIVOICEBUS_AGENT", "evil\x1b[2J\nx"), nil).Why()
	if strings.ContainsAny(got, "\x1b\n") {
		t.Errorf("control characters survive: %q", got)
	}
	if long := Printable(strings.Repeat("a", 500)); len(long) > 210 {
		t.Errorf("not truncated: %d", len(long))
	}
	if Printable("claude.a1b2") != "claude.a1b2" {
		t.Error("clean text changed")
	}
}

func TestMeFollowsTheHookOnlyForItsOwnTool(t *testing.T) {
	resetDetection(t, []Proc{{PID: 20, Start: "b", Exe: "/x/codex"}, {PID: 10, Start: "a", Exe: "/x/claude"}},
		"AIVOICEBUS_AGENT", "claude.ab12", "AIVOICEBUS_AGENT_SRC", "hook")
	if got := Me(); !strings.HasPrefix(got, Label()+"/codex.") {
		t.Errorf("codex under claude: %q", got)
	}
	resetDetection(t, []Proc{{PID: 10, Start: "a", Exe: "/x/claude"}},
		"AIVOICEBUS_AGENT", "claude.ab12", "AIVOICEBUS_AGENT_SRC", "hook")
	if got := Me(); got != Label()+"/claude.ab12" {
		t.Errorf("claude itself: %q", got)
	}
	resetDetection(t, []Proc{{PID: 20, Start: "b", Exe: "/x/codex"}},
		"AIVOICEBUS_AGENT", "claude.ab12") // set by the user: authoritative
	if got := Me(); got != Label()+"/claude.ab12" {
		t.Errorf("user-set agent replaced: %q", got)
	}
}

func TestAgentCallerIsTheEnvironmentOrAToolAboveNeverTurnedOff(t *testing.T) {
	codex := []Proc{{PID: 50, Start: "s", Exe: "/opt/bin/codex"}, {PID: 1, Exe: "/sbin/launchd"}}
	terminal := []Proc{{PID: 40, Exe: "/bin/zsh"}, {PID: 30, Exe: "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal"}}
	desktop := []Proc{{PID: 40, Exe: "/bin/zsh"}, {PID: 30, Exe: "/Applications/Claude.app/Contents/MacOS/Claude"}}
	for _, c := range []struct {
		what  string
		chain []Proc
		env   []string
		agent bool
	}{
		{"a terminal", terminal, nil, false},
		{"the Claude desktop app's terminal", desktop, nil, false},
		{"a terminal that names itself", terminal, []string{"AIVOICEBUS_ME", "air/florin"}, false},
		{"the Claude Code hook", terminal, []string{"AIVOICEBUS_AGENT", "claude.a1b2"}, true},
		{"under codex", codex, nil, true},
		{"under codex, named", codex, []string{"AIVOICEBUS_ME", "air/florin"}, true},
		{"under codex, detection off", codex, []string{"AIVOICEBUS_DETECT", "off"}, true},
		{"Claude Code's environment", terminal, []string{"CLAUDECODE", "1", "AIVOICEBUS_DETECT", "off"}, true},
	} {
		t.Run(c.what, func(t *testing.T) {
			resetDetection(t, c.chain, c.env...)
			if got, why := AgentCaller(); got != c.agent || got && why == "" {
				t.Errorf("AgentCaller() = %v %q, want %v", got, why, c.agent)
			}
		})
	}
}

// msg cannot import this package, so it keeps its own list of tool names: a tool's name addresses
// every session of it (host/codex), never one session, so a task for the person sent there stays theirs.
func TestEveryDetectedToolIsAToolToMsg(t *testing.T) {
	for _, tl := range tools {
		if !msg.KnownTool(tl.name) {
			t.Errorf("msg.KnownTool(%q) is false: add it to msg.knownTools", tl.name)
		}
	}
}
