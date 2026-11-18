package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

// The periodic check's decision: only with the setting on, only a release build, only a strictly
// newer release, never the one rolled back from, at most one try a day, and only after this
// machine's share of the fleet spread.
func TestAutoTickDecide(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	on, off := config.AutoUpdate{On: true}, config.AutoUpdate{}
	seen := func(v string, ago time.Duration) config.AutoState {
		return config.AutoState{Seen: now.Add(-ago), SeenTo: v}
	}
	type st = config.AutoState
	cases := []struct {
		name         string
		latest, here string
		a            config.AutoUpdate
		st           config.AutoState
		start        bool
		why          string
	}{
		{"off", "0.3.0", "0.2.0", off, seen("0.3.0", 9*time.Hour), false, "auto-update is off"},
		{"a dev build", "0.3.0", "dev", on, seen("0.3.0", 9*time.Hour), false, "build of its own"},
		{"no announcement", "", "0.2.0", on, st{}, false, "no node announced"},
		{"same release", "0.3.0", "0.3.0", on, seen("0.3.0", 9*time.Hour), false, "up to date"},
		{"older release", "0.2.0", "0.3.0", on, seen("0.2.0", 9*time.Hour), false, "up to date"},
		{"a prerelease is not a release", "0.4.0-rc1", "0.3.0", on, seen("0.4.0-rc1", 9*time.Hour), false, "up to date"},
		{"rolled back here", "0.3.0", "0.2.0", on, st{Skip: "0.3.0", Seen: now.Add(-9 * time.Hour), SeenTo: "0.3.0"}, false, "rolled back here"},
		{"newer than the rolled back one", "0.3.1", "0.2.0", on, st{Skip: "0.3.0", Seen: now.Add(-9 * time.Hour), SeenTo: "0.3.1"}, true, ""},
		{"tried today", "0.3.0", "0.2.0", on, st{Tried: now.Add(-3 * time.Hour), Seen: now.Add(-9 * time.Hour), SeenTo: "0.3.0"}, false, "next try is a day after"},
		{"tried yesterday", "0.3.0", "0.2.0", on, st{Tried: now.Add(-25 * time.Hour), Seen: now.Add(-30 * time.Hour), SeenTo: "0.3.0"}, true, ""},
		{"just seen: waits its share", "0.3.0", "0.2.0", on, seen("0.3.0", 10*time.Minute), false, "waits until"},
		{"its share has passed", "0.3.0", "0.2.0", on, seen("0.3.0", 2*time.Hour), true, ""},
	}
	for _, c := range cases {
		start, why := autoTickDecide(c.latest, c.here, c.a, c.st, time.Hour, now)
		if start != c.start || !strings.Contains(why, c.why) {
			t.Errorf("%s: start %v (%q), want %v (%q)", c.name, start, why, c.start, c.why)
		}
	}
}

// Each machine waits the same share every time, and the shares spread over the six hours.
func TestAutoHoldoffIsFixedPerMachineAndSpread(t *testing.T) {
	if autoHoldoff("imac-m1-imac") != autoHoldoff("imac-m1-imac") {
		t.Fatal("not fixed")
	}
	distinct := map[time.Duration]bool{}
	for _, h := range []string{"pentests-air", "m4-air", "imac-m1-imac", "mac14-m1pro-mbp", "0docker-builder", "grokbot-box"} {
		d := autoHoldoff(h)
		if d < 0 || d >= autoSpread {
			t.Errorf("%s: %v is outside the spread", h, d)
		}
		distinct[d] = true
	}
	if len(distinct) < 4 {
		t.Errorf("the shares do not spread: %v", distinct)
	}
}

func holdoff(t *testing.T, d time.Duration) {
	t.Helper()
	old := machineHoldoff
	t.Cleanup(func() { machineHoldoff = old })
	machineHoldoff = func() time.Duration { return d }
}

// `update --auto tick` asks no node and writes nothing while the setting is off.
func TestAutoTickDoesNothingWhileOff(t *testing.T) {
	dir, pub := signedRelease(t, "0.3.0")
	bin := autoMesh(t, dir, pub)
	if code, out, _ := run(t, "", "update", "--auto", "tick"); code != 0 || !strings.Contains(out, "auto-update is off") {
		t.Fatalf("%d %q", code, out)
	}
	binIs(t, bin, "0.2.0")
	if st := config.LoadAutoState(); !st.Checked.IsZero() || !st.Seen.IsZero() {
		t.Errorf("a check while off records nothing: %+v", st)
	}
}

// With the setting on, the first check that sees a newer release only notes it and waits this
// machine's share; a check after that installs it through the verified path, records it, and does
// not try again the same day.
func TestAutoTickWaitsItsShareThenInstalls(t *testing.T) {
	dir, pub := signedRelease(t, "0.3.0")
	bin := autoMesh(t, dir, pub)
	holdoff(t, time.Hour)
	config.SaveAutoUpdate(config.AutoUpdate{On: true})

	code, out, e := run(t, "", "update", "--auto", "tick")
	if code != 0 || !strings.Contains(out, "nothing done: release 0.3.0 seen") || !strings.Contains(out, "share of the fleet spread") {
		t.Fatalf("first sight: %d %q %q", code, out, e)
	}
	binIs(t, bin, "0.2.0")
	st := config.LoadAutoState()
	if st.SeenTo != "0.3.0" || st.Seen.IsZero() || st.CheckedBy != "tick" || !st.Tried.IsZero() {
		t.Fatalf("the record after first sight: %+v", st)
	}
	if _, out, _ := run(t, "", "update", "--auto", "status"); !strings.Contains(out, "before it installs 0.3.0") || !strings.Contains(out, "last periodic check") {
		t.Errorf("status while waiting: %q", out)
	}

	// A second check in the same hour still waits, and does not move the sighting.
	first := st.Seen
	run(t, "", "update", "--auto", "tick")
	if st := config.LoadAutoState(); !st.Seen.Equal(first) || !st.Tried.IsZero() {
		t.Fatalf("the sighting moved or a run started: %+v", st)
	}

	// Its share has passed.
	_ = config.EditAutoState(func(s *config.AutoState) bool { s.Seen = s.Seen.Add(-2 * time.Hour); return true })
	code, out, e = run(t, "", "update", "--auto", "tick")
	if code != 0 || !strings.Contains(out, "installing it (signature checked first)") || !strings.Contains(out, "installed aivoicebus 0.3.0") || !strings.Contains(out, "installed 0.3.0 (over 0.2.0)") {
		t.Fatalf("after its share: %d %q %q", code, out, e)
	}
	binIs(t, bin, "0.3.0")
	binIs(t, bin+".prev", "0.2.0")
	if st := config.LoadAutoState(); st.Result != config.AutoInstalled || st.To != "0.3.0" || st.Tried.IsZero() {
		t.Fatalf("the record: %+v", st)
	}

	// This process still says 0.2.0, but a run was tried today: nothing more.
	if code, out, _ := run(t, "", "update", "--auto", "tick"); code != 0 || !strings.Contains(out, "next try is a day after") {
		t.Fatalf("again: %d %q", code, out)
	}
}

// A release whose signature no trusted key made is never installed by a check: the run fails,
// records why, and the binary is untouched.
func TestAutoTickRefusesAnUntrustedRelease(t *testing.T) {
	dir, _ := signedRelease(t, "0.3.0")
	_, other := signedRelease(t, "0.3.0") // a key, but not the one that signed dir
	bin := autoMesh(t, dir, other)
	holdoff(t, 0)
	config.SaveAutoUpdate(config.AutoUpdate{On: true})
	code, _, e := run(t, "", "update", "--auto", "tick")
	if code == 0 || !strings.Contains(e, "refusing the release") {
		t.Fatalf("%d %q", code, e)
	}
	binIs(t, bin, "0.2.0")
	if st := config.LoadAutoState(); st.Result != config.AutoFailed || !strings.Contains(st.Error, "refusing the release") {
		t.Fatalf("the record: %+v", st)
	}
}
