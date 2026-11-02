package view

import "time"

// SetNow replaces the clock the renderers read, for tests in other packages; it returns what puts it back.
func SetNow(f func() time.Time) (restore func()) {
	old := now
	now = f
	return func() { now = old }
}
