package view

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Times a person reads are local: the clock alone today, the date and the zone otherwise, so a
// UTC time cannot be mistaken for a local one and yesterday cannot pass for today.
func TestWhenIsLocalWithTheDateAndZoneWhenNotToday(t *testing.T) {
	zone := time.FixedZone("EEST", 3*3600)
	old := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = old })
	now := time.Date(2026, 10, 18, 15, 0, 0, 0, zone)

	cases := []struct {
		at        time.Time
		when, sec string
	}{
		{time.Date(2026, 10, 18, 11, 3, 7, 0, time.UTC), "14:03", "14:03:07"},                                   // today here, given in UTC
		{time.Date(2026, 10, 17, 22, 30, 0, 0, time.UTC), "01:30", "01:30:00"},                                  // yesterday in UTC, today here
		{time.Date(2026, 10, 17, 11, 3, 0, 0, time.UTC), "Oct 17 14:03 EEST", "Oct 17 14:03:00 EEST"},           // yesterday
		{time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC), "Oct 19 09:00 EEST", "Oct 19 09:00:00 EEST"},            // tomorrow (an expiry)
		{time.Date(2025, 12, 31, 20, 0, 0, 0, time.UTC), "2025 Dec 31 23:00 EEST", "2025 Dec 31 23:00:00 EEST"}, // another year
		{time.Time{}, "never", "never"},
	}
	for _, c := range cases {
		if got := When(c.at, now); got != c.when {
			t.Errorf("When(%s) = %q, want %q", c.at, got, c.when)
		}
		if got := WhenSec(c.at, now); got != c.sec {
			t.Errorf("WhenSec(%s) = %q, want %q", c.at, got, c.sec)
		}
		if strings.Contains(When(c.at, now), "UTC") {
			t.Errorf("a UTC time leaked into %q", When(c.at, now))
		}
	}
}

// A session is named with its title and its state wherever it is listed or picked: two short
// hashes of one machine are easy to mix up, a title and "listening" are not.
func TestSessionNamesTheTitleAndWhetherItIsListening(t *testing.T) {
	now := time.Now()
	cases := []struct {
		e    msg.RosterEntry
		want string
	}{
		{msg.RosterEntry{Me: "mac14-m1pro-mbp/claude.aac5", Title: "Aivoicebus standby", Listening: true, Seen: now},
			`mac14-m1pro-mbp/claude.aac5 "Aivoicebus standby" (listening)`},
		{msg.RosterEntry{Me: "mac14-m1pro-mbp/claude.632e", Title: "Fix the login redirect", Seen: now.Add(-time.Minute).Truncate(time.Minute)},
			`mac14-m1pro-mbp/claude.632e "Fix the login redirect" (seen ` + When(now.Add(-time.Minute).Truncate(time.Minute), now) + `)`},
		{msg.RosterEntry{Me: "build/codex.q1w2", Labels: msg.Labels{"tool": "codex"}, Listening: true},
			`build/codex.q1w2 (listening)`}, // the id already says codex
		{msg.RosterEntry{Me: "build/reviewer", Labels: msg.Labels{"tool": "codex"}, Listening: true},
			`build/reviewer [codex] (listening)`}, // a registered name does not
		{msg.RosterEntry{Me: "air/claude.x1", Title: "say \"hi\"\nnow", Listening: true},
			`air/claude.x1 "say 'hi' now" (listening)`}, // a title cannot break the line
	}
	for _, c := range cases {
		if got := Session(c.e, now); got != c.want {
			t.Errorf("Session(%+v)\n got %s\nwant %s", c.e, got, c.want)
		}
	}
}
