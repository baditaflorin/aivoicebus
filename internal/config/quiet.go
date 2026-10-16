package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Quiet is the one do-not-disturb setting. While it is active the session-start and per-prompt
// hooks hold back everything except urgent messages (asks wait in the inbox and are only
// counted), and the collision radar and the update notice stay silent. It is active inside a
// daily window, or until a one-off time (`aivoicebus quiet for 2h`). The zero value is neither.
// Times are on the local clock.
type Quiet struct {
	From, To int       // minutes after midnight; From == To is no window
	Until    time.Time // active until then, whatever the window says; zero is no such time
}

// ParseQuiet reads "22:00-07:00". A window may cross midnight.
func ParseQuiet(s string) (Quiet, error) {
	from, to, ok := strings.Cut(strings.TrimSpace(s), "-")
	f, ferr := clockMinutes(from)
	t, terr := clockMinutes(to)
	if !ok || ferr != nil || terr != nil {
		return Quiet{}, fmt.Errorf("%q is not a window like 22:00-07:00", s)
	}
	if f == t {
		return Quiet{}, fmt.Errorf("%q starts and ends at the same time", s)
	}
	return Quiet{From: f, To: t}, nil
}

func clockMinutes(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	return t.Hour()*60 + t.Minute(), err
}

// Active reports whether t falls inside the window or before Until.
func (q Quiet) Active(t time.Time) bool {
	if t.Before(q.Until) {
		return true
	}
	if q.From == q.To {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if q.From < q.To {
		return m >= q.From && m < q.To
	}
	return m >= q.From || m < q.To // crosses midnight
}

// String is the window ("22:00-07:00"), or "off" when there is none; Until is not part of it.
func (q Quiet) String() string {
	if q.From == q.To {
		return "off"
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", q.From/60, q.From%60, q.To/60, q.To%60)
}

// Empty reports whether q holds no window and no time still ahead of now.
func (q Quiet) Empty(now time.Time) bool { return q.From == q.To && !now.Before(q.Until) }

func quietPath() string {
	if d := Dir(); d != "" {
		return d + "/quiet"
	}
	return ""
}

// untilPath is the one-off time, in a file of its own beside the window's: an older aivoicebus
// reads `quiet` by splitting it on the first "-", so anything else in that file would turn a
// working window into none for it.
func untilPath() string {
	if p := quietPath(); p != "" {
		return p + ".until"
	}
	return ""
}

// LoadQuiet reads the saved setting; a missing or unreadable file means none.
func LoadQuiet() Quiet {
	p := quietPath()
	if p == "" {
		return Quiet{}
	}
	var q Quiet
	if b, err := os.ReadFile(p); err == nil {
		if w, err := ParseQuiet(string(b)); err == nil {
			q = w
		}
	}
	if b, err := os.ReadFile(untilPath()); err == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err == nil {
			q.Until = t
		}
	}
	return q
}

// SaveQuiet stores the setting: the window in `quiet` and the time in `quiet.until`, each removed
// when there is nothing to keep in it.
func SaveQuiet(q Quiet) error {
	p := quietPath()
	if p == "" {
		return fmt.Errorf("no home directory to keep settings in")
	}
	if err := writeOrRemove(p, q.From != q.To, q.String()+"\n"); err != nil {
		return err
	}
	return writeOrRemove(untilPath(), q.Until.After(time.Now()), q.Until.UTC().Format(time.RFC3339)+"\n") // a time already past is no setting
}

func writeOrRemove(path string, keep bool, text string) error {
	if !keep {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o644)
}

// GroupsText is the sender's saved selector aliases (~/.config/aivoicebus/groups), as written.
func GroupsText() string {
	if d := Dir(); d != "" {
		b, _ := os.ReadFile(d + "/groups")
		return string(b)
	}
	return ""
}
