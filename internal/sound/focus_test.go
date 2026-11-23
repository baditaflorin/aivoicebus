package sound

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// writeFocus writes the Mac's Focus state as macOS keeps it: the assertions that are on, and a record of one
// that was turned off (which must never count).
func writeFocus(t *testing.T, active ...[2]string) {
	t.Helper()
	type rec struct {
		UUID    string `json:"assertionUUID"`
		Details struct {
			Mode string `json:"assertionDetailsModeIdentifier"`
		} `json:"assertionDetails"`
	}
	var on []rec
	for _, a := range active {
		r := rec{UUID: a[0]}
		r.Details.Mode = a[1]
		on = append(on, r)
	}
	off := map[string]any{"invalidationAssertion": map[string]any{
		"assertionUUID":    "OFF-1",
		"assertionDetails": map[string]any{"assertionDetailsModeIdentifier": "com.apple.donotdisturb.mode.default"},
	}, "invalidationReason": "user-changed-state"}
	b, err := json.Marshal(map[string]any{
		"header": map[string]any{"version": 8},
		"data":   []any{map[string]any{"storeAssertionRecords": on, "storeInvalidationRecords": []any{off}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, string(b))
}

func writeRaw(t *testing.T, text string) {
	t.Helper()
	p := focusFile()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNoFocusFileMeansNoFocus(t *testing.T) {
	setup(t)
	if modes, err := ActiveFocus(); err != nil || len(modes) != 0 {
		t.Fatalf("a Mac that never used Focus: %v %v", modes, err)
	}
}

func TestActiveFocusNamesWhatIsOnOnce(t *testing.T) {
	setup(t)
	writeFocus(t,
		[2]string{"A", "com.apple.sleep.sleep-mode"},
		[2]string{"B", "com.apple.focus.personal-time"},
		[2]string{"B", "com.apple.focus.personal-time"}, // the file lists an assertion twice when two stores hold it
		[2]string{"C", "com.apple.focus.custom.9F3A"},   // a Focus the person made
		[2]string{"D", ""},
	)
	got, err := ActiveFocus()
	if err != nil || !reflect.DeepEqual(got, []string{"9F3A", "Personal", "Sleep", "a Focus"}) {
		t.Fatalf("%v %v (a turned-off Do Not Disturb must not be listed)", got, err)
	}
	writeFocus(t)
	if got, err := ActiveFocus(); err != nil || len(got) != 0 {
		t.Fatalf("nothing on: %v %v", got, err)
	}
}

func TestOnlyAMacHasFocusState(t *testing.T) {
	setup(t)
	writeFocus(t, [2]string{"A", "com.apple.sleep.sleep-mode"})
	goos = "linux"
	if got, err := ActiveFocus(); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestATapWaitsForTheFocusToEndWhateverTheMessage(t *testing.T) {
	f, clock := setup(t)
	turnOn(t)
	writeFocus(t, [2]string{"A", "com.apple.sleep.sleep-mode"})
	Play(Sent, false)
	Play(Received, true) // urgent gets through quiet hours, not a Focus
	Play(Retracted, false)
	if len(f.started) != 0 {
		t.Fatalf("a Focus is on: %v", f.started)
	}
	writeFocus(t) // the Focus ends; a silenced tap did not use up the event's gap
	Play(Sent, false)
	if len(f.started) != 1 {
		t.Fatalf("after the Focus: %v", f.started)
	}
	*clock = clock.Add(time.Second)
	writeFocus(t, [2]string{"A", "com.apple.focus.personal-time"})
	Play(Sent, false)
	if len(f.started) != 1 {
		t.Fatalf("Personal is a Focus too: %v", f.started)
	}
}

func TestAPersonCanTapThroughAFocus(t *testing.T) {
	f, _ := setup(t)
	turnOn(t)
	if err := Update(func(s *Settings) error { s.IgnoreFocus = true; return nil }); err != nil {
		t.Fatal(err)
	}
	writeFocus(t, [2]string{"A", "com.apple.sleep.sleep-mode"})
	Play(Sent, false)
	if len(f.started) != 1 {
		t.Fatalf("ignoring Focus: %v", f.started)
	}
	if !Load().IgnoreFocus {
		t.Fatal("the choice is saved")
	}
}

func TestATestTapsThroughAFocus(t *testing.T) {
	f, _ := setup(t)
	writeFocus(t, [2]string{"A", "com.apple.sleep.sleep-mode"})
	if err := Test(Sent); err != nil || len(f.ran) != 1 {
		t.Fatalf("a person trying it feels it: %v %v", err, f.ran)
	}
}

func TestAFocusStateThatCannotBeReadIsSaidButDoesNotSilence(t *testing.T) {
	f, _ := setup(t)
	turnOn(t)
	writeRaw(t, "{not json")
	if _, err := ActiveFocus(); err == nil || !strings.Contains(err.Error(), "cannot understand") {
		t.Fatalf("%v", err)
	}
	Play(Sent, false)
	if len(f.started) != 1 {
		t.Fatalf("not knowing is not a Focus: %v", f.started)
	}
	writeRaw(t, strings.Repeat(" ", maxFocusFile+1))
	if _, err := ActiveFocus(); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("%v", err)
	}
}

func TestAFocusStateMacOSKeepsPrivateIsSaidWithTheFix(t *testing.T) {
	setup(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	writeFocus(t, [2]string{"A", "com.apple.sleep.sleep-mode"})
	if err := os.Chmod(focusFile(), 0); err != nil {
		t.Fatal(err)
	}
	_, err := ActiveFocus()
	if err == nil || !strings.Contains(err.Error(), "Full Disk Access") || !strings.Contains(err.Error(), "sound focus ignore") {
		t.Fatalf("a refusal says how to fix it: %v", err)
	}
}
