// Package sound makes the trackpad of the Mac the person is working on tap, in a pattern of its own,
// when a message is sent, arrives or is taken back there (`aivoicebus sound`).
//
// The sound comes from the Force Touch trackpad's haptic motor, not from the speakers: the glass moves
// fast enough to be heard and felt, so it works with the Mac muted. The motor is reached through a
// private macOS framework that a static Go binary cannot call, so a tiny helper (helper/haptic.c, embedded
// here) is built once on the Mac with its own compiler and run for each tap.
//
// It is off until the person on the machine turns it on, and it never speaks for anyone else: the
// patterns are fixed tables of numbers, and nothing a sender wrote reaches the helper's command line.
// Playing never fails the caller: a hook or a send must not stop because there is no trackpad, so Play
// says nothing about what went wrong (Test, which a person runs to try it, does).
package sound

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

// Event is something that happened to a message on this machine.
type Event string

const (
	Sent      Event = "sent"      // a message of this machine went to the nodes
	Received  Event = "received"  // a message arrived for a session of this machine and was not felt before
	Retracted Event = "retracted" // a message of this machine was taken back
)

// Events lists every event, in the order a person feels them in `sound test`.
var Events = []Event{Sent, Received, Retracted}

// ParseEvent reads an event name.
func ParseEvent(s string) (Event, bool) {
	for _, e := range Events {
		if string(e) == strings.ToLower(strings.TrimSpace(s)) {
			return e, true
		}
	}
	return "", false
}

// Names a setting may hold besides a pattern's name.
const (
	Off     = "off"     // nothing for this event
	Default = "default" // the pattern this release picks for it
)

const (
	settingsFile = "sound"            // in the config directory
	envName      = "AIVOICEBUS_SOUND" // 0/off or 1/on for this process, over the saved setting
	minGap       = 400 * time.Millisecond

	// MinLevel and MaxLevel bound how hard the motor is driven, as a share of its normal maximum.
	MinLevel     = 0.1
	MaxLevel     = 1.0
	DefaultLevel = 0.8
)

// Settings is what the person chose, saved in ~/.config/aivoicebus/sound.
type Settings struct {
	On       bool             `json:"on"`
	Patterns map[Event]string `json:"patterns,omitempty"` // per event: a pattern's name, Off or Default
	Level    float64          `json:"level,omitempty"`    // MinLevel..MaxLevel; 0 is DefaultLevel
}

// Pattern is a short run of taps. Args are the helper's arguments (see haptic.c): bursts of a soft thump
// plus sine tones (hz, strength, ms), and waits between them. The motor is audible from about 100 Hz to
// 4 kHz and is built for taps, not for sound, so a pattern stays under a third of a second.
type Pattern struct {
	Name string
	Desc string
	Args []string
}

var presets = []Pattern{
	{"tick", "one short, light tap", []string{"0.3,6:2200,0.5,12"}},
	{"tock", "one lower, rounder tap", []string{"0.4,8:700,0.6,18"}},
	{"double", "two quick taps, the second higher", []string{"0.3,6:1400,0.5,14", "wait:90", "0.3,6:1900,0.5,14"}},
	{"rise", "three taps stepping up", []string{"0.3,6:900,0.5,14", "wait:70", "0.3,6:1500,0.5,14", "wait:70", "0.3,6:2200,0.5,16"}},
	{"fall", "a tap that drops, then a low thump", []string{"0.3,10:520,0.4,40:260,0.3,40", "wait:60", "0.3,10:180,0.6,70"}},
	{"thud", "one heavy, low thump", []string{"0.3,14:130,0.7,60"}},
	{"buzz", "two short low buzzes", []string{"0.2,10:150,0.7,100", "wait:50", "0.2,10:150,0.7,100"}},
}

// Which pattern an event has when the person chose none: a light tap out, two rising taps in, and a
// drop for taking it back, so each is told apart by feel.
var defaults = map[Event]string{Sent: "tick", Received: "double", Retracted: "fall"}

// Presets lists the patterns a setting can name.
func Presets() []Pattern { return append([]Pattern(nil), presets...) }

// Name is the pattern chosen for e, Default when none was (the setting as typed).
func (s Settings) Name(e Event) string {
	if n := s.Patterns[e]; n != "" {
		return n
	}
	return Default
}

// level is the strength to play at.
func (s Settings) level() float64 {
	if s.Level >= MinLevel && s.Level <= MaxLevel {
		return s.Level
	}
	return DefaultLevel
}

// ValidLevel reports whether v is a setting `sound level` takes.
func ValidLevel(v float64) bool { return v >= MinLevel && v <= MaxLevel }

var errSilent = errors.New("no pattern is set for this event")

// Resolve turns a setting into the pattern to play: Default is e's own, a name is looked up (case does
// not matter). Off is errSilent.
func Resolve(name string, e Event) (Pattern, error) {
	switch name = strings.ToLower(strings.TrimSpace(name)); name {
	case Off:
		return Pattern{}, errSilent
	case "", Default:
		name = defaults[e]
	}
	for _, p := range presets {
		if p.Name == name {
			return p, nil
		}
	}
	names := make([]string, len(presets))
	for i, p := range presets {
		names[i] = p.Name
	}
	sort.Strings(names)
	return Pattern{}, fmt.Errorf("no pattern named %q (%s, or off)", name, strings.Join(names, ", "))
}

// Load reads the saved settings; anything missing, unreadable or malformed means off.
func Load() Settings {
	var s Settings
	if d := config.Dir(); d != "" {
		if text, err := config.ReadSmall(filepath.Join(d, settingsFile)); err == nil && json.Unmarshal([]byte(strings.TrimSpace(text)), &s) != nil {
			s = Settings{}
		}
	}
	return s
}

// Update changes the saved settings under the config directory's lock. edit may refuse the change.
func Update(edit func(*Settings) error) error {
	return config.UpdateSmall(settingsFile, func(old string) (string, error) {
		var s Settings
		if strings.TrimSpace(old) != "" && json.Unmarshal([]byte(strings.TrimSpace(old)), &s) != nil {
			s = Settings{} // a malformed file is replaced, as Load reads it as off
		}
		if err := edit(&s); err != nil {
			return "", err
		}
		b, err := json.Marshal(s)
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	})
}

// Enabled reports whether taps are on for this process: AIVOICEBUS_SOUND when it says so, else the saved
// setting. A machine nobody turned them on for (a node, a worker, CI) stays still.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envName))) {
	case "0", "off", "false", "no":
		return false
	case "1", "on", "true", "yes":
		return true
	}
	return Load().On
}

// Supported says whether taps can be made on this system.
func Supported() error {
	if goos != "darwin" {
		return fmt.Errorf("the trackpad can be tapped on macOS only (this is %s)", goos)
	}
	return nil
}

// Seams for tests: the platform, the clock, how the helper is started, run and built.
var (
	goos  = runtime.GOOS
	now   = time.Now
	start = func(cmd *exec.Cmd) error {
		detach(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait() // the tap ends by itself; waiting only keeps a long-lived caller (the worker, the MCP server) free of zombies
		return nil
	}
	run = func(cmd *exec.Cmd) ([]byte, error) { return cmd.CombinedOutput() }
	// haveTools says whether the Mac has a developer toolchain: asking `cc` without one makes macOS open
	// an installer dialog, so this is asked first.
	haveTools = func() error { return exec.Command("xcode-select", "-p").Run() }
	compile   = func(src, out string) ([]byte, error) {
		return exec.Command("cc", "-O2", "-o", out, src, "-framework", "CoreFoundation").CombinedOutput()
	}
)

var (
	gapMu  sync.Mutex
	played = map[Event]time.Time{}
)

// Play taps the trackpad for e, in the background, when taps are on. Quiet hours (`aivoicebus quiet`)
// silence it, except for urgent, which is what they let through for messages too. Several of one event
// within a fraction of a second are one tap. It never reports a problem: see Test.
func Play(e Event, urgent bool) {
	if !Enabled() || (!urgent && config.LoadQuiet().Active(now())) {
		return
	}
	gapMu.Lock()
	t := now()
	if last, ok := played[e]; ok && t.Sub(last) < minGap {
		gapMu.Unlock()
		return
	}
	played[e] = t
	gapMu.Unlock()
	s := Load()
	if p, err := Resolve(s.Name(e), e); err == nil {
		if cmd, err := command(s, p); err == nil {
			_ = start(cmd)
		}
	}
}

// Test taps the pattern the person chose for e now, whether or not taps are on and whatever the hour, and
// waits for it to end. The error says what is missing: no helper built, no trackpad, a pattern that is gone.
func Test(e Event) error {
	s := Load()
	p, err := Resolve(s.Name(e), e)
	if err != nil {
		return err
	}
	return TestPattern(p)
}

// TestPattern taps p now, at the saved level.
func TestPattern(p Pattern) error {
	cmd, err := command(Load(), p)
	if err != nil {
		return err
	}
	if out, err := run(cmd); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return errors.New(strings.TrimPrefix(msg, helperName+": "))
		}
		return fmt.Errorf("the trackpad helper failed: %v", err)
	}
	return nil
}

// command is the helper's command line for p under s, or the reason there is none.
func command(s Settings, p Pattern) (*exec.Cmd, error) {
	if err := Supported(); err != nil {
		return nil, err
	}
	path, err := helperReady()
	if err != nil {
		return nil, err
	}
	args := append([]string{"--level", strconv.FormatFloat(s.level(), 'f', 2, 64)}, p.Args...)
	return exec.Command(path, args...), nil
}

// ---- the helper ----

//go:embed helper/haptic.c
var helperSource []byte

const helperName = "aivoicebus-haptic"

// helperPath is where the helper for this release lives: the name carries a hash of its source, so a
// release with a changed helper builds its own and never runs an old one.
func helperPath() string {
	d := config.Dir()
	if d == "" {
		return ""
	}
	sum := sha256.Sum256(helperSource)
	return filepath.Join(d, "bin", helperName+"-"+hex.EncodeToString(sum[:4]))
}

// helperReady returns the helper's path when it is built and is the person's own, not writable by others.
func helperReady() (string, error) {
	p := helperPath()
	if p == "" {
		return "", errors.New("no home directory to keep the helper in")
	}
	fi, err := os.Lstat(p)
	switch {
	case err != nil:
		return "", errors.New("the trackpad helper is not built yet: run `aivoicebus sound setup`")
	case !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 || fi.Mode().Perm()&0o100 == 0:
		return "", fmt.Errorf("%s is not a private executable file; remove it and run `aivoicebus sound setup`", p)
	}
	return p, nil
}

// Setup builds the helper from its embedded source unless this release's is there, and says which. It
// needs the Mac's developer tools (Xcode or `xcode-select --install`) once; nothing is downloaded.
func Setup() (path string, built bool, err error) {
	if err := Supported(); err != nil {
		return "", false, err
	}
	if p, err := helperReady(); err == nil {
		return p, false, nil
	}
	target := helperPath()
	if target == "" {
		return "", false, errors.New("no home directory to keep the helper in")
	}
	if haveTools() != nil {
		return "", false, errors.New("building the trackpad helper needs the Mac's developer tools (Xcode, or run `xcode-select --install`); nothing else is needed")
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, err
	}
	src, err := os.CreateTemp(dir, ".haptic-*.c")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(src.Name())
	if _, err := src.Write(helperSource); err != nil {
		src.Close()
		return "", false, err
	}
	if err := src.Close(); err != nil {
		return "", false, err
	}
	tmp := target + ".build"
	defer os.Remove(tmp)
	if out, err := compile(src.Name(), tmp); err != nil {
		return "", false, fmt.Errorf("the compiler failed: %s", strings.TrimSpace(string(out)))
	}
	if err := os.Chmod(tmp, 0o700); err != nil {
		return "", false, err
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", false, err
	}
	if old, _ := filepath.Glob(filepath.Join(dir, helperName+"-*")); len(old) > 0 { // helpers of earlier releases
		for _, o := range old {
			if o != target {
				_ = os.Remove(o)
			}
		}
	}
	return target, true, nil
}

// Probe asks the helper which trackpad it finds, without tapping it: "" and an error when there is none.
func Probe() (string, error) {
	if err := Supported(); err != nil {
		return "", err
	}
	path, err := helperReady()
	if err != nil {
		return "", err
	}
	out, err := run(exec.Command(path, "--probe"))
	msg := strings.TrimSpace(string(out))
	if err != nil {
		return "", errors.New(strings.TrimPrefix(msg, helperName+": "))
	}
	return msg, nil
}

// Describe is what e does under s: name is the pattern it taps (a default is named too, so the person sees
// which), or Off, and desc says what that feels like, "" for Off. err says why a setting plays nothing (a
// pattern that is not there); name is then as it was typed. It is a lookup in the tables, so it answers the
// same on every system: whether this one can tap at all is Supported and Probe.
func Describe(s Settings, e Event) (name, desc string, err error) {
	name = s.Name(e)
	p, err := Resolve(name, e)
	if errors.Is(err, errSilent) {
		return Off, "", nil
	}
	if err != nil {
		return name, "", err
	}
	return p.Name, p.Desc, nil
}
