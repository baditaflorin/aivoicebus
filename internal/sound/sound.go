// Package sound plays the short sounds the person on a machine can ask for when a message is sent,
// arrives or is taken back there (`aivoicebus sound`).
//
// It is off until that person turns it on, and it never speaks for anyone else: the sounds are
// fixed system files or a path the person chose, and nothing a sender wrote reaches a command line.
// Playing never fails the caller: a hook or a send must not stop because the speakers are missing,
// so Play says nothing about what went wrong (Test, which a person runs to try it, does).
package sound

import (
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
	Received  Event = "received"  // a message arrived for a session of this machine and was not heard before
	Retracted Event = "retracted" // a message of this machine was taken back
)

// Events lists every event, in the order a person hears them in `sound test`.
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

// Names a setting may hold besides a sound's name or a path.
const (
	Off     = "off"     // no sound for this event
	Default = "default" // the sound this release picks for it
)

const (
	settingsFile = "sound"            // in the config directory
	envName      = "AIVOICEBUS_SOUND" // 0/off or 1/on for this process, over the saved setting
	minGap       = 400 * time.Millisecond
	maxVolume    = 1.0
	minVolume    = 0.05
)

// Settings is what the person chose, saved in ~/.config/aivoicebus/sound.
type Settings struct {
	On     bool             `json:"on"`
	Sounds map[Event]string `json:"sounds,omitempty"` // per event: a sound's name, an absolute path, Off or Default
	Volume float64          `json:"volume,omitempty"` // minVolume..maxVolume of the system volume; 0 is full
}

// Which sound an event has when the person chose none: macOS system sounds, short and distinct.
var defaults = map[Event]string{Sent: "Pop", Received: "Glass", Retracted: "Funk"}

// Name is the sound chosen for e, Default when none was (the setting as typed, not a path).
func (s Settings) Name(e Event) string {
	if n := s.Sounds[e]; n != "" {
		return n
	}
	return Default
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

// Enabled reports whether sounds are on for this process: AIVOICEBUS_SOUND when it says so, else
// the saved setting. A machine nobody turned them on for (a node, a worker, CI) stays silent.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envName))) {
	case "0", "off", "false", "no":
		return false
	case "1", "on", "true", "yes":
		return true
	}
	return Load().On
}

// Seams for tests: the platform, the clock, and how a player is started.
var (
	goos  = runtime.GOOS
	now   = time.Now
	start = func(cmd *exec.Cmd) error {
		detach(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait() // the sound ends by itself; waiting only keeps a long-lived caller (the worker, the MCP server) free of zombies
		return nil
	}
	run = func(cmd *exec.Cmd) error { return cmd.Run() }
	// home is where a person's own sounds live.
	home = func() string { h, _ := os.UserHomeDir(); return h }
)

var (
	gapMu  sync.Mutex
	played = map[Event]time.Time{}
)

// Play makes the sound for e, in the background, when sounds are on. Quiet hours (`aivoicebus quiet`)
// silence it, except for urgent, which is what they let through for messages too. Several of one
// event within a fraction of a second are one sound. It never reports a problem: see Test.
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
	if cmd, err := command(Load(), e); err == nil {
		_ = start(cmd)
	}
}

// Test plays the sound for e now, whether or not sounds are on and whatever the hour, and waits for
// it to end. The error says what is missing: no player on this system, a sound that is not there.
func Test(e Event) error {
	cmd, err := command(Load(), e)
	if err != nil {
		return err
	}
	if err := run(cmd); err != nil {
		return fmt.Errorf("the player failed: %v", err)
	}
	return nil
}

var errSilent = errors.New("no sound is set for this event")

// Supported says whether sounds can be played on this system.
func Supported() error {
	if goos != "darwin" {
		return fmt.Errorf("sounds are played on macOS only so far (this is %s)", goos)
	}
	return nil
}

// Describe is the setting for e as the person gave it and the file it plays: path is "" for Off, and
// err says why a setting plays nothing (a name or file that is not there, or no player on this system).
func Describe(s Settings, e Event) (name, path string, err error) {
	name = s.Name(e)
	if err = Supported(); err != nil {
		return name, "", err
	}
	if path, err = Resolve(name, e); errors.Is(err, errSilent) {
		return name, "", nil
	}
	return name, path, err
}

// command is the player's command line for e under s, or the reason there is none.
func command(s Settings, e Event) (*exec.Cmd, error) {
	if err := Supported(); err != nil {
		return nil, err
	}
	path, err := Resolve(s.Name(e), e)
	if err != nil {
		return nil, err
	}
	args := []string{path}
	if v := s.Volume; v > 0 && v < maxVolume {
		args = append([]string{"-v", strconv.FormatFloat(v, 'f', 2, 64)}, args...)
	}
	return exec.Command("afplay", args...), nil
}

// soundExts are the files the player takes.
var soundExts = map[string]bool{".aiff": true, ".aif": true, ".wav": true, ".caf": true, ".mp3": true, ".m4a": true}

func soundDirs() []string {
	d := []string{"/System/Library/Sounds", "/Library/Sounds"}
	if h := home(); h != "" {
		d = append([]string{filepath.Join(h, "Library", "Sounds")}, d...) // the person's own come first
	}
	return d
}

// Resolve turns a setting into the file to play: Default is e's own, a name is looked up (case does
// not matter) among the person's sounds and the system's, and a path must be absolute and a sound
// file that is there. Off is errSilent.
func Resolve(name string, e Event) (string, error) {
	switch name {
	case Off:
		return "", errSilent
	case "", Default:
		name = defaults[e]
	}
	if strings.ContainsRune(name, '/') {
		return checkFile(name)
	}
	if !validName(name) {
		return "", fmt.Errorf("%q is not a sound's name (letters, digits, space, - and _), nor an absolute path", name)
	}
	for _, d := range soundDirs() {
		ents, _ := os.ReadDir(d)
		for _, ent := range ents {
			ext := filepath.Ext(ent.Name())
			if soundExts[strings.ToLower(ext)] && strings.EqualFold(strings.TrimSuffix(ent.Name(), ext), name) {
				return filepath.Join(d, ent.Name()), nil
			}
		}
	}
	return "", fmt.Errorf("no sound named %q (`aivoicebus sound list` shows them)", name)
}

func checkFile(p string) (string, error) {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return "", fmt.Errorf("%q: give the whole path, without ./ or ..", p)
	}
	if !soundExts[strings.ToLower(filepath.Ext(p))] {
		return "", fmt.Errorf("%q is not a sound file (.aiff, .wav, .caf, .mp3 or .m4a)", p)
	}
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a file here", p)
	}
	return p, nil
}

func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == ' ' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// List is the names of the sounds a setting can use, sorted, without repeats.
func List() []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range soundDirs() {
		ents, _ := os.ReadDir(d)
		for _, ent := range ents {
			ext := filepath.Ext(ent.Name())
			if n := strings.TrimSuffix(ent.Name(), ext); soundExts[strings.ToLower(ext)] && validName(n) && !seen[strings.ToLower(n)] {
				seen[strings.ToLower(n)] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ValidVolume reports whether v is a setting `sound volume` takes.
func ValidVolume(v float64) bool { return v >= minVolume && v <= maxVolume }
