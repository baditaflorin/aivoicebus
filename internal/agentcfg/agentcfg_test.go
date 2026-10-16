package agentcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

const testURL = "http://a:7777,http://b:7777"

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func statusOf(steps []Step, name string) string {
	for _, s := range steps {
		if s.Name == name {
			return s.Status
		}
	}
	return "<absent>"
}

func settingsDoc(t *testing.T, home string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(read(t, Paths{home}.Settings())), &doc); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	return doc
}

func TestSetupOnAFreshMachineThenIdempotent(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755) // codex installed, gemini not
	p := Paths{home}

	steps, err := Setup(Options{Home: home, URL: testURL})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, p.Env()); got != "export PATH=\"$HOME/.local/bin:$PATH\"\nexport AIVOICEBUS_URL='"+testURL+"'\n" {
		t.Errorf("env file:\n%s", got)
	}
	if !strings.Contains(read(t, p.Zshrc()), shellLine) {
		t.Error("zshrc does not source the env file")
	}
	if hookStatus(settingsDoc(t, home)) != "current" {
		t.Error("claude hook not installed")
	}
	if read(t, p.Instructions(Tools[0])) != Block() || read(t, p.Instructions(Tools[1])) != Block() {
		t.Error("claude/codex instruction block missing")
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini")); err == nil {
		t.Error("gemini is not installed; setup must not create ~/.gemini")
	}
	for name, want := range map[string]string{"env": Changed, "shell": Changed, "claude hook": Changed,
		"claude instructions": Changed, "codex instructions": Changed, "gemini instructions": Skipped, "token": Skipped, "state": Changed} {
		if got := statusOf(steps, name); got != want {
			t.Errorf("step %q = %s, want %s", name, got, want)
		}
	}

	before := map[string]string{}
	for _, f := range []string{p.Env(), p.Zshrc(), p.Settings(), p.Instructions(Tools[0]), p.Instructions(Tools[1]), p.State()} {
		before[f] = read(t, f)
	}
	steps, err = Setup(Options{Home: home, URL: testURL})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Status == Changed || s.Status == Failed {
			t.Errorf("second run should change nothing, but %q was %s", s.Name, s.Status)
		}
	}
	for f, b := range before {
		if read(t, f) != b {
			t.Errorf("%s changed on the second run", f)
		}
	}
	if matches, _ := filepath.Glob(p.Settings() + ".bak-aivoicebus-*"); len(matches) != 0 {
		t.Errorf("a brand-new settings.json needs no backup, found %v", matches)
	}
}

func TestSetupMigratesOldInstallsAndKeepsEverythingElse(t *testing.T) {
	home := t.TempDir()
	p := Paths{home}
	write(t, p.Settings(), `{
  "model": "opus",
  "hooks": {
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "echo pre"}]}],
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "[ -x \"$HOME/.local/bin/aivoicebus\" ] && \"$HOME/.local/bin/aivoicebus\" hook --tool claude || true", "timeout": 10}]},
      {"hooks": [{"type": "command", "command": "\"$HOME/.local/bin/aivoicebus-hook\"", "timeout": 5}, {"type": "command", "command": "echo keep-me"}]}
    ]
  }
}`)
	write(t, p.Instructions(Tools[0]), "# My rules\n\nBe nice.\n\n## aivoicebus: messages between agents on other machines\n\nold long text\nmore old text\n\n## Other section\n\nkeep me\n")

	steps, err := Setup(Options{Home: home, URL: testURL})
	if err != nil {
		t.Fatal(err)
	}
	if statusOf(steps, "claude hook") != Changed || statusOf(steps, "claude instructions") != Changed {
		t.Fatalf("expected migration, got %+v", steps)
	}

	doc := settingsDoc(t, home)
	if doc["model"] != "opus" {
		t.Error("unrelated setting lost")
	}
	raw, _ := json.Marshal(doc)
	if !strings.Contains(string(raw), `"echo pre"`) || !strings.Contains(string(raw), `"echo keep-me"`) {
		t.Errorf("other hooks were not preserved: %s", raw)
	}
	if n, want := strings.Count(string(raw), "aivoicebus"), strings.Count(HookCommand, "aivoicebus")+strings.Count(PromptHookCommand, "aivoicebus"); n != want {
		t.Errorf("exactly one aivoicebus hook per event expected, found %d mentions, want %d: %s", n, want, raw)
	}
	if hookStatus(doc) != "current" {
		t.Errorf("hook is %s", hookStatus(doc))
	}
	if m, _ := filepath.Glob(p.Settings() + ".bak-aivoicebus-*"); len(m) != 1 {
		t.Errorf("expected one backup of the old settings.json, got %v", m)
	}

	md := read(t, p.Instructions(Tools[0]))
	for _, keep := range []string{"# My rules", "Be nice.", "## Other section", "keep me"} {
		if !strings.Contains(md, keep) {
			t.Errorf("lost %q:\n%s", keep, md)
		}
	}
	if strings.Contains(md, "old long text") || strings.Count(md, beginMark) != 1 {
		t.Errorf("old section not replaced by exactly one block:\n%s", md)
	}
	if again, _ := Setup(Options{Home: home, URL: testURL}); statusOf(again, "claude instructions") != OK || statusOf(again, "claude hook") != OK {
		t.Errorf("migration is not idempotent: %+v", again)
	}
}

func TestSettingsThatAreNotValidJSONAreLeftAlone(t *testing.T) {
	home := t.TempDir()
	p := Paths{home}
	write(t, p.Settings(), "{not json")
	steps, err := Setup(Options{Home: home, URL: testURL})
	if err == nil || statusOf(steps, "claude hook") != Failed {
		t.Fatalf("expected a failed claude hook step, got err=%v steps=%+v", err, steps)
	}
	if read(t, p.Settings()) != "{not json" {
		t.Error("an unparseable settings.json must not be touched")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	os.MkdirAll(filepath.Join(home, ".config/aivoicebus"), 0o700)
	write(t, filepath.Join(home, ".config/aivoicebus/token"), "tok\n")
	os.Chmod(filepath.Join(home, ".config/aivoicebus/token"), 0o644)
	var before []string
	filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error { before = append(before, path); return nil })

	steps, err := Setup(Options{Home: home, URL: testURL, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if statusOf(steps, "env") != Changed || statusOf(steps, "claude hook") != Changed || statusOf(steps, "token") != Changed {
		t.Errorf("a dry run should report what would change: %+v", steps)
	}
	var after []string
	filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error { after = append(after, path); return nil })
	if len(before) != len(after) {
		t.Errorf("dry run created files: %v -> %v", before, after)
	}
	if fi, _ := os.Stat(filepath.Join(home, ".config/aivoicebus/token")); fi.Mode().Perm() != 0o644 {
		t.Error("dry run changed the token's mode")
	}
}

func TestTokenModeIsTightened(t *testing.T) {
	home := t.TempDir()
	p := Paths{home}
	write(t, p.Token(), strings.Repeat("x", 40))
	os.Chmod(p.Token(), 0o644)
	steps, _ := Setup(Options{Home: home, URL: testURL})
	if statusOf(steps, "token") != Changed {
		t.Fatalf("token step: %+v", steps)
	}
	if fi, _ := os.Stat(p.Token()); fi.Mode().Perm() != 0o600 {
		t.Errorf("token mode %04o, want 0600", fi.Mode().Perm())
	}
}

func TestSetupKeepsOtherLinesInTheEnvFile(t *testing.T) {
	home := t.TempDir()
	p := Paths{home}
	write(t, p.Env(), "export PATH=\"$HOME/.local/bin:$PATH\"\nexport AIVOICEBUS_URL=http://old:7777\nexport AIVOICEBUS_HOST=laptop-a16\n")
	if _, err := Setup(Options{Home: home, URL: testURL}); err != nil {
		t.Fatal(err)
	}
	got := read(t, p.Env())
	if !strings.Contains(got, "AIVOICEBUS_URL='"+testURL+"'\n") || strings.Contains(got, "old:7777") {
		t.Errorf("URL not replaced:\n%s", got)
	}
	if !strings.Contains(got, "export AIVOICEBUS_HOST=laptop-a16") || strings.Count(got, "export PATH=") != 1 {
		t.Errorf("other lines must survive, PATH must not be duplicated:\n%s", got)
	}
}

func TestSetupWithoutAnyURLSkipsTheEnvFile(t *testing.T) {
	t.Setenv("AIVOICEBUS_URL", "")
	home := t.TempDir()
	steps, _ := Setup(Options{Home: home})
	if statusOf(steps, "env") != Skipped || statusOf(steps, "shell") != Skipped {
		t.Errorf("no URL: env and shell should be skipped: %+v", steps)
	}
	if _, err := os.Stat(Paths{home}.Env()); err == nil {
		t.Error("env file must not be created without a URL")
	}
}

func TestApplyBlock(t *testing.T) {
	b := Block()
	stale := "<!-- aivoicebus:begin id=deadbeef -->\nold\n<!-- aivoicebus:end -->\n"
	cases := []struct{ name, in, want string }{
		{"empty", "", b},
		{"append", "# Mine\n\ntext\n", "# Mine\n\ntext\n\n" + b},
		{"append without trailing newline", "# Mine", "# Mine\n\n" + b},
		{"replace stale block", "before\n\n" + stale + "\nafter\n", "before\n\n" + b + "\nafter\n"},
		{"legacy at the start", "## aivoicebus: old\n\nold text\n\n## Next\n\nx\n", b + "\n## Next\n\nx\n"},
		{"legacy in the middle", "# T\n\n## aivoicebus: old\nold\n\n## Next\nx\n", "# T\n\n" + b + "\n## Next\nx\n"},
		{"legacy at the end", "# T\n\n## aivoicebus: old\nold text\n", "# T\n\n" + b},
		{"current block is unchanged", "x\n\n" + b, "x\n\n" + b},
	}
	for _, c := range cases {
		got, changed, _ := ApplyBlock(c.in, b)
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
		if changed != (got != c.in) {
			t.Errorf("%s: changed=%v but content differs=%v", c.name, changed, got != c.in)
		}
		if again, ch, _ := ApplyBlock(got, b); again != got || ch {
			t.Errorf("%s: not idempotent: %q", c.name, again)
		}
	}
}

func levelOf(cs []Check, name string) string {
	for _, c := range cs {
		if c.Name == name {
			return c.Level
		}
	}
	return "<absent>"
}

func TestDoctorFindsDriftAndTheFixClearsIt(t *testing.T) {
	t.Setenv("AIVOICEBUS_URL", "")
	t.Setenv("AIVOICEBUS_TOKEN", "")
	home := t.TempDir()
	p := Paths{home}
	opts := DoctorOptions{Home: home, Exe: p.Binary()}

	cs := Doctor(opts)
	if !HasFailures(cs) {
		t.Fatal("an unconfigured machine must fail")
	}
	for _, name := range []string{"env", "token", "claude hook", "claude instructions", "install"} {
		if levelOf(cs, name) != LevelFail {
			t.Errorf("%s should fail on a bare machine, got %s", name, levelOf(cs, name))
		}
	}

	// install the binary and token, then let setup do the rest
	write(t, p.Binary(), "binary")
	os.Chmod(p.Binary(), 0o755)
	write(t, p.Token(), strings.Repeat("t", 40))
	os.Chmod(p.Token(), 0o600)
	if _, err := Setup(Options{Home: home, URL: testURL}); err != nil {
		t.Fatal(err)
	}
	cs = Doctor(opts)
	if HasFailures(cs) {
		t.Fatalf("a set-up machine should pass: %+v", cs)
	}
	if levelOf(cs, "state") != LevelOK {
		t.Errorf("state: %s", levelOf(cs, "state"))
	}

	// drift: an old hook, an old snippet, a world-readable token
	write(t, p.Settings(), `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"aivoicebus hook --tool claude"}]}]}}`)
	write(t, p.Instructions(Tools[0]), "## aivoicebus: old\nold\n")
	os.Chmod(p.Token(), 0o644)
	cs = Doctor(opts)
	for _, name := range []string{"claude hook", "claude instructions", "token"} {
		if levelOf(cs, name) != LevelFail {
			t.Errorf("%s should fail after drift, got %s", name, levelOf(cs, name))
		}
	}
	if _, err := Setup(Options{Home: home}); err != nil { // what `doctor --fix` runs
		t.Fatal(err)
	}
	if cs = Doctor(opts); HasFailures(cs) {
		t.Fatalf("setup should clear the drift: %+v", cs)
	}
}

func TestDoctorMeshAndRunningCopyAreWarningsNotFailures(t *testing.T) {
	t.Setenv("AIVOICEBUS_URL", "")
	t.Setenv("AIVOICEBUS_TOKEN", "")
	home := t.TempDir()
	p := Paths{home}
	write(t, p.Binary(), "binary")
	os.Chmod(p.Binary(), 0o755)
	write(t, p.Token(), strings.Repeat("t", 40))
	os.Chmod(p.Token(), 0o600)
	if _, err := Setup(Options{Home: home, URL: testURL}); err != nil {
		t.Fatal(err)
	}
	cs := Doctor(DoctorOptions{Home: home, Exe: filepath.Join(t.TempDir(), "elsewhere"), Mesh: func() error { return errors.New("no route") }})
	if HasFailures(cs) {
		t.Fatalf("a down mesh or a copy running from elsewhere must only warn: %+v", cs)
	}
	if levelOf(cs, "mesh") != LevelWarn || levelOf(cs, "install") != LevelWarn {
		t.Errorf("mesh=%s install=%s, want warn/warn", levelOf(cs, "mesh"), levelOf(cs, "install"))
	}
}

func TestDoctorFailsOnARejectedTokenButOnlyWarnsWhenNobodyAnswers(t *testing.T) {
	t.Setenv("AIVOICEBUS_URL", "")
	t.Setenv("AIVOICEBUS_TOKEN", "")
	home := t.TempDir()
	p := Paths{home}
	write(t, p.Binary(), "binary")
	os.Chmod(p.Binary(), 0o755)
	write(t, p.Token(), strings.Repeat("t", 40))
	os.Chmod(p.Token(), 0o600)
	if _, err := Setup(Options{Home: home, URL: testURL}); err != nil {
		t.Fatal(err)
	}
	probe := func(err error) []Check {
		return Doctor(DoctorOptions{Home: home, Exe: p.Binary(), Mesh: func() error { return err }})
	}

	if cs := probe(nil); HasFailures(cs) || levelOf(cs, "mesh") != LevelOK {
		t.Errorf("an accepted token passes: %+v", cs)
	}
	cs := probe(errors.New("no node reachable: connection refused"))
	if HasFailures(cs) || levelOf(cs, "mesh") != LevelWarn {
		t.Errorf("nobody answering only warns: %+v", cs)
	}

	cs = probe(fmt.Errorf("%w: 401 Unauthorized: unauthorized", ErrTokenRejected))
	if !HasFailures(cs) || levelOf(cs, "mesh") != LevelFail {
		t.Fatalf("a rejected token is a failure: %+v", cs)
	}
	for _, c := range cs {
		if c.Name == "mesh" && (!strings.Contains(c.Detail, "401") || !strings.Contains(c.Detail, p.Token())) {
			t.Errorf("the failure says what was refused and where the token goes: %q", c.Detail)
		}
	}
	t.Setenv("AIVOICEBUS_TOKEN", "from-the-shell")
	for _, c := range probe(ErrTokenRejected) {
		if c.Name == "mesh" && !strings.Contains(c.Detail, "AIVOICEBUS_TOKEN") {
			t.Errorf("a token in the shell overrides the file, and doctor says so: %q", c.Detail)
		}
	}
}

func TestDoctorReportsWhatTheHooksLastFound(t *testing.T) {
	t.Setenv("AIVOICEBUS_URL", "")
	t.Setenv("AIVOICEBUS_TOKEN", "")
	home := t.TempDir()
	p := Paths{home}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	doctor := func() []Check {
		return Doctor(DoctorOptions{Home: home, Exe: p.Binary(), Now: func() time.Time { return now }})
	}
	detail := func(cs []Check) string {
		for _, c := range cs {
			if c.Name == "hook runs" {
				return c.Level + ": " + c.Detail
			}
		}
		return ""
	}
	save := func(st config.HookStatus) {
		t.Helper()
		if err := st.Save(p.HookStatus()); err != nil {
			t.Fatal(err)
		}
	}

	if got := detail(doctor()); got != "" {
		t.Errorf("a machine whose hooks have not run yet has nothing to report: %q", got)
	}
	save(config.HookStatus{LastRun: now.Add(-time.Minute), Outcome: config.HookOK, LastOK: now.Add(-time.Minute)})
	if got := detail(doctor()); got != "ok: last run ok" {
		t.Errorf("%q", got)
	}
	save(config.HookStatus{LastRun: now.Add(-3 * 24 * time.Hour), Outcome: config.HookOK})
	if got := detail(doctor()); got != "ok: last run ok, 3d ago" {
		t.Errorf("an old note says how old: %q", got)
	}
	save(config.HookStatus{LastRun: now.Add(-5 * time.Minute), Outcome: config.HookRejected, Failures: 4, LastOK: now.Add(-3 * time.Hour)})
	if got := detail(doctor()); !strings.HasPrefix(got, "warn: ") || !strings.Contains(got, "refused") ||
		!strings.Contains(got, "5m ago") || !strings.Contains(got, "4 failed in a row") || !strings.Contains(got, "3h ago") {
		t.Errorf("a refusal: %q", got)
	}
	save(config.HookStatus{LastRun: now, Outcome: config.HookUnreachable, Failures: 3})
	if got := detail(doctor()); !strings.HasPrefix(got, "warn: ") || !strings.Contains(got, "no node") || !strings.Contains(got, "last success never") {
		t.Errorf("unreachable: %q", got)
	}
	if levelOf(doctor(), "hook runs") == LevelFail {
		t.Error("the record alone never fails doctor: the live probe decides")
	}
	write(t, p.HookStatus(), "{ not json")
	if got := detail(doctor()); got != "" {
		t.Errorf("an unreadable note is ignored: %q", got)
	}
}

func TestEnsureHookInstallsEveryEventOnceAndKeepsOtherHooks(t *testing.T) {
	doc := map[string]any{
		"model": "opus",
		"hooks": map[string]any{
			"SessionStart":     []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo mine"}}}},
			"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo also mine"}}}},
		},
	}
	if hookStatus(doc) != "missing" {
		t.Fatalf("status %s", hookStatus(doc))
	}
	changed, err := ensureHook(doc)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	raw, _ := json.Marshal(doc)
	for _, want := range []string{"echo mine", "echo also mine", `"model":"opus"`, "hook --tool claude --event prompt"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %q in %s", want, raw)
		}
	}
	if hookStatus(doc) != "current" {
		t.Errorf("status %s", hookStatus(doc))
	}
	if again, err := ensureHook(doc); err != nil || again {
		t.Errorf("a second run changes nothing: %v %v", again, err)
	}

	// a machine set up before the prompt hook existed has only the first entry: stale, then current
	old := map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{
		map[string]any{"type": "command", "command": HookCommand, "timeout": float64(HookTimeout)}}}}}}
	if hookStatus(old) != "stale" {
		t.Errorf("only the session-start hook installed is stale, got %s", hookStatus(old))
	}
	if changed, err := ensureHook(old); err != nil || !changed || hookStatus(old) != "current" {
		t.Errorf("%v %v %s", changed, err, hookStatus(old))
	}
	if n := strings.Count(mustJSON(old), "aivoicebus"); n != strings.Count(HookCommand, "aivoicebus")+strings.Count(PromptHookCommand, "aivoicebus") {
		t.Errorf("the existing entry is kept once, not duplicated: %d mentions", n)
	}
	if _, err := ensureHook(map[string]any{"hooks": map[string]any{"UserPromptSubmit": "not a list"}}); err == nil {
		t.Error("a malformed settings file is left alone with an error")
	}
	if HookID() == "" || len(HookID()) != 8 {
		t.Errorf("hook id %q", HookID())
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestSetupInstallsTheGivenBinaryOnlyWhereNoneIsInstalled(t *testing.T) {
	home := t.TempDir()
	exe := filepath.Join(t.TempDir(), "running")
	write(t, exe, "new build")
	bin := Paths{home}.Binary()

	steps, err := Setup(Options{Home: home, URL: testURL, InstallExe: exe, DryRun: true})
	if err != nil || statusOf(steps, "install") != Changed {
		t.Fatalf("dry run: %v %+v", err, steps)
	}
	if _, err := os.Stat(bin); err == nil {
		t.Fatal("a dry run installed the binary")
	}

	if _, err := Setup(Options{Home: home, URL: testURL, InstallExe: exe}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(bin); err != nil || fi.Mode().Perm()&0o111 == 0 || read(t, bin) != "new build" {
		t.Fatalf("not installed as an executable copy: %v %v", fi, err)
	}

	write(t, exe, "newer build") // an installed copy is left to `update`
	steps, _ = Setup(Options{Home: home, URL: testURL, InstallExe: exe})
	if statusOf(steps, "install") != OK || read(t, bin) != "new build" {
		t.Errorf("replaced an installed binary: %+v", steps)
	}

	steps, err = Setup(Options{Home: home, URL: testURL, InstallExe: filepath.Join(home, "missing"), DryRun: false})
	if statusOf(steps, "install") != OK { // installed already: the source is not even read
		t.Errorf("%+v %v", steps, err)
	}
	os.Remove(bin)
	if steps, err = Setup(Options{Home: home, URL: testURL, InstallExe: filepath.Join(home, "missing")}); err == nil || statusOf(steps, "install") != Failed {
		t.Errorf("an unreadable source must fail the step: %+v %v", steps, err)
	}
}

func TestApplyBlockRefusesWhatItCouldNotReplaceWithoutLosingText(t *testing.T) {
	b := Block()
	cases := []struct{ name, in string }{
		{"begin marker without an end", "# Mine\n<!-- aivoicebus:begin id=x -->\nmy words\n"},
		{"end marker only before the begin", "<!-- aivoicebus:end -->\ntext\n<!-- aivoicebus:begin id=x -->\nmine\n"},
		{"legacy section with a heading of its own", "## aivoicebus: old\n\nold\n\n### mine\ntext\n\n## Next\n"},
		{"legacy section with a second paragraph", "## aivoicebus: old\n\nold\n\nmy own paragraph\n"},
		{"legacy section whose first paragraph follows a gap and then more text", "# T\n\n## aivoicebus: old\nold\n\nmine\n\n## Next\n"},
	}
	for _, c := range cases {
		got, changed, err := ApplyBlock(c.in, b)
		if err == nil || changed || got != c.in {
			t.Errorf("%s: err=%v changed=%v got %q", c.name, err, changed, got)
		}
	}
	// the installer's own single paragraph, with or without a gap after the heading, is still migrated
	for _, in := range []string{"## aivoicebus: old\nold\n", "## aivoicebus: old\n\nold\nmore\n\n## Next\n"} {
		if _, changed, err := ApplyBlock(in, b); err != nil || !changed {
			t.Errorf("%q: %v %v", in, changed, err)
		}
	}
}

func TestSetupLeavesAnInstructionFileItRefusedUntouchedAndSaysWhich(t *testing.T) {
	home := t.TempDir()
	p := Paths{home}
	orphan := "# Mine\n<!-- aivoicebus:begin id=x -->\nmy words\n"
	write(t, p.Instructions(Tools[0]), orphan)
	steps, err := Setup(Options{Home: home, URL: testURL})
	if err == nil || statusOf(steps, "claude instructions") != Failed {
		t.Fatalf("%v %+v", err, steps)
	}
	for _, s := range steps {
		if s.Name == "claude instructions" && !strings.Contains(s.Detail, p.Instructions(Tools[0])) {
			t.Errorf("does not say which file: %s", s.Detail)
		}
	}
	if read(t, p.Instructions(Tools[0])) != orphan {
		t.Error("the refused file was rewritten")
	}
	if m, _ := filepath.Glob(p.Instructions(Tools[0]) + ".bak-*"); len(m) != 0 {
		t.Errorf("a backup of an untouched file: %v", m)
	}
}

func TestSetupTreatsAnEmptyOrNonExecutableBinaryAsNotInstalled(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "running")
	write(t, exe, "new build")
	for name, setup := range map[string]func(bin string){
		"empty file":     func(bin string) { write(t, bin, ""); os.Chmod(bin, 0o755) },
		"not executable": func(bin string) { write(t, bin, "x"); os.Chmod(bin, 0o644) },
	} {
		home := t.TempDir()
		bin := Paths{home}.Binary()
		setup(bin)
		steps, err := Setup(Options{Home: home, URL: testURL, InstallExe: exe})
		if err != nil || statusOf(steps, "install") != Changed || read(t, bin) != "new build" {
			t.Errorf("%s: %v %+v", name, err, steps)
		}
	}
	// a directory is reported, never removed
	home := t.TempDir()
	bin := Paths{home}.Binary()
	os.MkdirAll(bin, 0o755)
	steps, err := Setup(Options{Home: home, URL: testURL, InstallExe: exe})
	if err == nil || statusOf(steps, "install") != Failed || !isDir(bin) || !strings.Contains(detailOf(steps, "install"), "is a directory") {
		t.Errorf("directory: %v %+v", err, steps)
	}
	// the running binary cannot be found: said, not skipped
	home = t.TempDir()
	steps, err = Setup(Options{Home: home, URL: testURL, InstallExeErr: errors.New("no /proc")})
	if err == nil || statusOf(steps, "install") != Failed || !strings.Contains(detailOf(steps, "install"), "cannot be found: no /proc") {
		t.Errorf("exe error: %v %+v", err, steps)
	}
	// ...but not when a good copy is already installed
	write(t, Paths{home}.Binary(), "ok")
	os.Chmod(Paths{home}.Binary(), 0o755)
	if steps, err := Setup(Options{Home: home, URL: testURL, InstallExeErr: errors.New("no /proc")}); err != nil || statusOf(steps, "install") != OK {
		t.Errorf("installed copy: %v %+v", err, steps)
	}
}

func detailOf(steps []Step, name string) string {
	for _, s := range steps {
		if s.Name == name {
			return s.Detail
		}
	}
	return ""
}
