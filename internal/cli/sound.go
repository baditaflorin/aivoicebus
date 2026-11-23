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

const soundUsage = "usage: aivoicebus sound [on | off | toggle | test [EVENT] | set EVENT NAME|/path/to/file|off|default | volume 0.05-1|full | list]   (EVENT: sent, received, retracted)"

// soundCmd shows or changes the one setting for the sounds a message makes on this machine: when it
// is sent, when it arrives, when it is taken back. Off until the person turns it on (see internal/sound).
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
	case "list":
		if err := sound.Supported(); err != nil {
			return outbox.Usagef("%v", err)
		}
		fmt.Fprintln(s.Out, strings.Join(sound.List(), "\n"))
		return nil
	case "test":
		return soundTest(rest, s)
	case "set":
		return soundSet(rest, s)
	case "volume":
		return soundVolume(rest, s)
	}
	return outbox.Usagef("sound: %q is not a command here\n%s", word, soundUsage)
}

// soundSwitch is the master switch: on, off, or the other of the two. Each event can also be silenced
// by itself (`sound set sent off`), and AIVOICEBUS_SOUND=0 or 1 decides for one process whatever is saved.
func soundSwitch(word string, s Streams) error {
	want := word == "on"
	if word == "toggle" {
		want = !sound.Load().On
	}
	if want {
		if err := sound.Supported(); err != nil {
			return outbox.Usagef("%v", err)
		}
	}
	if err := sound.Update(func(st *sound.Settings) error { st.On = want; return nil }); err != nil {
		return err
	}
	if !want {
		fmt.Fprintln(s.Out, "sounds: off")
	} else {
		fmt.Fprintln(s.Out, "sounds: on. Hear them with `aivoicebus sound test`; quiet hours silence them (`aivoicebus quiet`)")
	}
	if env := strings.TrimSpace(os.Getenv("AIVOICEBUS_SOUND")); env != "" && sound.Enabled() != want {
		fmt.Fprintf(s.Out, "note: AIVOICEBUS_SOUND=%s in this environment overrides the saved setting\n", env)
	}
	return nil
}

func soundTest(rest []string, s Streams) error {
	events := sound.Events
	if len(rest) > 1 {
		return outbox.Usagef("%s", soundUsage)
	}
	if len(rest) == 1 {
		e, ok := sound.ParseEvent(rest[0])
		if !ok {
			return outbox.Usagef("sound test: %q is not an event (sent, received, retracted)", rest[0])
		}
		events = []sound.Event{e}
	}
	for i, e := range events {
		name, _, err := sound.Describe(sound.Load(), e)
		if err != nil {
			return outbox.Usagef("sound test: %s: %v", e, err)
		}
		if name == sound.Off {
			fmt.Fprintf(s.Out, "%-9s off\n", e)
			continue
		}
		fmt.Fprintf(s.Out, "%-9s %s\n", e, name)
		if err := sound.Test(e); err != nil {
			return fmt.Errorf("sound test: %s: %v", e, err)
		}
		if i < len(events)-1 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	return nil
}

func soundSet(rest []string, s Streams) error {
	if len(rest) != 2 {
		return outbox.Usagef("%s", soundUsage)
	}
	e, ok := sound.ParseEvent(rest[0])
	if !ok {
		return outbox.Usagef("sound set: %q is not an event (sent, received, retracted)", rest[0])
	}
	name := rest[1]
	if lower := strings.ToLower(name); lower == sound.Off || lower == sound.Default {
		name = lower
	} else if err := sound.Supported(); err != nil {
		return outbox.Usagef("sound set: %v", err)
	} else if _, err := sound.Resolve(name, e); err != nil {
		return outbox.Usagef("sound set: %v", err) // a name or path that is not there is refused now, not heard as silence later
	}
	err := sound.Update(func(st *sound.Settings) error {
		if name == sound.Default {
			delete(st.Sounds, e)
			return nil
		}
		if st.Sounds == nil {
			st.Sounds = map[sound.Event]string{}
		}
		st.Sounds[e] = name
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "%s: %s\n", e, name)
	return nil
}

func soundVolume(rest []string, s Streams) error {
	if len(rest) != 1 {
		return outbox.Usagef("%s", soundUsage)
	}
	v := 0.0 // full
	if arg := strings.ToLower(rest[0]); arg != "full" && arg != "1" {
		var err error
		if v, err = strconv.ParseFloat(strings.TrimSuffix(arg, "%"), 64); err != nil {
			return outbox.Usagef("sound volume: %q is not a number from 0.05 to 1 (or `full`)", rest[0])
		}
		if strings.HasSuffix(arg, "%") {
			v /= 100
		}
		if !sound.ValidVolume(v) {
			return outbox.Usagef("sound volume: %q is out of range: from 0.05 to 1 (or `full`)", rest[0])
		}
	}
	if err := sound.Update(func(st *sound.Settings) error { st.Volume = v; return nil }); err != nil {
		return err
	}
	if v == 0 {
		fmt.Fprintln(s.Out, "volume: full")
	} else {
		fmt.Fprintf(s.Out, "volume: %.2f of the system volume\n", v)
	}
	return nil
}

func soundStatus(s Streams, asJSON bool) error {
	st := sound.Load()
	on := sound.Enabled()
	type row struct {
		Event string `json:"event"`
		Sound string `json:"sound"`
		File  string `json:"file,omitempty"`
		Error string `json:"error,omitempty"`
	}
	var rows []row
	for _, e := range sound.Events {
		name, path, err := sound.Describe(st, e)
		r := row{Event: string(e), Sound: name, File: path}
		if err != nil {
			r.Error = err.Error()
		}
		rows = append(rows, r)
	}
	env := strings.TrimSpace(os.Getenv("AIVOICEBUS_SOUND"))
	quiet := config.LoadQuiet().Active(time.Now())
	if asJSON {
		return writeJSON(s.Out, struct {
			On     bool    `json:"on"`
			Saved  bool    `json:"saved"` // the saved setting; on may differ when AIVOICEBUS_SOUND says so
			Volume float64 `json:"volume"`
			Quiet  bool    `json:"quiet_now"`
			Events []row   `json:"events"`
		}{on, st.On, volumeOrFull(st.Volume), quiet, rows})
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
		width = max(width, len(r.Sound))
	}
	for _, r := range rows {
		line := fmt.Sprintf("  %-9s %-*s", r.Event, width, r.Sound)
		switch {
		case r.Error != "":
			line += "   !! " + r.Error
		case r.File != "":
			line += "   " + r.File
		}
		fmt.Fprintln(s.Out, line)
	}
	if st.Volume > 0 {
		fmt.Fprintf(s.Out, "volume: %.2f of the system volume\n", st.Volume)
	} else {
		fmt.Fprintln(s.Out, "volume: full")
	}
	if quiet {
		fmt.Fprintln(s.Out, "quiet hours are active now: only an urgent message arriving makes a sound")
	}
	if !on {
		fmt.Fprintln(s.Out, "turn them on with `aivoicebus sound on`; `aivoicebus sound test` plays each one first")
	}
	return nil
}

func volumeOrFull(v float64) float64 {
	if v <= 0 {
		return 1
	}
	return v
}
