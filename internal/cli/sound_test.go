package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// soundHome gives the test a HOME of its own, with sounds not forced either way by the environment.
func soundHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("AIVOICEBUS_SOUND", "")
	return h
}

type soundStatusJSON struct {
	On     bool    `json:"on"`
	Saved  bool    `json:"saved"`
	Volume float64 `json:"volume"`
	Events []struct {
		Event string `json:"event"`
		Sound string `json:"sound"`
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
	if code != 0 || !strings.Contains(out, "sounds: off") || !strings.Contains(out, "aivoicebus sound on") {
		t.Fatalf("a fresh machine is silent and says how to change that: %d %q", code, out)
	}
	if st := soundJSON(t); st.On || len(st.Events) != 3 || st.Events[0].Event != "sent" || st.Events[1].Event != "received" || st.Events[2].Event != "retracted" {
		t.Fatalf("%+v", st)
	}
	if runtime.GOOS != "darwin" {
		// only a Mac plays: turning them on elsewhere is refused with the reason, and nothing is saved
		if code, _, e := run(t, "", "sound", "on"); code != exitUsage || !strings.Contains(e, "macOS only") {
			t.Fatalf("code %d, %q", code, e)
		}
		if soundJSON(t).On {
			t.Fatal("a refused `on` saved nothing")
		}
	} else {
		if code, out, e := run(t, "", "sound", "on"); code != 0 || !strings.Contains(out, "sounds: on") {
			t.Fatalf("on: %d %q %q", code, out, e)
		}
		if st := soundJSON(t); !st.On || !st.Saved {
			t.Fatalf("%+v", st)
		}
		if code, out, _ := run(t, "", "sound", "toggle"); code != 0 || !strings.Contains(out, "sounds: off") || soundJSON(t).On {
			t.Fatalf("toggle turns an on setting off: %d %q", code, out)
		}
		if code, out, _ := run(t, "", "sound", "toggle"); code != 0 || !strings.Contains(out, "sounds: on") || !soundJSON(t).On {
			t.Fatalf("and an off one on: %d %q", code, out)
		}
	}
	if code, out, _ := run(t, "", "sound", "off"); code != 0 || !strings.Contains(out, "sounds: off") || soundJSON(t).On {
		t.Fatalf("off: %d %q", code, out)
	}
}

func TestSoundOffWorksAnywhereAndToggleFromOff(t *testing.T) {
	soundHome(t)
	// `off` and `toggle` never need a player: toggling from off is the one that asks for one
	if code, _, _ := run(t, "", "sound", "off"); code != 0 {
		t.Fatal("off is always allowed")
	}
	if runtime.GOOS != "darwin" {
		if code, _, e := run(t, "", "sound", "toggle"); code != exitUsage || !strings.Contains(e, "macOS only") {
			t.Fatalf("toggle from off is a turn on, refused here: %d %q", code, e)
		}
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

func TestSoundSetAndVolumeAreCheckedAtOnce(t *testing.T) {
	h := soundHome(t)
	for name, args := range map[string][]string{
		"an unknown event":   {"sound", "set", "sneezed", "off"},
		"too few arguments":  {"sound", "set", "sent"},
		"a volume of nought": {"sound", "volume", "0"},
		"a volume too high":  {"sound", "volume", "3"},
		"not a number":       {"sound", "volume", "loud"},
		"an unknown word":    {"sound", "shout"},
		"a test of nothing":  {"sound", "test", "sneezed"},
	} {
		if code, _, e := run(t, "", args...); code != exitUsage {
			t.Errorf("%s: code %d, %q", name, code, e)
		}
	}
	if code, _, _ := run(t, "", "sound", "set", "retracted", "off"); code != 0 {
		t.Fatal("silencing one event is always allowed")
	}
	if st := soundJSON(t); st.Events[2].Sound != "off" || st.Events[0].Sound != "default" {
		t.Fatalf("%+v", st)
	}
	if code, _, _ := run(t, "", "sound", "set", "retracted", "default"); code != 0 || soundJSON(t).Events[2].Sound != "default" {
		t.Fatal("default puts the release's own sound back")
	}
	if code, out, _ := run(t, "", "sound", "volume", "50%"); code != 0 || !strings.Contains(out, "0.50") || soundJSON(t).Volume != 0.5 {
		t.Fatalf("a percentage is a volume: %d %q", code, out)
	}
	if code, out, _ := run(t, "", "sound", "volume", "full"); code != 0 || !strings.Contains(out, "full") || soundJSON(t).Volume != 1 {
		t.Fatalf("full: %d %q", code, out)
	}
	if b, err := os.ReadFile(filepath.Join(h, ".config", "aivoicebus", "sound")); err != nil || !strings.Contains(string(b), `"on"`) {
		t.Fatalf("the setting is one small file: %q %v", b, err)
	}
}

func TestSoundIsInHelp(t *testing.T) {
	if _, out, _ := run(t, "", "help"); !strings.Contains(out, "sound") {
		t.Error("help lists the command")
	}
}
