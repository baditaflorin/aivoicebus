package agentcfg

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A machine that had settings, instructions and a shell file of its own gets them back exactly.
func TestUninstallLeavesTheMachineAsItWasBeforeSetup(t *testing.T) {
	home := t.TempDir()
	p := Paths{home}
	settings := `{
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "echo mine", "timeout": 3}]}
    ]
  }
}
`
	claudeMD := "# my notes\n\nbe brief\n"
	zshrc := "export EDITOR=vi\n"
	write(t, p.Settings(), settings)
	write(t, p.Instructions(Tools[0]), claudeMD)
	write(t, p.Zshrc(), zshrc)
	write(t, p.Token(), "tok-tok-tok-tok-tok-tok-tok-tok-tok\n")
	if _, err := Setup(Options{Home: home, URL: testURL}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, p.Settings()), "aivoicebus") || !strings.Contains(read(t, p.Instructions(Tools[0])), beginMark) {
		t.Fatal("setup did not install")
	}
	steps, err := Uninstall(RemoveOptions{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	var doc, want map[string]any
	if err := json.Unmarshal([]byte(read(t, p.Settings())), &doc); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(settings), &want)
	gotJ, _ := json.Marshal(doc)
	wantJ, _ := json.Marshal(want)
	if string(gotJ) != string(wantJ) {
		t.Errorf("settings.json: %s\nwant %s", gotJ, wantJ)
	}
	if got := read(t, p.Instructions(Tools[0])); got != claudeMD {
		t.Errorf("CLAUDE.md: %q, want %q", got, claudeMD)
	}
	if got := read(t, p.Zshrc()); got != zshrc {
		t.Errorf(".zshrc: %q, want %q", got, zshrc)
	}
	for _, f := range []string{p.Env(), p.State(), p.HookStatus()} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was left behind", f)
		}
	}
	if _, err := os.Stat(p.Token()); err != nil {
		t.Errorf("the token must stay without --purge: %v", err)
	}
	if statusOf(steps, "claude hook") != Changed {
		t.Errorf("steps: %+v", steps)
	}
	// a second run has nothing left to do
	steps, err = Uninstall(RemoveOptions{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Status == Changed {
			t.Errorf("not idempotent: %+v", s)
		}
	}
}

func TestUninstallDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	p := Paths{home}
	if _, err := Setup(Options{Home: home, URL: testURL}); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for _, f := range []string{p.Settings(), p.Instructions(Tools[0]), p.Zshrc(), p.Env(), p.State()} {
		before[f] = read(t, f)
	}
	steps, err := Uninstall(RemoveOptions{Home: home, DryRun: true, Purge: true})
	if err != nil {
		t.Fatal(err)
	}
	if statusOf(steps, "config dir") != Changed {
		t.Errorf("the plan should list the config dir: %+v", steps)
	}
	for f, b := range before {
		if read(t, f) != b {
			t.Errorf("%s changed in a dry run", f)
		}
	}
}

func TestUninstallRemovesTheFileItsBlockWasAloneIn(t *testing.T) {
	home := t.TempDir()
	p := Paths{home}
	if _, err := Setup(Options{Home: home, URL: testURL}); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(RemoveOptions{Home: home}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Instructions(Tools[0])); err == nil {
		t.Error("an instruction file that held only the block should go with it")
	}
}

func TestUninstallRefusesWhatItCannotVouchFor(t *testing.T) {
	home := t.TempDir()
	p := Paths{home}
	write(t, p.Settings(), "{not json")
	broken := "keep me\n" + beginMark + " id=x -->\nno end marker\n"
	write(t, p.Instructions(Tools[0]), broken)
	steps, err := Uninstall(RemoveOptions{Home: home})
	if err == nil {
		t.Fatal("a failure must be an error")
	}
	if statusOf(steps, "claude hook") != Failed || statusOf(steps, "claude instructions") != Failed {
		t.Errorf("steps: %+v", steps)
	}
	if read(t, p.Settings()) != "{not json" || read(t, p.Instructions(Tools[0])) != broken {
		t.Error("files it refused to change were changed")
	}
}

func TestUninstallPurgeRemovesTheConfigDirAndTheBinary(t *testing.T) {
	home := t.TempDir()
	p := Paths{home}
	if _, err := Setup(Options{Home: home, URL: testURL}); err != nil {
		t.Fatal(err)
	}
	write(t, p.Token(), "tok-tok-tok-tok-tok-tok-tok-tok-tok\n")
	write(t, p.Binary(), "binary")
	if _, err := Uninstall(RemoveOptions{Home: home, Purge: true}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p.ConfigDir(), p.Binary()} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s should be gone", f)
		}
	}
}

func TestUninstallOnlyTouchesTheRequestedTools(t *testing.T) {
	home := t.TempDir()
	p := Paths{home}
	write(t, p.join(".codex", "AGENTS.md"), "x\n")
	if _, err := Setup(Options{Home: home, URL: testURL}); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(RemoveOptions{Home: home, Tools: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, p.Settings()), "aivoicebus") {
		t.Error("the claude hook was removed though only codex was asked for")
	}
	if strings.Contains(read(t, p.join(".codex", "AGENTS.md")), beginMark) {
		t.Error("the codex block stayed")
	}
}
