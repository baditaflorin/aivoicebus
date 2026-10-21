package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func presenceStore(t *testing.T, dir string) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(dir, "n1")
	if err != nil {
		t.Fatal(err)
	}
	// Open judges presence.json by the wall clock (a restarted node drops sessions past seenKeep)
	// before any test clock is set, so the test clock starts at the wall clock: a fixed date would
	// make every restart check fail once it is a week old.
	now := time.Now().UTC().Truncate(time.Second)
	s.SetClock(func() time.Time { return now })
	return s, &now
}

func rosterOf(s *Store) map[string]msg.RosterEntry {
	out := map[string]msg.RosterEntry{}
	for _, e := range s.Roster() {
		out[e.Me] = e
	}
	return out
}

func TestMergePresenceTakesTheLaterSightingAndKeepsLabelsThatWereDeclared(t *testing.T) {
	s, now := presenceStore(t, t.TempDir())
	t0 := *now
	s.InboxFor("mac/claude.a", false, msg.Labels{"repo": "alpha"}) // seen locally at t0, with labels
	*now = t0.Add(time.Minute)

	n := s.MergePresence([]msg.RosterEntry{
		{Me: "mac/claude.a", Seen: t0.Add(30 * time.Second)},                                    // later, but declares nothing
		{Me: "air/codex.b", Seen: t0.Add(10 * time.Second), Labels: msg.Labels{"repo": "beta"}}, // new
		{Me: "imac/claude.c", Seen: t0.Add(-time.Hour)},                                         // an old one that is still in range
	})
	if n != 3 {
		t.Fatalf("changed %d, want 3", n)
	}
	r := rosterOf(s)
	if e := r["mac/claude.a"]; !e.Seen.Equal(t0.Add(30*time.Second)) || e.Labels["repo"] != "alpha" {
		t.Errorf("a later sighting without labels keeps the labels already known: %+v", e)
	}
	if e := r["air/codex.b"]; e.Labels["repo"] != "beta" {
		t.Errorf("%+v", e)
	}

	// an older sighting never goes back in time, but may add labels where there were none
	s.MergePresence([]msg.RosterEntry{
		{Me: "imac/claude.c", Seen: t0.Add(-2 * time.Hour), Labels: msg.Labels{"repo": "gamma"}},
		{Me: "mac/claude.a", Seen: t0.Add(-time.Hour), Labels: msg.Labels{"repo": "STALE"}},
	})
	r = rosterOf(s)
	if e := r["imac/claude.c"]; !e.Seen.Equal(t0.Add(-time.Hour)) || e.Labels["repo"] != "gamma" {
		t.Errorf("an older sighting fills in missing labels but not the time: %+v", e)
	}
	if e := r["mac/claude.a"]; e.Labels["repo"] != "alpha" || !e.Seen.Equal(t0.Add(30*time.Second)) {
		t.Errorf("an older sighting must not overwrite a newer one: %+v", e)
	}
	if n := s.MergePresence([]msg.RosterEntry{{Me: "air/codex.b", Seen: t0.Add(10 * time.Second), Labels: msg.Labels{"repo": "beta"}}}); n != 0 {
		t.Errorf("the same news again changes nothing: %d", n)
	}
}

func TestMergePresenceClampsFutureClocksAndIgnoresTheAncient(t *testing.T) {
	s, now := presenceStore(t, t.TempDir())
	s.MergePresence([]msg.RosterEntry{
		{Me: "wrongclock/claude", Seen: now.Add(48 * time.Hour)}, // a peer whose clock is two days fast
		{Me: "ancient/claude", Seen: now.Add(-10 * 24 * time.Hour)},
		{Me: "", Seen: *now},
	})
	r := rosterOf(s)
	if e, ok := r["wrongclock/claude"]; !ok || e.Seen.After(now.Add(maxClockAhead)) {
		t.Errorf("a sighting from the future is clamped to now, or it would outlive every real one: %+v", e)
	}
	if _, ok := r["ancient/claude"]; ok {
		t.Error("a session past the retention window is not taken in")
	}
	if len(r) != 1 {
		t.Errorf("%v", r)
	}
}

func TestPresenceSurvivesARestartAndAFlushIsThrottled(t *testing.T) {
	dir := t.TempDir()
	s, now := presenceStore(t, dir)
	s.InboxFor("mac/claude.a", false, msg.Labels{"repo": "alpha", "branch": "main"})
	if _, err := os.Stat(filepath.Join(dir, presenceFile)); err != nil {
		t.Fatalf("the first change is written at once: %v", err)
	}
	first, _ := os.ReadFile(filepath.Join(dir, presenceFile))
	*now = now.Add(time.Second)
	s.InboxFor("air/codex.b", false, msg.Labels{"repo": "beta"})
	if again, _ := os.ReadFile(filepath.Join(dir, presenceFile)); string(again) != string(first) {
		t.Error("a second change within five (wall clock) seconds waits for the next flush")
	}
	s.FlushPresence()
	if again, _ := os.ReadFile(filepath.Join(dir, presenceFile)); string(again) == string(first) {
		t.Error("an explicit flush writes it")
	}
	if info, _ := os.Stat(filepath.Join(dir, presenceFile)); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}

	// a new process on the same disk knows who was around and what they worked on
	re, err := Open(dir, "n1")
	if err != nil {
		t.Fatal(err)
	}
	re.SetClock(func() time.Time { return *now })
	r := rosterOf(re)
	if e := r["mac/claude.a"]; e.Labels["repo"] != "alpha" || e.Labels["branch"] != "main" || e.Seen.IsZero() {
		t.Errorf("after a restart: %+v", e)
	}
	if r["air/codex.b"].Labels["repo"] != "beta" {
		t.Errorf("after a restart: %+v", r)
	}

	// a week later they are gone
	later, _ := Open(dir, "n1")
	later.SetClock(func() time.Time { return now.Add(8 * 24 * time.Hour) })
	later.loadPresence()
	if len(later.Roster()) > 2 { // loadPresence ran at Open with the real clock; reload with the late one
		t.Errorf("sessions older than the retention window are dropped on load: %v", later.Roster())
	}
}

func TestACorruptPresenceFileIsIgnoredNotFatal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, presenceFile), []byte("{not json"), 0o600)
	s, err := Open(dir, "n1")
	if err != nil || len(s.Roster()) != 0 {
		t.Fatalf("%v %v", err, s.Roster())
	}
}

func TestRosterSinceReturnsOnlyWhatIsNewer(t *testing.T) {
	s, now := presenceStore(t, t.TempDir())
	s.InboxFor("old/claude", false, nil)
	*now = now.Add(time.Minute)
	cut := *now
	*now = now.Add(time.Minute)
	s.InboxFor("new/claude", false, nil)
	got := s.RosterSince(cut)
	if len(got) != 1 || got[0].Me != "new/claude" {
		t.Errorf("%+v", got)
	}
	if all := s.RosterSince(time.Time{}); len(all) != 2 {
		t.Errorf("the zero time means everything: %+v", all)
	}
}

// A peer that holds a session's older labels but heard of a newer send from it reports a later Seen
// with those labels. Taking them as news dropped a room the session had joined since (0.7.1, live):
// labels are judged by when they were declared, and that time survives a restart.
func TestMergePresenceJudgesLabelsByWhenTheyWereDeclared(t *testing.T) {
	dir := t.TempDir()
	s, now := presenceStore(t, dir)
	t0 := *now
	*now = t0.Add(time.Minute)
	s.InboxFor("mac/claude.a", false, msg.Labels{"tool": "claude", "rooms": "v07"}) // declared at t0+1m

	stale := msg.RosterEntry{Me: "mac/claude.a", Seen: t0.Add(2 * time.Minute), LabelsAt: t0, Labels: msg.Labels{"tool": "claude"}}
	*now = t0.Add(3 * time.Minute)
	if n := s.MergePresence([]msg.RosterEntry{stale}); n != 1 {
		t.Fatalf("changed %d, want 1 (the later sighting)", n)
	}
	e := rosterOf(s)["mac/claude.a"]
	if e.Labels["rooms"] != "v07" || !e.LabelsAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("older labels with a newer sighting replaced the newer labels: %+v", e)
	}
	if !e.Seen.Equal(t0.Add(2 * time.Minute)) {
		t.Errorf("the sighting still moves on: %+v", e)
	}

	newer := msg.RosterEntry{Me: "mac/claude.a", Seen: t0.Add(2 * time.Minute), LabelsAt: t0.Add(2 * time.Minute), Labels: msg.Labels{"tool": "claude"}}
	s.MergePresence([]msg.RosterEntry{newer})
	if e := rosterOf(s)["mac/claude.a"]; e.Labels["rooms"] != "" {
		t.Errorf("labels declared later (the session left the room) win: %+v", e)
	}

	s.FlushPresence()
	r, _ := presenceStore(t, dir)
	if e := rosterOf(r)["mac/claude.a"]; !e.LabelsAt.Equal(t0.Add(2*time.Minute)) || !e.Seen.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("when the labels were declared is kept across a restart: %+v", e)
	}
}
