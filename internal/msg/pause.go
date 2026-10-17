package msg

import (
	"sort"
	"strings"
	"time"
)

// The kill switch. `aivoicebus pause` sends "[aivoicebus pause] until <RFC 3339>: reason" to a
// machine or to all; workers there take no task and stop the one they run, schedules do not fire,
// and sessions are told to stop acting on peer messages. Anyone may pull the brake. Only a person
// at a terminal on the machine that paused takes it off ("[aivoicebus resume]"), or time does.

const (
	pausePrefix  = "[aivoicebus pause] until "
	resumePrefix = "[aivoicebus resume]"
)

// PauseBody is a pause until until.
func PauseBody(until time.Time, reason string) string {
	return pausePrefix + until.UTC().Format(time.RFC3339) + ": " + reason
}

// ResumeBody lifts the pauses this machine sent.
func ResumeBody(note string) string { return strings.TrimSpace(resumePrefix + " " + note) }

// ParsePause reads a pause: until when, and why.
func ParsePause(body string) (time.Time, string, bool) {
	rest, ok := strings.CutPrefix(body, pausePrefix)
	if !ok {
		return time.Time{}, "", false
	}
	when, reason, _ := strings.Cut(rest, ": ")
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(when))
	return t, reason, err == nil
}

// IsResume reports whether body lifts a pause.
func IsResume(body string) bool { return strings.HasPrefix(body, resumePrefix) }

// ActivePause is the pause in force at now among ms (messages addressed to the reader): the
// latest one not lifted by a later resume from the same machine, and not run out.
func ActivePause(ms []Message, now time.Time) (Message, time.Time, bool) {
	sorted := append([]Message(nil), ms...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	var on []Message
	for _, m := range sorted {
		if m.Retracted != nil {
			continue
		}
		if _, _, ok := ParsePause(m.Body); ok {
			on = append(on, m)
			continue
		}
		if IsResume(m.Body) {
			keep := on[:0]
			for _, p := range on {
				if HostOf(p.From) != HostOf(m.From) {
					keep = append(keep, p) // a resume lifts only what its own machine paused
				}
			}
			on = keep
		}
	}
	for i := len(on) - 1; i >= 0; i-- {
		if until, _, _ := ParsePause(on[i].Body); until.After(now) {
			return on[i], until, true
		}
	}
	return Message{}, time.Time{}, false
}
