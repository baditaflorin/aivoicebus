package outbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func deadlinesPath(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, ".config", "aivoicebus", deadlinesFile)
}

func dl(from string, by time.Time) Deadline {
	id := msg.NewID()
	return Deadline{ID: id, Thread: id, From: from, To: "build/claude.q", Kind: msg.DeadlineReply, Sent: by.Add(-15 * time.Minute), By: by}
}

func TestADeadlineIsRememberedWithoutABodyAndTheFileIsPrivate(t *testing.T) {
	path := deadlinesPath(t)
	at(t, noon)
	d := dl(meID, noon.Add(10*time.Minute))
	if !RememberDeadline(d) {
		t.Fatal("it was not written")
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the file is private: %v %v", fi, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "body") {
		t.Errorf("no body in the file: %s", raw)
	}
	got, ok := DeadlineFor("me", d.ID)
	if !ok || got.ID != d.ID || got.To != d.To || !got.By.Equal(d.By) || got.Kind != msg.DeadlineReply {
		t.Errorf("round trip: %+v %v", got, ok)
	}
	if _, ok := DeadlineFor("other", d.ID); ok {
		t.Error("another machine's list does not hold it")
	}
	if len(DeadlinesFrom("me")) != 1 {
		t.Error("one deadline")
	}
	names, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range names {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temporary file is left: %s", e.Name())
		}
	}
}

func TestARepeatedSendIsOneDeadlineAndTheListIsCapped(t *testing.T) {
	deadlinesPath(t)
	at(t, noon)
	d := dl(meID, noon.Add(time.Hour))
	RememberDeadline(d)
	d.By = noon.Add(2 * time.Hour)
	RememberDeadline(d)
	if got := DeadlinesFrom("me"); len(got) != 1 || !got[0].By.Equal(noon.Add(2*time.Hour)) {
		t.Fatalf("the later one replaces the earlier: %+v", got)
	}
	for i := 0; i < deadlinesKeep+5; i++ {
		RememberDeadline(dl(meID, noon.Add(time.Hour)))
	}
	if n := len(DeadlinesFrom("me")); n != deadlinesKeep {
		t.Errorf("kept %d, want %d", n, deadlinesKeep)
	}
}

func TestMarkingReportedAndAddingRemindersChangeOnlyThatDeadline(t *testing.T) {
	deadlinesPath(t)
	at(t, noon)
	a, b := dl(meID, noon.Add(time.Minute)), dl(meID, noon.Add(time.Minute))
	RememberDeadline(a)
	RememberDeadline(b)
	rem := msg.NewID()
	if !AddReminders("me", a.ID, rem, rem) || !MarkReported("me", a.ID, noon) {
		t.Fatal("not updated")
	}
	ga, _ := DeadlineFor("me", a.ID)
	gb, _ := DeadlineFor("me", b.ID)
	if len(ga.Reminders) != 1 || ga.Reminders[0] != rem || ga.Reported.IsZero() {
		t.Errorf("a: %+v", ga)
	}
	if len(gb.Reminders) != 0 || !gb.Reported.IsZero() {
		t.Errorf("b must not change: %+v", gb)
	}
	if MarkReported("me", msg.NewID(), noon) {
		t.Error("an unknown id is not found")
	}
}

func TestADeadlineEntryThatIsNotShapedLikeWhatWasWrittenIsDropped(t *testing.T) {
	path := deadlinesPath(t)
	at(t, noon)
	good := dl(meID, noon.Add(time.Hour))
	bad := []Deadline{
		func() Deadline { d := dl(meID, noon.Add(time.Hour)); d.Kind = "run"; return d }(),
		func() Deadline { d := dl(meID, noon.Add(time.Hour)); d.ID = "nope"; return d }(),
		func() Deadline { d := dl(meID, noon.Add(time.Hour)); d.From = "other/claude.x"; return d }(), // not this machine's list
		func() Deadline { d := dl(meID, noon.Add(time.Hour)); d.To = strings.Repeat("a", 500); return d }(),
		func() Deadline { d := dl(meID, noon.Add(time.Hour)); d.By = d.Sent.Add(-time.Minute); return d }(),
		func() Deadline { d := dl(meID, noon.Add(time.Hour)); d.By = d.Sent.Add(72 * time.Hour); return d }(),
		func() Deadline { d := dl(meID, noon.Add(time.Hour)); d.Reminders = []string{"x"}; return d }(),
		func() Deadline { d := dl(meID, noon.Add(-60*24*time.Hour)); return d }(), // forgotten
		func() Deadline {
			d := dl(meID, noon.Add(time.Hour))
			d.Reported = noon.Add(-8 * 24 * time.Hour)
			return d
		}(), // reported long ago
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	b, _ := json.Marshal(map[string][]Deadline{"me": append(bad, good)})
	os.WriteFile(path, b, 0o600)
	got := DeadlinesFrom("me")
	if len(got) != 1 || got[0].ID != good.ID {
		t.Errorf("only the well-formed entry is read back: %+v", got)
	}
	os.WriteFile(path, []byte("not json"), 0o600)
	if len(DeadlinesFrom("me")) != 0 {
		t.Error("a corrupt file is an empty book")
	}
	if RememberDeadline(Deadline{ID: "bad"}) {
		t.Error("a malformed deadline is not written")
	}
}

func TestLiveOnListsTheLiveSessionsOfOneMachineSameRepoFirst(t *testing.T) {
	now := noon
	seen := func(me string, ago time.Duration, repo string, listening bool) msg.RosterEntry {
		e := msg.RosterEntry{Me: me, Seen: now.Add(-ago), Listening: listening}
		if repo != "" {
			e.Labels = msg.Labels{"repo": repo}
		}
		return e
	}
	roster := []msg.RosterEntry{
		seen("mac14/claude.me", time.Minute, "x", false),
		seen("mac14/claude.old", 2*time.Hour, "x", false),
		seen("mac14/claude.recent", time.Minute, "", false),
		seen("mac14/claude.listening", 20*time.Minute, "", true),
		seen("mac14/claude.samerepo", 25*time.Minute, "x", false),
		seen("other/claude.a", time.Minute, "x", false),
		seen("mac14/"+msg.ProbeSenderAgent, time.Minute, "", false),
		seen("mac14/claude.skip", time.Minute, "x", false),
	}
	got := LiveOn(roster, "mac14", "mac14/claude.me", "x", now, LiveWithin, "mac14/claude.skip")
	var names []string
	for _, e := range got {
		names = append(names, e.Me)
	}
	want := []string{"mac14/claude.samerepo", "mac14/claude.listening", "mac14/claude.recent"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("LiveOn = %v, want %v", names, want)
	}
}
