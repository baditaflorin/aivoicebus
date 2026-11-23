package sound

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

type fake struct {
	started []string // command lines handed to the background player
	ran     []string // command lines run and waited for
	runOut  string   // what a waited-for run prints
	runErr  error
}

// setup puts the package on a pretend Mac with its own HOME, a recording player, a clock the test moves, and
// a helper that is already built (a file where the real one goes). It leaves AIVOICEBUS_SOUND unset.
func setup(t *testing.T) (*fake, *time.Time) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv(envName, "")
	f := &fake{}
	clock := time.Now().Truncate(time.Second) // the real time: SaveQuiet drops a time that is past by the real clock
	oldGoos, oldNow, oldStart, oldRun, oldTools, oldCompile := goos, now, start, run, haveTools, compile
	goos, now = "darwin", func() time.Time { return clock }
	start = func(c *exec.Cmd) error { f.started = append(f.started, strings.Join(c.Args, " ")); return nil }
	run = func(c *exec.Cmd) ([]byte, error) {
		f.ran = append(f.ran, strings.Join(c.Args, " "))
		return []byte(f.runOut), f.runErr
	}
	haveTools = func() error { return nil }
	gapMu.Lock()
	played = map[Event]time.Time{}
	gapMu.Unlock()
	t.Cleanup(func() {
		goos, now, start, run, haveTools, compile = oldGoos, oldNow, oldStart, oldRun, oldTools, oldCompile
	})
	putHelper(t, 0o700)
	return f, &clock
}

func putHelper(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := helperPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func turnOn(t *testing.T) {
	t.Helper()
	if err := Update(func(s *Settings) error { s.On = true; return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestNothingTapsUntilThePersonTurnsItOn(t *testing.T) {
	f, _ := setup(t)
	Play(Sent, false)
	if len(f.started) != 0 {
		t.Fatalf("off by default: %v", f.started)
	}
	turnOn(t)
	Play(Sent, false)
	want := helperPath() + " --level 0.80 0.3,6:2200,0.5,12"
	if len(f.started) != 1 || !strings.HasSuffix(f.started[0], want) {
		t.Fatalf("on: %v, want ...%s", f.started, want)
	}
}

func TestTheEnvironmentOverridesTheSavedSetting(t *testing.T) {
	f, _ := setup(t)
	turnOn(t)
	t.Setenv(envName, "off")
	Play(Sent, false)
	if len(f.started) != 0 || Enabled() {
		t.Fatalf("AIVOICEBUS_SOUND=off beats a saved on: %v", f.started)
	}
	t.Setenv(envName, "1")
	if !Enabled() {
		t.Fatal("AIVOICEBUS_SOUND=1 turns it on for this process")
	}
	if err := Update(func(s *Settings) error { s.On = false; return nil }); err != nil {
		t.Fatal(err)
	}
	Play(Received, false)
	if len(f.started) != 1 || !strings.Contains(f.started[0], "wait:90") {
		t.Fatalf("=1 taps with the default patterns: %v", f.started)
	}
}

func TestEachEventHasItsOwnPatternSoTheyAreToldApartByFeel(t *testing.T) {
	f, clock := setup(t)
	turnOn(t)
	for _, e := range Events {
		Play(e, false)
		*clock = clock.Add(time.Second)
	}
	if len(f.started) != 3 {
		t.Fatalf("%v", f.started)
	}
	seen := map[string]bool{}
	for _, l := range f.started {
		_, args, _ := strings.Cut(l, "--level 0.80 ")
		if seen[args] {
			t.Fatalf("two events tap the same: %v", f.started)
		}
		seen[args] = true
	}
	if !strings.Contains(f.started[0], "2200") || !strings.Contains(f.started[1], "wait:90") || !strings.Contains(f.started[2], "180,0.6,70") {
		t.Fatalf("sent is a tick, received a double, retracted a fall: %v", f.started)
	}
}

func TestAChosenPatternLevelAndASilencedEvent(t *testing.T) {
	f, clock := setup(t)
	turnOn(t)
	if err := Update(func(s *Settings) error {
		s.Patterns = map[Event]string{Sent: "THUD", Received: "rise", Retracted: Off} // a name in any case, one more, silence
		s.Level = 0.5
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, e := range Events {
		Play(e, false)
		*clock = clock.Add(time.Second)
	}
	if len(f.started) != 2 {
		t.Fatalf("the silenced event taps nothing: %v", f.started)
	}
	if !strings.Contains(f.started[0], "--level 0.50 0.3,14:130,0.7,60") || !strings.Contains(f.started[1], "900,0.5,14") {
		t.Fatalf("%v", f.started)
	}
}

func TestQuietHoursStillTheTrackpadExceptForAnUrgentArrival(t *testing.T) {
	f, clock := setup(t)
	turnOn(t)
	if err := config.SaveQuiet(config.Quiet{Until: clock.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	Play(Sent, false)
	Play(Received, false)
	Play(Retracted, false)
	if len(f.started) != 0 {
		t.Fatalf("quiet hours are active: %v", f.started)
	}
	Play(Received, true)
	if len(f.started) != 1 {
		t.Fatalf("an urgent message still taps: %v", f.started)
	}
	*clock = clock.Add(2 * time.Hour)
	Play(Sent, false)
	if len(f.started) != 2 {
		t.Fatalf("after quiet hours: %v", f.started)
	}
}

func TestSeveralOfOneEventWithinAFractionOfASecondAreOneTap(t *testing.T) {
	f, clock := setup(t)
	turnOn(t)
	Play(Retracted, false)
	*clock = clock.Add(minGap / 2)
	Play(Retracted, false) // an incident retracting a dozen messages
	Play(Sent, false)      // another event has its own gap
	if len(f.started) != 2 {
		t.Fatalf("%v", f.started)
	}
	*clock = clock.Add(minGap)
	Play(Retracted, false)
	if len(f.started) != 3 {
		t.Fatalf("a later one taps again: %v", f.started)
	}
}

func TestNotOnAMacOrWithoutTheHelperNothingHappensAndTheCallerIsNotTold(t *testing.T) {
	f, _ := setup(t)
	turnOn(t)
	goos = "linux"
	Play(Sent, false)
	if len(f.started) != 0 {
		t.Fatalf("only macOS has the motor: %v", f.started)
	}
	if err := Test(Sent); err == nil || !strings.Contains(err.Error(), "macOS only") {
		t.Fatalf("a person trying it is told why: %v", err)
	}
	goos = "darwin"
	if err := os.Remove(helperPath()); err != nil {
		t.Fatal(err)
	}
	Play(Sent, false)
	if len(f.started) != 0 {
		t.Fatalf("no helper built: %v", f.started)
	}
	if err := Test(Sent); err == nil || !strings.Contains(err.Error(), "sound setup") {
		t.Fatalf("and a person is told what to run: %v", err)
	}
}

func TestAHelperOthersCanWriteIsNotRun(t *testing.T) {
	f, _ := setup(t)
	turnOn(t)
	putHelper(t, 0o777)
	Play(Sent, false)
	if len(f.started) != 0 {
		t.Fatalf("a world-writable helper is never run: %v", f.started)
	}
	if _, err := helperReady(); err == nil || !strings.Contains(err.Error(), "private executable") {
		t.Fatalf("%v", err)
	}
}

func TestResolveFindsAPatternByNameAndSaysWhatIsAvailable(t *testing.T) {
	if p, err := Resolve("Double", Sent); err != nil || p.Name != "double" {
		t.Errorf("case does not matter: %v %v", p, err)
	}
	if p, err := Resolve("", Retracted); err != nil || p.Name != "fall" {
		t.Errorf("empty is the event's own: %v %v", p, err)
	}
	if _, err := Resolve(Off, Sent); !errors.Is(err, errSilent) {
		t.Errorf("off is silence: %v", err)
	}
	if _, err := Resolve("../../x", Sent); err == nil || !strings.Contains(err.Error(), "buzz, double, fall, rise, thud, tick, tock") {
		t.Errorf("an unknown name lists the real ones: %v", err)
	}
}

func TestTestTapsWhateverTheSettingOrTheHourAndReportsWhatTheHelperSaid(t *testing.T) {
	f, clock := setup(t)
	if err := config.SaveQuiet(config.Quiet{Until: clock.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := Test(Received); err != nil || len(f.ran) != 1 || !strings.Contains(f.ran[0], "wait:90") {
		t.Fatalf("a person trying a pattern feels it whatever the setting or the hour: %v %v", err, f.ran)
	}
	f.runOut, f.runErr = "aivoicebus-haptic: no trackpad found\n", errors.New("exit status 4")
	if err := Test(Sent); err == nil || err.Error() != "no trackpad found" {
		t.Fatalf("the helper's own words: %v", err)
	}
	f.runOut = ""
	if err := Test(Sent); err == nil || !strings.Contains(err.Error(), "exit status 4") {
		t.Fatalf("%v", err)
	}
	if err := Update(func(s *Settings) error { s.Patterns = map[Event]string{Sent: "nope"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := Test(Sent); err == nil || !strings.Contains(err.Error(), "no pattern named") {
		t.Fatalf("a pattern that is gone is said: %v", err)
	}
}

func TestSetupBuildsTheHelperOnceFromItsOwnSourceAndSweepsOldOnes(t *testing.T) {
	setup(t)
	if err := os.Remove(helperPath()); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(filepath.Dir(helperPath()), helperName+"-00000000")
	if err := os.WriteFile(old, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	var compiled []string
	compile = func(src, out string) ([]byte, error) {
		b, err := os.ReadFile(src)
		if err != nil || string(b) != string(helperSource) {
			t.Errorf("the compiler is given the embedded source: %v", err)
		}
		compiled = append(compiled, out)
		return nil, os.WriteFile(out, []byte("binary"), 0o755)
	}
	p, built, err := Setup()
	if err != nil || !built || p != helperPath() || len(compiled) != 1 {
		t.Fatalf("%q %v %v %v", p, built, err, compiled)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("private to the person: %v %v", fi, err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("a helper of an earlier release is removed")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".haptic-*")); len(left) != 0 {
		t.Errorf("the source copy is removed: %v", left)
	}
	if _, built, err := Setup(); err != nil || built || len(compiled) != 1 {
		t.Fatalf("a second setup builds nothing: %v %v %v", built, err, compiled)
	}
}

func TestSetupSaysWhatIsMissing(t *testing.T) {
	setup(t)
	if err := os.Remove(helperPath()); err != nil {
		t.Fatal(err)
	}
	haveTools = func() error { return errors.New("no developer dir") }
	compile = func(string, string) ([]byte, error) {
		t.Fatal("the compiler must not be asked without tools: it opens an installer")
		return nil, nil
	}
	if _, _, err := Setup(); err == nil || !strings.Contains(err.Error(), "xcode-select --install") {
		t.Fatalf("%v", err)
	}
	haveTools = func() error { return nil }
	compile = func(string, string) ([]byte, error) {
		return []byte("haptic.c:1: error: boom\n"), errors.New("exit status 1")
	}
	if _, _, err := Setup(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("the compiler's words reach the person: %v", err)
	}
	if _, err := os.Stat(helperPath()); err == nil {
		t.Fatal("a failed build leaves no helper")
	}
	goos = "linux"
	if _, _, err := Setup(); err == nil || !strings.Contains(err.Error(), "macOS only") {
		t.Fatalf("%v", err)
	}
}

func TestProbeSaysWhichTrackpadWithoutTapping(t *testing.T) {
	f, _ := setup(t)
	f.runOut = "trackpad id=1 builtin=1\n"
	got, err := Probe()
	if err != nil || got != "trackpad id=1 builtin=1" || len(f.ran) != 1 || !strings.HasSuffix(f.ran[0], "--probe") {
		t.Fatalf("%q %v %v", got, err, f.ran)
	}
	f.runOut, f.runErr = "aivoicebus-haptic: no trackpad found\n", errors.New("exit status 4")
	if _, err := Probe(); err == nil || err.Error() != "no trackpad found" {
		t.Fatalf("%v", err)
	}
}

func TestSettingsRoundTripAndAMalformedFileIsOff(t *testing.T) {
	setup(t)
	if err := Update(func(s *Settings) error {
		s.On, s.Level, s.Patterns = true, 0.25, map[Event]string{Sent: "tock"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if s := Load(); !s.On || s.Level != 0.25 || s.Name(Sent) != "tock" || s.Name(Received) != Default || s.level() != 0.25 {
		t.Fatalf("%+v", s)
	}
	if (Settings{}).level() != DefaultLevel || (Settings{Level: 7}).level() != DefaultLevel {
		t.Fatal("no level, or a level out of range, is the default")
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

func TestDescribeSaysWhatEachEventDoes(t *testing.T) {
	setup(t)
	s := Settings{Patterns: map[Event]string{Retracted: Off, Sent: "Nope"}}
	if name, desc, err := Describe(s, Received); err != nil || name != "double" || !strings.Contains(desc, "two quick taps") {
		t.Errorf("default: %q %q %v", name, desc, err)
	}
	if name, desc, err := Describe(s, Retracted); err != nil || name != Off || desc != "" {
		t.Errorf("off is not a problem: %q %q %v", name, desc, err)
	}
	if _, _, err := Describe(s, Sent); err == nil {
		t.Error("a name that is not there is a problem to show")
	}
}

// Every pattern must stay inside what haptic.c accepts and what the motor is good for: the helper
// refuses what is outside, and a pattern longer than a third of a second is a buzz, not a tap.
func TestEveryPatternIsWithinTheHelpersLimits(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range presets {
		if seen[p.Name] || p.Desc == "" {
			t.Errorf("%s: repeated or undescribed", p.Name)
		}
		seen[p.Name] = true
		if len(p.Args) == 0 || len(p.Args) > 12 {
			t.Errorf("%s: %d items", p.Name, len(p.Args))
		}
		total := 0.0
		for _, a := range p.Args {
			if w, ok := strings.CutPrefix(a, "wait:"); ok {
				ms, err := strconv.ParseFloat(w, 64)
				if err != nil || ms < 0 || ms > 500 {
					t.Errorf("%s: %q", p.Name, a)
				}
				total += ms
				continue
			}
			parts := strings.Split(a, ":")
			var base, baseMS float64
			if n, _ := sscan(parts[0], &base, &baseMS); n != 2 || base < 0 || base > 0.6 || baseMS < 1.5 || baseMS > 60 || len(parts) > 5 {
				t.Errorf("%s: burst %q", p.Name, a)
				continue
			}
			longest, sum := baseMS, base
			for _, tone := range parts[1:] {
				var hz, amp, ms float64
				if n, _ := sscan(tone, &hz, &amp, &ms); n != 3 || hz < 95 || hz > 4000 || amp < 0 || amp > 1 || ms < 1.5 || ms > 120 {
					t.Errorf("%s: tone %q", p.Name, tone)
				}
				longest, sum = max(longest, ms), sum+amp
			}
			if sum > 1.0 { // the helper scales the tones, but a table that needs scaling is a table to fix
				t.Errorf("%s: strengths add up to %.2f in %q", p.Name, sum, a)
			}
			total += longest + 12
		}
		if total > 333 {
			t.Errorf("%s lasts %.0f ms", p.Name, total)
		}
	}
	for e, name := range defaults {
		if _, err := Resolve(Default, e); err != nil || !seen[name] {
			t.Errorf("default of %s: %v", e, err)
		}
	}
}

// sscan reads comma-separated numbers into the given pointers; it returns how many it read.
func sscan(s string, out ...*float64) (int, error) {
	fields := strings.Split(s, ",")
	for i, f := range fields {
		if i >= len(out) {
			return i, errors.New("too many")
		}
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return i, err
		}
		*out[i] = v
	}
	return len(fields), nil
}

// On a Mac with a compiler the embedded source must build, and the helper must refuse bad arguments
// before it touches the framework (exit 2), so this needs no trackpad.
func TestTheEmbeddedHelperBuildsAndRefusesBadArguments(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the helper is built on a Mac")
	}
	if _, err := exec.LookPath("cc"); err != nil || exec.Command("xcode-select", "-p").Run() != nil {
		t.Skip("no developer tools here")
	}
	dir := t.TempDir()
	src, out := filepath.Join(dir, "haptic.c"), filepath.Join(dir, "helper")
	if err := os.WriteFile(src, helperSource, 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, err := exec.Command("cc", "-O2", "-Wall", "-Werror", "-o", out, src, "-framework", "CoreFoundation").CombinedOutput(); err != nil {
		t.Fatalf("haptic.c does not build cleanly: %v\n%s", err, msg)
	}
	for _, args := range [][]string{{}, {"--level", "5", "0.3,6:2200,0.5,12"}, {"wait:9999"}, {"nonsense"}, {"0.3,6:1,2"}} {
		err := exec.Command(out, args...).Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Errorf("%v: %v, want exit 2", args, err)
		}
	}
}
