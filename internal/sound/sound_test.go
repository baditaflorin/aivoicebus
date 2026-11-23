package sound

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

// fake is a player that records command lines instead of making a noise.
type fake struct {
	lines []string
}

// setup puts the package on a pretend Mac with its own HOME: three sounds in the person's own folder (where
// a lookup looks first, so the test needs nothing of the real system's), a recording player, and a clock the
// test moves. It leaves AIVOICEBUS_SOUND unset.
func setup(t *testing.T) (*fake, *time.Time) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv(envName, "")
	dir := filepath.Join(h, "Library", "Sounds")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Pop", "Glass", "Funk", "Tink"} {
		if err := os.WriteFile(filepath.Join(dir, n+".aiff"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &fake{}
	clock := time.Now().Truncate(time.Second) // the real time: SaveQuiet drops a time that is past by the real clock
	oldGoos, oldNow, oldStart, oldRun, oldHome := goos, now, start, run, home
	goos, now = "darwin", func() time.Time { return clock }
	record := func(c *exec.Cmd) error { f.lines = append(f.lines, strings.Join(c.Args, " ")); return nil }
	start, run = record, record
	home = func() string { return h }
	gapMu.Lock()
	played = map[Event]time.Time{}
	gapMu.Unlock()
	t.Cleanup(func() { goos, now, start, run, home = oldGoos, oldNow, oldStart, oldRun, oldHome })
	return f, &clock
}

func turnOn(t *testing.T) {
	t.Helper()
	if err := Update(func(s *Settings) error { s.On = true; return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestNothingIsPlayedUntilThePersonTurnsSoundsOn(t *testing.T) {
	f, _ := setup(t)
	Play(Sent, false)
	if len(f.lines) != 0 {
		t.Fatalf("sounds are off by default: %v", f.lines)
	}
	turnOn(t)
	Play(Sent, false)
	if len(f.lines) != 1 || !strings.HasPrefix(f.lines[0], "afplay ") || !strings.HasSuffix(f.lines[0], "Pop.aiff") {
		t.Fatalf("on: %v", f.lines)
	}
}

func TestTheEnvironmentOverridesTheSavedSetting(t *testing.T) {
	f, _ := setup(t)
	turnOn(t)
	t.Setenv(envName, "off")
	Play(Sent, false)
	if len(f.lines) != 0 || Enabled() {
		t.Fatalf("AIVOICEBUS_SOUND=off beats a saved on: %v", f.lines)
	}
	t.Setenv(envName, "1")
	if !Enabled() {
		t.Fatal("AIVOICEBUS_SOUND=1 turns it on for this process")
	}
	if err := Update(func(s *Settings) error { s.On = false; return nil }); err != nil {
		t.Fatal(err)
	}
	Play(Received, false)
	if len(f.lines) != 1 || !strings.HasSuffix(f.lines[0], "Glass.aiff") {
		t.Fatalf("=1 plays with the default sounds: %v", f.lines)
	}
}

func TestEachEventHasItsOwnSound(t *testing.T) {
	f, clock := setup(t)
	turnOn(t)
	for _, e := range Events {
		Play(e, false)
		*clock = clock.Add(time.Second)
	}
	var got []string
	for _, l := range f.lines {
		got = append(got, filepath.Base(l))
	}
	if want := []string{"Pop.aiff", "Glass.aiff", "Funk.aiff"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sent, received, retracted played %v, want %v", got, want)
	}
}

func TestAChosenSoundVolumeAndASilencedEvent(t *testing.T) {
	f, clock := setup(t)
	turnOn(t)
	own := filepath.Join(t.TempDir(), "mine.wav")
	if err := os.WriteFile(own, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Update(func(s *Settings) error {
		s.Sounds = map[Event]string{Sent: "tink", Received: own, Retracted: Off} // a name in any case, a path, silence
		s.Volume = 0.5
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, e := range Events {
		Play(e, false)
		*clock = clock.Add(time.Second)
	}
	if len(f.lines) != 2 {
		t.Fatalf("the silenced event plays nothing: %v", f.lines)
	}
	if !strings.HasPrefix(f.lines[0], "afplay -v 0.50 ") || !strings.HasSuffix(f.lines[0], "Tink.aiff") || !strings.HasSuffix(f.lines[1], own) {
		t.Fatalf("%v", f.lines)
	}
}

func TestQuietHoursSilenceEverythingButAnUrgentArrival(t *testing.T) {
	f, clock := setup(t)
	turnOn(t)
	if err := config.SaveQuiet(config.Quiet{Until: clock.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	Play(Sent, false)
	Play(Received, false)
	Play(Retracted, false)
	if len(f.lines) != 0 {
		t.Fatalf("quiet hours are active: %v", f.lines)
	}
	Play(Received, true)
	if len(f.lines) != 1 {
		t.Fatalf("an urgent message still sounds: %v", f.lines)
	}
	*clock = clock.Add(2 * time.Hour)
	Play(Sent, false)
	if len(f.lines) != 2 {
		t.Fatalf("after quiet hours: %v", f.lines)
	}
}

func TestSeveralOfOneEventWithinAFractionOfASecondAreOneSound(t *testing.T) {
	f, clock := setup(t)
	turnOn(t)
	Play(Retracted, false)
	*clock = clock.Add(minGap / 2)
	Play(Retracted, false) // an incident retracting a dozen messages
	Play(Sent, false)      // another event has its own gap
	if len(f.lines) != 2 {
		t.Fatalf("%v", f.lines)
	}
	*clock = clock.Add(minGap)
	Play(Retracted, false)
	if len(f.lines) != 3 {
		t.Fatalf("a later one sounds again: %v", f.lines)
	}
}

func TestNoPlayerNoSound(t *testing.T) {
	f, _ := setup(t)
	turnOn(t)
	goos = "linux"
	Play(Sent, false)
	if len(f.lines) != 0 {
		t.Fatalf("only macOS plays so far: %v", f.lines)
	}
	if err := Test(Sent); err == nil || !strings.Contains(err.Error(), "macOS only") {
		t.Fatalf("a person trying it is told why: %v", err)
	}
}

func TestResolveRefusesWhatIsNotASoundOrNotThere(t *testing.T) {
	setup(t)
	for _, c := range []struct{ name, want string }{
		{"../../etc/passwd", "give the whole path"},   // a path that is not whole
		{"x; rm -rf ~", "not a sound's name"},         // never reaches a command line as anything but one argument
		{"Nosuchsound", "no sound named"},             // not in any folder
		{"/etc/hosts", "not a sound file"},            // a path to something that is no sound
		{"/no/such/file.aiff", "not a file"},          // a sound file that is not there
		{"relative/file.aiff", "give the whole path"}, // not whole
	} {
		if _, err := Resolve(c.name, Sent); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want it to say %q", c.name, err, c.want)
		}
	}
	if p, err := Resolve("GLASS", Sent); err != nil || filepath.Base(p) != "Glass.aiff" {
		t.Errorf("case does not matter: %q %v", p, err)
	}
}

func TestTestPlaysEvenWhenOffAndWaits(t *testing.T) {
	f, clock := setup(t)
	if err := config.SaveQuiet(config.Quiet{Until: clock.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := Test(Received); err != nil || len(f.lines) != 1 || !strings.HasSuffix(f.lines[0], "Glass.aiff") {
		t.Fatalf("a person trying a sound hears it whatever the setting or the hour: %v %v", err, f.lines)
	}
	if err := Update(func(s *Settings) error { s.Sounds = map[Event]string{Received: "Gone"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := Test(Received); err == nil || !strings.Contains(err.Error(), "no sound named") {
		t.Fatalf("a sound that is gone is said: %v", err)
	}
}

func TestSettingsRoundTripAndAMalformedFileIsOff(t *testing.T) {
	setup(t)
	if err := Update(func(s *Settings) error {
		s.On, s.Volume, s.Sounds = true, 0.25, map[Event]string{Sent: "Tink"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if s := Load(); !s.On || s.Volume != 0.25 || s.Name(Sent) != "Tink" || s.Name(Received) != Default {
		t.Fatalf("%+v", s)
	}
	if err := os.WriteFile(filepath.Join(config.Dir(), settingsFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Load().On || Enabled() {
		t.Fatal("a file nobody can read is off, never a crash")
	}
	if err := Update(func(s *Settings) error { s.On = true; return nil }); err != nil || !Load().On {
		t.Fatalf("a command replaces a malformed file: %v", err)
	}
}

func TestDescribeSaysWhatEachEventPlays(t *testing.T) {
	setup(t)
	s := Settings{Sounds: map[Event]string{Retracted: Off, Sent: "Nope"}}
	if name, path, err := Describe(s, Received); err != nil || name != Default || filepath.Base(path) != "Glass.aiff" {
		t.Errorf("default: %q %q %v", name, path, err)
	}
	if name, path, err := Describe(s, Retracted); err != nil || name != Off || path != "" {
		t.Errorf("off is not a problem: %q %q %v", name, path, err)
	}
	if _, _, err := Describe(s, Sent); err == nil {
		t.Error("a name that is not there is a problem to show")
	}
}

func TestListIsSortedAndHasNoRepeats(t *testing.T) {
	setup(t)
	got := List()
	if !sort.StringsAreSorted(got) {
		t.Fatalf("not sorted: %v", got)
	}
	seen := map[string]bool{}
	for _, n := range got {
		if seen[strings.ToLower(n)] {
			t.Fatalf("%s twice: %v", n, got)
		}
		seen[strings.ToLower(n)] = true
	}
	for _, want := range []string{"Funk", "Glass", "Pop", "Tink"} {
		if !seen[strings.ToLower(want)] {
			t.Errorf("%s is missing: %v", want, got)
		}
	}
}
