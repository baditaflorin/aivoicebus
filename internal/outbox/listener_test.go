package outbox

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// A message to one session that is not listening, while another session of the same machine is
// standing by, gets a warning naming both with their titles: a hand-off once went to the wrong one
// of two look-alike ids on one machine.
func TestListenerWarningNamesTheSessionStandingBy(t *testing.T) {
	now := time.Now()
	roster := []msg.RosterEntry{
		{Me: "mac14/claude.632e", Title: "Old refactor", Seen: now.Add(-time.Hour)},
		{Me: "mac14/claude.aac5", Title: "Aivoicebus standby", Seen: now, Listening: true},
		{Me: "mac14/worker", Seen: now, Listening: true},  // a worker is not a session someone meant
		{Me: "air/claude.b1", Seen: now, Listening: true}, // another machine
		{Me: "me/claude.0001", Seen: now, Listening: true},
	}
	w := ListenerWarning(roster, "mac14/claude.632e", "me/claude.0001", now)
	for _, want := range []string{`mac14/claude.632e "Old refactor" (seen`, "is not listening", `mac14/claude.aac5 "Aivoicebus standby" (listening)`, "send to it instead"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing %q in %q", want, w)
		}
	}
	for _, not := range []string{"worker", "air/"} {
		if strings.Contains(w, not) {
			t.Errorf("%q should not be offered: %q", not, w)
		}
	}
	if w := ListenerWarning(roster, "MAC14/claude.632E", "me/claude.0001", now); w == "" {
		t.Error("an id in capitals is the same session")
	}
	if w := ListenerWarning(roster, "mac14/claude.dead", "me/claude.0001", now); !strings.Contains(w, "has not been seen") { // a typo in the id
		t.Errorf("an unknown id: %q", w)
	}

	quiet := []string{
		"mac14/claude.aac5", // the one listening
		"mac14",             // a machine: whoever reads first
		"mac14/claude",      // a tool there
		"@repo=x",           // a selector
		"imac/claude.c2",    // nobody listening on imac
	}
	for _, to := range quiet {
		if w := ListenerWarning(roster, to, "me/claude.0001", now); w != "" {
			t.Errorf("%s: no warning expected, got %q", to, w)
		}
	}
	// A session long gone is not "standing by", even if a node still says so.
	stale := []msg.RosterEntry{roster[0], {Me: "mac14/claude.aac5", Seen: now.Add(-48 * time.Hour), Listening: true}}
	if w := ListenerWarning(stale, "mac14/claude.632e", "me/claude.0001", now); w != "" {
		t.Errorf("a stale listener: %q", w)
	}
	// The sender standing by itself is not the alternative.
	self := []msg.RosterEntry{roster[0], {Me: "mac14/claude.aac5", Seen: now, Listening: true}}
	if w := ListenerWarning(self, "mac14/claude.632e", "mac14/claude.aac5", now); w != "" {
		t.Errorf("the sender itself: %q", w)
	}
}

// The warning reaches the sender through Prepare, which send, reply, task and token hand share,
// and does not stop the message.
func TestPrepareWarnsWhenAnotherSessionThereIsListening(t *testing.T) {
	c, s := meshWith(t, "me/claude.0001", map[string]string{"mac14/claude.632e": "", "mac14/claude.aac5": ""})
	s.NoteTitle("mac14/claude.aac5", "Aivoicebus standby")
	s.NoteWaiting("mac14/claude.aac5", time.Now().Add(time.Minute))
	var warn strings.Builder
	p, err := Prepare(c, Request{To: "mac14/claude.632e", Body: "take over the release"}, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if p.Msg.To != "mac14/claude.632e" {
		t.Errorf("the target was changed: %s", p.Msg.To)
	}
	if !strings.Contains(warn.String(), `mac14/claude.aac5 "Aivoicebus standby" (listening)`) {
		t.Errorf("warn: %q", warn.String())
	}
	warn.Reset()
	if _, err := Prepare(c, Request{To: "mac14/claude.aac5", Body: "take over the release, again"}, &warn); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(warn.String(), "listening") {
		t.Errorf("to the listener itself: %q", warn.String())
	}
}
