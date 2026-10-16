package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// quietCmd's `for` span is bounded: a typo (`for 9999d`) must not silence the hooks for years.
const (
	minQuietFor = time.Minute
	maxQuietFor = 30 * 24 * time.Hour
)

// quietCmd shows, sets or clears the one do-not-disturb setting: a daily window, and/or a
// stretch from now. While it is active the hooks hold back everything except urgent messages,
// and the radar line and update notice stay silent (see config.Quiet).
func quietCmd(args []string, s Streams) error {
	fs := newFlagSet("quiet")
	asJSON := fs.Bool("json", false, "print the setting as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	q := config.LoadQuiet()
	rest := strings.ToLower(strings.TrimSpace(strings.Join(takeJSON(fs.Args(), asJSON), " ")))
	switch word, arg, _ := strings.Cut(rest, " "); {
	case rest == "":
	case rest == "off" || rest == "none" || rest == "clear":
		q = config.Quiet{}
	case word == "for":
		d, err := msg.ParseSpan(strings.TrimSpace(arg))
		switch {
		case err != nil:
			return outbox.Usagef("quiet for: %v (like `quiet for 2h`)", err)
		case d < minQuietFor || d > maxQuietFor:
			return outbox.Usagef("quiet for: %s is out of range: from %s to %s (`quiet off` clears it)", strings.TrimSpace(arg), minQuietFor, "30d")
		}
		q.Until = time.Now().Add(d)
	default:
		w, err := config.ParseQuiet(rest)
		if err != nil {
			return outbox.Usagef("%s (or `for 2h`, or `off`)", err)
		}
		q.From, q.To = w.From, w.To
	}
	if rest != "" {
		if err := config.SaveQuiet(q); err != nil {
			return err
		}
	}
	now := time.Now()
	if *asJSON {
		v := struct {
			Window string     `json:"window"` // "22:00-07:00", or "off"
			Until  *time.Time `json:"until,omitempty"`
			Active bool       `json:"active"`
		}{q.String(), nil, q.Active(now)}
		if q.Until.After(now) {
			v.Until = &q.Until
		}
		return writeJSON(s.Out, v)
	}
	if q.Empty(now) {
		fmt.Fprintln(s.Out, "quiet hours: off")
		return nil
	}
	var parts []string
	if q.String() != "off" {
		parts = append(parts, q.String()+" daily")
	}
	if q.Until.After(now) {
		parts = append(parts, "until "+q.Until.Local().Format("15:04 (Jan 2)"))
	}
	state := "not active now"
	if q.Active(now) {
		state = "active now"
	}
	fmt.Fprintf(s.Out, "quiet hours: %s (local time), %s; urgent messages still arrive, the rest wait in the inbox, and the radar and update notice stay silent\n", strings.Join(parts, ", "), state)
	return nil
}
