package identity

import (
	"os"
	"strings"
	"testing"
)

func registered(t *testing.T, names ...string) {
	t.Helper()
	old := nameRegistered
	t.Cleanup(func() { nameRegistered = old })
	nameRegistered = func(n string) bool {
		for _, r := range names {
			if r == n {
				return true
			}
		}
		return false
	}
}

func TestARegisteredNameIsTheIdentityWhateverTheSession(t *testing.T) {
	registered(t, "codex.build1")
	codex := func(pid int, start string) []Proc {
		return []Proc{{PID: pid, Start: start, Exe: "/usr/bin/codex"}}
	}
	env := envOf("AIVOICEBUS_NAME", "Codex.Build1")
	a := Detect(env, codex(4242, "Tue Oct 14 09:00:00 2026"))
	b := Detect(env, codex(9999, "Wed Oct 15 11:11:11 2026")) // restarted: another pid, another start
	if a.Agent() != "codex.build1" || b.Agent() != a.Agent() || a.Tool != "codex" || !strings.Contains(a.Source, "AIVOICEBUS_NAME") {
		t.Errorf("%+v %+v", a, b)
	}
	if auto := Detect(envOf(), codex(4242, "t")); auto.Agent() == "codex.build1" || len(auto.Session) > 4 {
		t.Errorf("an automatic identity must never be a chosen one: %+v", auto)
	}
}

func TestANameIsUsedOnlyWhenEveryConditionHolds(t *testing.T) {
	registered(t, "codex.build1", "mybot.nightly")
	claude := []Proc{{PID: 7, Start: "s", Exe: "/usr/bin/claude"}}
	for _, c := range []struct {
		name  string
		env   []string
		chain []Proc
		want  string // "" = the name is not used
		why   string // in Source when ignored
	}{
		{"registered, tool matches", []string{"AIVOICEBUS_NAME", "codex.build1", "CODEX_CI", "1"}, nil, "codex.build1", ""},
		{"not registered", []string{"AIVOICEBUS_NAME", "codex.other99"}, nil, "", "not registered"},
		{"invalid shape", []string{"AIVOICEBUS_NAME", "codex.a1b2"}, nil, "", "at least"},
		{"another tool is running", []string{"AIVOICEBUS_NAME", "codex.build1"}, claude, "", "different tool"},
		{"a name for no known tool is used when no tool is detected", []string{"AIVOICEBUS_NAME", "mybot.nightly"}, nil, "mybot.nightly", ""},
		{"a name for no known tool still needs the registry", []string{"AIVOICEBUS_NAME", "mybot.weekly"}, nil, "", "not registered"},
		{"but not over a detected tool", []string{"AIVOICEBUS_NAME", "mybot.nightly"}, claude, "", "different tool"},
		{"AIVOICEBUS_AGENT is explicit and wins", []string{"AIVOICEBUS_NAME", "codex.build1", "AIVOICEBUS_AGENT", "build-bot"}, nil, "build-bot", ""},
		{"AIVOICEBUS_ME wins over everything", []string{"AIVOICEBUS_NAME", "codex.build1", "AIVOICEBUS_ME", "a/b"}, nil, "", "AIVOICEBUS_ME"},
	} {
		d := Detect(envOf(c.env...), c.chain)
		got := d.Agent()
		if c.want == "" {
			if got == "codex.build1" || got == "mybot.nightly" {
				t.Errorf("%s: used %q", c.name, got)
			}
			if !strings.Contains(d.Source, c.why) {
				t.Errorf("%s: source %q says nothing about %q", c.name, d.Source, c.why)
			}
		} else if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNamedAgentIsForTheHooksOwnToolOnly(t *testing.T) {
	registered(t, "codex.build1")
	t.Setenv("AIVOICEBUS_NAME", "codex.build1")
	if NamedAgent("codex") != "codex.build1" || NamedAgent("claude") != "" {
		t.Error("a Codex name must not label a Claude session")
	}
	t.Setenv("AIVOICEBUS_NAME", "")
	if NamedAgent("codex") != "" {
		t.Error("no request, no name")
	}
}

func TestTheRegistryIsPerMachineAndSurvivesRestarts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if len(RegisteredNames()) != 0 {
		t.Fatal("empty at first")
	}
	for _, n := range []string{"codex.build1", "Claude.review", "codex.build1"} { // twice is fine
		if err := AddName(n); err != nil {
			t.Fatal(n, err)
		}
	}
	if got := strings.Join(RegisteredNames(), ","); got != "codex.build1,claude.review" {
		t.Errorf("%q", got)
	}
	if AddName("codex.a1b2") == nil || AddName("aivoicebus.build1") == nil {
		t.Error("an automatic-shaped or reserved name must be refused")
	}
	if err := RemoveName("codex.build1"); err != nil || RemoveName("codex.build1") == nil {
		t.Error("remove once")
	}
	if got := strings.Join(RegisteredNames(), ","); got != "claude.review" {
		t.Errorf("%q", got)
	}
	// a hand-edited file with junk keeps the good lines
	path := os.Getenv("HOME") + "/.config/aivoicebus/names"
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(b, []byte("junk line\ncodex.аbcde\ncodex.ok123 # note\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(RegisteredNames(), ","); got != "claude.review,codex.ok123" {
		t.Errorf("%q", got)
	}
}

func TestEveryAutomaticSuffixIsShorterThanAChosenOne(t *testing.T) {
	chain := []Proc{{PID: 4242, Start: "t", Exe: "/usr/bin/codex"}}
	for _, env := range [][]string{nil, {"CODEX_THREAD_ID", "0199-aaaa-bbbb-cccc-dddd-eeee"}, {"CLAUDE_CODE_SESSION_ID", "9058aa53-5065-4bc2", "CLAUDECODE", "1"}} {
		if d := Detect(envOf(env...), chain); len(d.Session) >= 5 {
			t.Errorf("%v: suffix %q could equal a chosen name", env, d.Session)
		}
	}
}

func TestTheRoleLabelComesFromAIVOICEBUS_ROLE(t *testing.T) {
	fakeGit(t, "alpha", "main")
	t.Setenv("AIVOICEBUS_ROLE", "Build, review,here,build,a=b")
	if l := labelsFor(t, ""); l["role"] != "build+review" {
		t.Errorf("%v", l)
	}
	t.Setenv("AIVOICEBUS_ROLE", "")
	if l := labelsFor(t, ""); l["role"] != "" {
		t.Errorf("%v", l)
	}
	t.Setenv("AIVOICEBUS_ROLE", "build")
	if l := labelsFor(t, "role=lint"); l["role"] != "lint" {
		t.Errorf("a pinned AIVOICEBUS_LABELS is laid over what is derived: %v", l)
	}
}

func TestTheRegistryRefusesALinkAndAHugeFileAndIsPrivate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := AddName("codex.build1"); err != nil {
		t.Fatal(err)
	}
	path := home + "/.config/aivoicebus/names"
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the registry is private: %v %v", fi, err)
	}
	// a symlink is not followed: nothing registered, nothing written through it
	target := home + "/elsewhere"
	os.WriteFile(target, []byte("codex.linked1\n"), 0o644)
	os.Remove(path)
	if err := os.Symlink(target, path); err != nil {
		t.Skip(err)
	}
	if len(RegisteredNames()) != 0 || AddName("codex.build2") == nil {
		t.Error("a symlinked registry must be refused")
	}
	if b, _ := os.ReadFile(target); string(b) != "codex.linked1\n" {
		t.Errorf("written through the link: %q", b)
	}
	// a file over 64 KiB registers nothing
	os.Remove(path)
	os.WriteFile(path, []byte(strings.Repeat("# padding\n", 8000)+"codex.big12345\n"), 0o600)
	if len(RegisteredNames()) != 0 {
		t.Error("an oversized registry must be refused")
	}
}
