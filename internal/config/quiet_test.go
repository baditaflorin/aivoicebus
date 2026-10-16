package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestQuietWindowParsesAndCrossesMidnight(t *testing.T) {
	q, err := ParseQuiet("22:00-07:00")
	if err != nil || q.String() != "22:00-07:00" {
		t.Fatalf("%v %v", q, err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 14, h, m, 0, 0, time.Local) }
	for _, c := range []struct {
		t    time.Time
		want bool
	}{
		{at(21, 59), false}, {at(22, 0), true}, {at(23, 30), true}, {at(0, 0), true}, {at(6, 59), true}, {at(7, 0), false}, {at(12, 0), false},
	} {
		if q.Active(c.t) != c.want {
			t.Errorf("%v: %v, want %v", c.t.Format("15:04"), !c.want, c.want)
		}
	}
	day, _ := ParseQuiet("09:00-17:30")
	if !day.Active(at(9, 0)) || day.Active(at(17, 30)) || day.Active(at(8, 59)) {
		t.Error("a same-day window")
	}
	for _, bad := range []string{"", "22:00", "25:00-07:00", "22:00-7", "08:00-08:00", "a-b"} {
		if _, err := ParseQuiet(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if (Quiet{}).Active(at(3, 0)) || (Quiet{}).String() != "off" {
		t.Error("the zero value is no window")
	}
}

func TestQuietWindowIsSavedAndCleared(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if LoadQuiet().String() != "off" {
		t.Fatal("nothing saved yet")
	}
	q, _ := ParseQuiet("23:00-06:30")
	if err := SaveQuiet(q); err != nil || LoadQuiet() != q {
		t.Fatalf("%v %v", err, LoadQuiet())
	}
	if err := SaveQuiet(Quiet{}); err != nil || LoadQuiet().String() != "off" {
		t.Fatalf("clearing: %v", err)
	}
	if err := SaveQuiet(Quiet{}); err != nil {
		t.Errorf("clearing twice is fine: %v", err)
	}
}

func TestQuietUntilIsActiveUntilThenGoneAndSurvivesTheFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	q := Quiet{Until: now.Add(2 * time.Hour)}
	if !q.Active(now) || !q.Active(now.Add(119*time.Minute)) || q.Active(now.Add(2*time.Hour+time.Second)) {
		t.Error("active until the time, not after")
	}
	if q.Empty(now) || !q.Empty(now.Add(3*time.Hour)) {
		t.Error("empty only once the time has passed")
	}
	if err := SaveQuiet(q); err != nil {
		t.Fatal(err)
	}
	got := LoadQuiet()
	if !got.Until.Equal(q.Until.Truncate(time.Second)) || got.String() != "off" || !got.Active(now) {
		t.Errorf("a time with no window survives the files: %+v", got)
	}
	if _, err := os.Stat(quietPath()); err == nil {
		t.Error("a time alone leaves the window file absent, as an older aivoicebus expects for no window")
	}
	// A window and a time together: the time keeps it quiet outside the window too.
	w, _ := ParseQuiet("22:00-07:00")
	w.Until = now.Add(time.Hour)
	if err := SaveQuiet(w); err != nil {
		t.Fatal(err)
	}
	got = LoadQuiet()
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	if got.String() != "22:00-07:00" || got.Until.IsZero() || got.Active(noon.Add(48*time.Hour)) {
		t.Errorf("%+v", got)
	}
	if !got.Active(now) {
		t.Error("quiet right now because of Until, whatever the clock says")
	}
	// Clearing the window keeps the time, and clearing both removes both files.
	if err := SaveQuiet(Quiet{Until: w.Until}); err != nil || LoadQuiet().String() != "off" || !LoadQuiet().Active(now) {
		t.Errorf("%v %+v", err, LoadQuiet())
	}
	if err := SaveQuiet(Quiet{}); err != nil || !LoadQuiet().Empty(now) {
		t.Errorf("%v %+v", err, LoadQuiet())
	}
	if _, err := os.Stat(untilPath()); err == nil {
		t.Error("the until file is removed when there is no time")
	}
	// A time already past is no setting: nothing is written.
	if err := SaveQuiet(Quiet{Until: now.Add(-time.Hour)}); err != nil || !LoadQuiet().Empty(now) {
		t.Errorf("%v %+v", err, LoadQuiet())
	}
	// A garbled until file is ignored, not trusted and not fatal.
	if err := SaveQuiet(w); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(untilPath(), []byte("tomorrow-ish\n"), 0o644)
	if got := LoadQuiet(); got.String() != "22:00-07:00" || !got.Until.IsZero() {
		t.Errorf("%+v", got)
	}
}

// release081ParseQuiet is how aivoicebus 0.8.1 reads the quiet file: the whole text is the window,
// split on the first "-". It is copied here so that a change to the file's format that would turn
// a working window into none for an older binary on the same machine fails a test.
func release081ParseQuiet(s string) (from, to int, err error) {
	f, t, ok := strings.Cut(strings.TrimSpace(s), "-")
	clock := func(s string) (int, error) {
		v, err := time.Parse("15:04", strings.TrimSpace(s))
		return v.Hour()*60 + v.Minute(), err
	}
	fm, ferr := clock(f)
	tm, terr := clock(t)
	if !ok || ferr != nil || terr != nil {
		return 0, 0, fmt.Errorf("%q is not a window", s)
	}
	return fm, tm, nil
}

func TestAnOlderBinaryStillReadsTheWindowWhenAOneOffTimeIsSet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, _ := ParseQuiet("22:00-07:00")
	w.Until = time.Now().Add(time.Hour)
	if err := SaveQuiet(w); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(quietPath())
	if err != nil {
		t.Fatal(err)
	}
	if from, to, err := release081ParseQuiet(string(b)); err != nil || from != 22*60 || to != 7*60 {
		t.Errorf("0.8.1 reads %q as %d-%d, %v", b, from, to, err)
	}
	if _, err := os.Stat(untilPath()); err != nil {
		t.Errorf("the one-off time lives in its own file: %v", err)
	}
}
