package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/sound"
)

const soundUsage = "usage: aivoicebus sound [on | off | toggle | setup | test [EVENT|PATTERN] | set EVENT PATTERN|off|default | level 0.1-1 | focus [follow|ignore] | list]   (EVENT: sent, received, retracted)"

// soundCmd shows or changes the one setting for what a message does to the trackpad of this Mac: it
// taps when a message is sent, when one arrives, when one is taken back. The sound comes from the
// Force Touch trackpad's motor, so the Mac can stay muted. Off until the person turns it on (see
// internal/sound).
func soundCmd(args []string, s Streams) error {
	fs := newFlagSet("sound")
	asJSON := fs.Bool("json", false, "print the setting as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	rest := takeJSON(fs.Args(), asJSON)
	var word string
	if len(rest) > 0 {
		word, rest = strings.ToLower(rest[0]), rest[1:]
	}
	switch word {
	case "":
		return soundStatus(s, *asJSON)
	case "on", "off", "toggle":
		return soundSwitch(word, s)
	case "setup":
		return soundSetup(s)
	case "list":
		for _, p := range sound.Presets() {
			fmt.Fprintf(s.Out, "%-7s %s\n", p.Name, p.Desc)
		}
		return nil
	case "test":
		return soundTest(rest, s)
	case "set":
		return soundSet(rest, s)
	case "level":
		return soundLevel(rest, s)
	case "focus":
		return soundFocus(rest, s)
	}
	return outbox.Usagef("sound: %q is not a command here\n%s", word, soundUsage)
}

// soundReady builds the helper if need be and finds the trackpad: what has to be true before a tap can
// happen. The error is what stands in the way, in words a person can act on.
func soundReady() (trackpad string, built bool, err error) {
	if err := sound.Supported(); err != nil {
		return "", false, err
	}
	if _, built, err = sound.Setup(); err != nil {
		return "", false, err
	}
	if trackpad, err = sound.Probe(); err != nil {
		return "", built, err
	}
	return trackpad, built, nil
}

func soundSetup(s Streams) error {
	trackpad, built, err := soundReady()
	if err != nil {
		return outbox.Usagef("sound setup: %v", err)
	}
	if built {
		fmt.Fprintln(s.Out, "built the trackpad helper")
	} else {
		fmt.Fprintln(s.Out, "the trackpad helper is already built")
	}
	fmt.Fprintln(s.Out, trackpad)
	return nil
}

// soundSwitch is the master switch: on, off, or the other of the two. Each event can also be silenced
// by itself (`sound set sent off`), and AIVOICEBUS_SOUND=0 or 1 decides for one process whatever is saved.
func soundSwitch(word string, s Streams) error {
	want := word == "on"
	if word == "toggle" {
		want = !sound.Load().On
	}
	if want { // on needs the helper and a trackpad: refused now, not left silent
		if _, _, err := soundReady(); err != nil {
			return outbox.Usagef("sound: %v", err)
		}
	}
	if err := sound.Update(func(st *sound.Settings) error { st.On = want; return nil }); err != nil {
		return err
	}
	if !want {
		fmt.Fprintln(s.Out, "sounds: off")
	} else {
		fmt.Fprintln(s.Out, "sounds: on. The trackpad taps, so the Mac can stay muted. Feel them with `aivoicebus sound test`; quiet hours silence them (`aivoicebus quiet`)")
	}
	if env := strings.TrimSpace(os.Getenv("AIVOICEBUS_SOUND")); env != "" && sound.Enabled() != want {
		fmt.Fprintf(s.Out, "note: AIVOICEBUS_SOUND=%s in this environment overrides the saved setting\n", env)
	}
	return nil
}

func soundTest(rest []string, s Streams) error {
	if len(rest) > 1 {
		return outbox.Usagef("%s", soundUsage)
	}
	if err := sound.Supported(); err != nil {
		return outbox.Usagef("sound test: %v", err)
	}
	if _, _, err := sound.Setup(); err != nil { // a test is the first thing a person tries: build what it needs
		return outbox.Usagef("sound test: %v", err)
	}
	if len(rest) == 1 {
		if e, ok := sound.ParseEvent(rest[0]); ok {
			return soundTapEvents(s, []sound.Event{e})
		}
		p, err := sound.Resolve(rest[0], sound.Sent)
		if err != nil || strings.EqualFold(rest[0], sound.Off) || strings.EqualFold(rest[0], sound.Default) {
			return outbox.Usagef("sound test: %q is not an event (sent, received, retracted) or a pattern (`aivoicebus sound list`)", rest[0])
		}
		fmt.Fprintf(s.Out, "%-9s %s\n", p.Name, p.Desc)
		if err := sound.TestPattern(p); err != nil {
			return fmt.Errorf("sound test: %v", err)
		}
		soundFocusNote(s)
		return nil
	}
	if err := soundTapEvents(s, sound.Events); err != nil {
		return err
	}
	soundFocusNote(s)
	return nil
}

// soundFocusNote says, after a test, that the real taps are silent right now because a Focus is on: a test taps
// through it, which would otherwise make the person think the real ones will.
func soundFocusNote(s Streams) {
	if sound.Load().IgnoreFocus {
		return
	}
	if modes, _ := sound.ActiveFocus(); len(modes) > 0 {
		if len(modes) > 1 {
			fmt.Fprintf(s.Out, "note: %s are on, so the real taps are silent until they end (`aivoicebus sound focus ignore` taps through them)\n", strings.Join(modes, ", "))
		} else {
			fmt.Fprintf(s.Out, "note: %s is on, so the real taps are silent until it ends (`aivoicebus sound focus ignore` taps through it)\n", modes[0])
		}
	}
}

func soundTapEvents(s Streams, events []sound.Event) error {
	for i, e := range events {
		name, desc, err := sound.Describe(sound.Load(), e)
		if err != nil {
			return outbox.Usagef("sound test: %s: %v", e, err)
		}
		if name == sound.Off {
			fmt.Fprintf(s.Out, "%-9s off\n", e)
			continue
		}
		fmt.Fprintf(s.Out, "%-9s %s (%s)\n", e, name, desc)
		if err := sound.Test(e); err != nil {
			return fmt.Errorf("sound test: %s: %v", e, err)
		}
		if i < len(events)-1 {
			time.Sleep(700 * time.Millisecond) // long enough to tell one from the next
		}
	}
	return nil
}

// soundFocus shows which Focus modes the Mac is in, or changes whether taps wait for them to end. The state is read
// from the Mac itself (see internal/sound), so there is nothing to tell aivoicebus about Sleep or Personal.
func soundFocus(rest []string, s Streams) error {
	if len(rest) > 1 {
		return outbox.Usagef("%s", soundUsage)
	}
	if len(rest) == 1 {
		word := strings.ToLower(rest[0])
		if word != "follow" && word != "ignore" {
			return outbox.Usagef("sound focus: %q is not follow or ignore", rest[0])
		}
		if err := sound.Update(func(st *sound.Settings) error { st.IgnoreFocus = word == "ignore"; return nil }); err != nil {
			return err
		}
	}
	fmt.Fprintln(s.Out, soundFocusLine(sound.Load()))
	return nil
}

// soundFocusLine is the one line that says what the Mac's Focus does to the taps now.
func soundFocusLine(st sound.Settings) string {
	are := func(modes []string) string { // "Sleep is on", "Personal, Sleep are on"
		if len(modes) > 1 {
			return strings.Join(modes, ", ") + " are on"
		}
		return strings.Join(modes, ", ") + " is on"
	}
	modes, err := sound.ActiveFocus()
	switch {
	case st.IgnoreFocus:
		line := "focus: ignored, taps do not wait for a Focus (`aivoicebus sound focus follow` makes them)"
		if len(modes) > 0 {
			line += "; " + are(modes) + " now"
		}
		return line
	case err != nil:
		return "!! focus: " + err.Error()
	case len(modes) > 0:
		return "focus: " + are(modes) + ": taps are silent until it ends"
	}
	return "focus: none is on; taps wait for one to end (Sleep, Personal, Do Not Disturb...)"
}

func soundSet(rest []string, s Streams) error {
	if len(rest) != 2 {
		return outbox.Usagef("%s", soundUsage)
	}
	e, ok := sound.ParseEvent(rest[0])
	if !ok {
		return outbox.Usagef("sound set: %q is not an event (sent, received, retracted)", rest[0])
	}
	name := strings.ToLower(rest[1])
	if _, err := sound.Resolve(name, e); err != nil && name != sound.Off { // a name that is not there is refused now, not felt as nothing later
		return outbox.Usagef("sound set: %v", err)
	}
	err := sound.Update(func(st *sound.Settings) error {
		if name == sound.Default {
			delete(st.Patterns, e)
			return nil
		}
		if st.Patterns == nil {
			st.Patterns = map[sound.Event]string{}
		}
		st.Patterns[e] = name
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "%s: %s\n", e, name)
	return nil
}

func soundLevel(rest []string, s Streams) error {
	if len(rest) != 1 {
		return outbox.Usagef("%s", soundUsage)
	}
	v := 0.0 // the default
	if arg := strings.ToLower(rest[0]); arg != "default" {
		var err error
		if v, err = strconv.ParseFloat(strings.TrimSuffix(arg, "%"), 64); err != nil {
			return outbox.Usagef("sound level: %q is not a number from 0.1 to 1 (or `default`)", rest[0])
		}
		if strings.HasSuffix(arg, "%") {
			v /= 100
		}
		if !sound.ValidLevel(v) {
			return outbox.Usagef("sound level: %q is out of range: from 0.1 to 1 (or `default`)", rest[0])
		}
	}
	if err := sound.Update(func(st *sound.Settings) error { st.Level = v; return nil }); err != nil {
		return err
	}
	if v == 0 {
		fmt.Fprintf(s.Out, "level: default (%.2f of the motor's normal strength)\n", sound.DefaultLevel)
	} else {
		fmt.Fprintf(s.Out, "level: %.2f of the motor's normal strength\n", v)
	}
	return nil
}

func soundStatus(s Streams, asJSON bool) error {
	st := sound.Load()
	on := sound.Enabled()
	type row struct {
		Event   string `json:"event"`
		Pattern string `json:"pattern"`          // what it taps: a name, or off
		Chosen  bool   `json:"chosen,omitempty"` // the person picked it; false for the release's own
		Desc    string `json:"desc,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	var rows []row
	for _, e := range sound.Events {
		name, desc, err := sound.Describe(st, e)
		r := row{Event: string(e), Pattern: name, Chosen: st.Name(e) != sound.Default, Desc: desc}
		if err != nil {
			r.Error = err.Error()
		}
		rows = append(rows, r)
	}
	trackpad, probeErr := sound.Probe() // no tap: only which trackpad the helper finds
	focus, focusErr := sound.ActiveFocus()
	env := strings.TrimSpace(os.Getenv("AIVOICEBUS_SOUND"))
	quiet := config.LoadQuiet().Active(time.Now())
	level := st.Level
	if level == 0 {
		level = sound.DefaultLevel
	}
	if asJSON {
		v := struct {
			On       bool    `json:"on"`
			Saved    bool    `json:"saved"` // the saved setting; on may differ when AIVOICEBUS_SOUND says so
			Level    float64 `json:"level"`
			Quiet    bool    `json:"quiet_now"`
			Trackpad string  `json:"trackpad,omitempty"`
			Problem  string  `json:"problem,omitempty"`
			// FollowFocus: taps wait for a macOS Focus to end. Focus names the ones on now, FocusProblem
			// says why that could not be read.
			FollowFocus  bool     `json:"follow_focus"`
			Focus        []string `json:"focus"`
			FocusProblem string   `json:"focus_problem,omitempty"`
			Events       []row    `json:"events"`
		}{On: on, Saved: st.On, Level: level, Quiet: quiet, Trackpad: trackpad, FollowFocus: !st.IgnoreFocus, Focus: focus, Events: rows}
		if probeErr != nil {
			v.Problem = probeErr.Error()
		}
		if focusErr != nil {
			v.FocusProblem = focusErr.Error()
		}
		if v.Focus == nil {
			v.Focus = []string{}
		}
		return writeJSON(s.Out, v)
	}
	state := "off"
	if on {
		state = "on"
	}
	fmt.Fprintf(s.Out, "sounds: %s", state)
	if env != "" && on != st.On {
		fmt.Fprintf(s.Out, " (AIVOICEBUS_SOUND=%s overrides the saved setting, which is %t)", env, st.On)
	}
	fmt.Fprintln(s.Out)
	width := 0
	for _, r := range rows {
		width = max(width, len(r.Pattern))
	}
	for _, r := range rows {
		pattern := r.Pattern
		if !r.Chosen {
			pattern += " (default)"
		}
		line := fmt.Sprintf("  %-9s %-*s", r.Event, width+10, pattern)
		switch {
		case r.Error != "":
			line += "   !! " + r.Error
		case r.Desc != "":
			line += "   " + r.Desc
		}
		fmt.Fprintln(s.Out, line)
	}
	fmt.Fprintf(s.Out, "level: %.2f of the motor's normal strength\n", level)
	if probeErr != nil {
		fmt.Fprintf(s.Out, "!! trackpad: %v\n", probeErr)
	} else {
		fmt.Fprintf(s.Out, "trackpad: %s\n", trackpad)
	}
	if quiet {
		fmt.Fprintln(s.Out, "quiet hours are active now: only an urgent message arriving makes a tap")
	}
	fmt.Fprintln(s.Out, soundFocusLine(st))
	if !on {
		fmt.Fprintln(s.Out, "turn them on with `aivoicebus sound on`; `aivoicebus sound test` taps each one first")
	}
	return nil
}
