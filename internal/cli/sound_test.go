package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// soundHome gives the test a HOME of its own, with taps not forced either way by the environment.
func soundHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("AIVOICEBUS_SOUND", "")
	return h
}

type soundStatusJSON struct {
	On          bool     `json:"on"`
	Saved       bool     `json:"saved"`
	Level       float64  `json:"level"`
	FollowFocus bool     `json:"follow_focus"`
	Focus       []string `json:"focus"`
	Events      []struct {
		Event   string `json:"event"`
		Pattern string `json:"pattern"`
		Chosen  bool   `json:"chosen"`
	} `json:"events"`
}

func soundJSON(t *testing.T) soundStatusJSON {
	t.Helper()
	code, out, e := run(t, "", "sound", "--json")
	var st soundStatusJSON
	if code != 0 || json.Unmarshal([]byte(out), &st) != nil {
		t.Fatalf("sound --json: code %d, %q %q", code, out, e)
	}
	return st
}

func TestSoundIsOffUntilTheyTurnItOnAndOffAgain(t *testing.T) {
	soundHome(t)
	code, out, _ := run(t, "", "sound")
	if code != 0 || !strings.Contains(out, "sounds: off") || !strings.Contains(out, "aivoicebus sound on") || !strings.Contains(out, "double (default)") {
		t.Fatalf("a fresh machine is still and says how to change that: %d %q", code, out)
	}
	if st := soundJSON(t); st.On || st.Level != 0.8 || len(st.Events) != 3 || st.Events[0].Event != "sent" || st.Events[1].Event != "received" || st.Events[2].Event != "retracted" {
		t.Fatalf("%+v", st)
	}
	if runtime.GOOS != "darwin" {
		// only a Mac has the motor: turning it on elsewhere is refused with the reason, and nothing is saved
		for _, w := range []string{"on", "toggle", "setup", "test"} {
			if code, _, e := run(t, "", "sound", w); code != exitUsage || !strings.Contains(e, "macOS only") {
				t.Fatalf("%s: code %d, %q", w, code, e)
			}
		}
		if soundJSON(t).On {
			t.Fatal("a refused `on` saved nothing")
		}
	}
	// `on`, `toggle` and `test` on a Mac build the helper and tap the real trackpad: not for a test
	if code, out, _ := run(t, "", "sound", "off"); code != 0 || !strings.Contains(out, "sounds: off") || soundJSON(t).On {
		t.Fatalf("off is always allowed: %d %q", code, out)
	}
}

func TestTheEnvironmentOverridesTheSavedSettingAndStatusSaysSo(t *testing.T) {
	soundHome(t)
	t.Setenv("AIVOICEBUS_SOUND", "1")
	code, out, _ := run(t, "", "sound")
	if code != 0 || !strings.Contains(out, "sounds: on (AIVOICEBUS_SOUND=1 overrides the saved setting, which is false)") {
		t.Fatalf("%d %q", code, out)
	}
	if st := soundJSON(t); !st.On || st.Saved {
		t.Fatalf("on for this process, off in the file: %+v", st)
	}
	t.Setenv("AIVOICEBUS_SOUND", "off")
	if code, out, _ := run(t, "", "sound", "off"); code != 0 || strings.Contains(out, "note:") {
		t.Fatalf("an override that agrees needs no note: %d %q", code, out)
	}
}

func TestSoundSetAndLevelAreCheckedAtOnce(t *testing.T) {
	h := soundHome(t)
	for name, args := range map[string][]string{
		"an unknown event":     {"sound", "set", "sneezed", "off"},
		"an unknown pattern":   {"sound", "set", "sent", "klaxon"},
		"too few arguments":    {"sound", "set", "sent"},
		"a level of nought":    {"sound", "level", "0"},
		"a level too high":     {"sound", "level", "3"},
		"not a number":         {"sound", "level", "loud"},
		"an unknown word":      {"sound", "shout"},
		"too many to test":     {"sound", "test", "sent", "tick"},
		"the old volume":       {"sound", "volume", "50%"},
		"a path is no pattern": {"sound", "set", "sent", "/System/Library/Sounds/Pop.aiff"},
	} {
		if code, _, e := run(t, "", args...); code != exitUsage {
			t.Errorf("%s: code %d, %q", name, code, e)
		}
	}
	if code, out, _ := run(t, "", "sound", "set", "retracted", "THUD"); code != 0 || !strings.Contains(out, "retracted: thud") {
		t.Fatalf("a pattern in any case: %d %q", code, out)
	}
	if st := soundJSON(t); st.Events[2].Pattern != "thud" || !st.Events[2].Chosen || st.Events[0].Pattern != "tick" || st.Events[0].Chosen {
		t.Fatalf("%+v", st)
	}
	if code, _, _ := run(t, "", "sound", "set", "retracted", "off"); code != 0 || soundJSON(t).Events[2].Pattern != "off" {
		t.Fatal("silencing one event is always allowed")
	}
	if code, _, _ := run(t, "", "sound", "set", "retracted", "default"); code != 0 || soundJSON(t).Events[2].Pattern != "fall" || soundJSON(t).Events[2].Chosen {
		t.Fatal("default puts the release's own pattern back")
	}
	if code, out, _ := run(t, "", "sound", "level", "50%"); code != 0 || !strings.Contains(out, "0.50") || soundJSON(t).Level != 0.5 {
		t.Fatalf("a percentage is a level: %d %q", code, out)
	}
	if code, out, _ := run(t, "", "sound", "level", "default"); code != 0 || !strings.Contains(out, "default") || soundJSON(t).Level != 0.8 {
		t.Fatalf("default: %d %q", code, out)
	}
	if b, err := os.ReadFile(filepath.Join(h, ".config", "aivoicebus", "sound")); err != nil || !strings.Contains(string(b), `"on"`) {
		t.Fatalf("the setting is one small file: %q %v", b, err)
	}
}

func TestSoundListNamesEveryPattern(t *testing.T) {
	soundHome(t)
	code, out, _ := run(t, "", "sound", "list")
	if code != 0 {
		t.Fatal(code)
	}
	for _, name := range []string{"tick", "tock", "double", "rise", "fall", "thud", "buzz"} {
		if !strings.Contains(out, name+" ") {
			t.Errorf("%s is not listed: %q", name, out)
		}
	}
}

func TestSoundIsInHelp(t *testing.T) {
	if _, out, _ := run(t, "", "help"); !strings.Contains(out, "sound") {
		t.Error("help lists the command")
	}
}

func TestSoundFocusShowsAndSetsWhetherTapsWaitForAFocus(t *testing.T) {
	soundHome(t)
	code, out, _ := run(t, "", "sound", "focus")
	if code != 0 || !strings.Contains(out, "focus: none is on") || !strings.Contains(out, "Sleep, Personal") {
		t.Fatalf("by default taps follow the Mac's Focus: %d %q", code, out)
	}
	if st := soundJSON(t); !st.FollowFocus || st.Focus == nil || len(st.Focus) != 0 {
		t.Fatalf("%+v", st)
	}
	if code, out, _ := run(t, "", "sound", "focus", "ignore"); code != 0 || !strings.Contains(out, "focus: ignored") || soundJSON(t).FollowFocus {
		t.Fatalf("ignore: %d %q", code, out)
	}
	if _, out, _ := run(t, "", "sound"); !strings.Contains(out, "focus: ignored") {
		t.Fatalf("status says it too: %q", out)
	}
	if code, out, _ := run(t, "", "sound", "focus", "FOLLOW"); code != 0 || !strings.Contains(out, "focus: none is on") || !soundJSON(t).FollowFocus {
		t.Fatalf("follow: %d %q", code, out)
	}
	for _, args := range [][]string{{"sound", "focus", "maybe"}, {"sound", "focus", "follow", "ignore"}} {
		if code, _, e := run(t, "", args...); code != exitUsage {
			t.Errorf("%v: code %d, %q", args, code, e)
		}
	}
}

// On a Mac the state is read from the person's own library; elsewhere there is none, so this runs there.
func TestSoundSaysWhichFocusModesAreOn(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only a Mac keeps a Focus state")
	}
	h := soundHome(t)
	dir := filepath.Join(h, "Library", "DoNotDisturb", "DB")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := `{"data":[{"storeAssertionRecords":[` +
		`{"assertionUUID":"A","assertionDetails":{"assertionDetailsModeIdentifier":"com.apple.sleep.sleep-mode"}},` +
		`{"assertionUUID":"B","assertionDetails":{"assertionDetailsModeIdentifier":"com.apple.focus.personal-time"}}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "Assertions.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, "", "sound", "focus"); code != 0 || !strings.Contains(out, "focus: Personal, Sleep are on: taps are silent until it ends") {
		t.Fatalf("%d %q", code, out)
	}
	if st := soundJSON(t); len(st.Focus) != 2 || st.Focus[0] != "Personal" || st.Focus[1] != "Sleep" {
		t.Fatalf("%+v", st)
	}
	if _, out, _ := run(t, "", "sound"); !strings.Contains(out, "Personal, Sleep are on") {
		t.Fatalf("status says it too: %q", out)
	}
}
