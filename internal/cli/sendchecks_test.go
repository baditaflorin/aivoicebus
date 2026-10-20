package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// deadSession makes `lap/claude.dead` a session the node last heard from three hours ago, with
// `lap/claude.live` seen now on the same machine and in the same repo, and leaves the CLI acting as
// `desk/claude.me`.
func deadSession(t *testing.T) *store.Store {
	t.Helper()
	s := meshStore(t)
	s.SetClock(func() time.Time { return time.Now().Add(-3 * time.Hour) })
	session(t, "lap/claude.dead", "repo=alpha")
	run(t, "", "inbox")
	s.SetClock(time.Now)
	session(t, "lap/claude.live", "repo=alpha")
	run(t, "", "inbox")
	session(t, "desk/claude.me", "")
	return s
}

// held is the message id as the node stores it.
func held(t *testing.T, s *store.Store, id string) msg.Message {
	t.Helper()
	for _, m := range s.Thread(id) {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("the node does not hold %s", id)
	return msg.Message{}
}

func TestSendToAnExactSessionNotSeenFor30MinutesIsRefused(t *testing.T) {
	s := deadSession(t)
	code, out, e := run(t, "", "send", "--to", "lap/claude.dead", "the PR is ready")
	if code == 0 || out != "" {
		t.Fatalf("it was sent: %d %q", code, out)
	}
	for _, want := range []string{"refused: lap/claude.dead was last seen", "3h ago", "live on lap now: lap/claude.live (repo alpha,", `ask lap "..."`, `task lap "..."`, "--queue (or --force)"} {
		if !strings.Contains(e, want) {
			t.Errorf("stderr is missing %q:\n%s", want, e)
		}
	}
	if code, _, e := run(t, "", "send", "--to", "lap/claude.dead", "--dry-run", "the PR is ready"); code == 0 || !strings.Contains(e, "refused: lap/claude.dead") {
		t.Errorf("a dry run shows the refusal too: %d %q", code, e)
	}
	if n := s.Stats().Messages; n != 0 {
		t.Errorf("a refused send stored %d message(s)", n)
	}
	for _, flag := range []string{"--queue", "--force"} {
		code, out, e := run(t, "", "send", "--to", "lap/claude.dead", flag, "the PR is ready")
		if code != 0 || !strings.HasPrefix(out, "sent ") {
			t.Errorf("%s: %d %q %q", flag, code, out, e)
		}
	}
	// Whoever is live, or the whole machine, is not a stale session.
	for _, to := range []string{"lap/claude.live", "lap/claude", "lap", "@repo=alpha"} {
		if code, out, e := run(t, "", "send", "--to", to, "the PR is ready"); code != 0 {
			t.Errorf("%s: %d %q %q", to, code, out, e)
		}
	}
	// A session the nodes never heard of is stale as well.
	if code, _, e := run(t, "", "send", "--to", "lap/claude.nobody", "hello"); code == 0 || !strings.Contains(e, "has not been seen by the nodes") {
		t.Errorf("never seen: %d %q", code, e)
	}
	// task names the session too.
	if code, _, e := run(t, "", "task", "lap/claude.dead", "do this"); code == 0 || !strings.Contains(e, "refused: lap/claude.dead") {
		t.Errorf("task to a dead session: %d %q", code, e)
	}
}

func TestAReplyIsWarnedAboutAGoneAskerButSent(t *testing.T) {
	s := deadSession(t)
	session(t, "lap/claude.dead", "repo=alpha")
	s.SetClock(func() time.Time { return time.Now().Add(-3 * time.Hour) }) // sending moves a session's last-seen: it asked 3h ago
	_, out, e := run(t, "", "send", "--to", "desk/claude.me", "--queue", "can you check this?")
	s.SetClock(time.Now)
	asked, _ := sentID(t, out)
	if asked == "" {
		t.Fatalf("setup: %q", e)
	}
	session(t, "desk/claude.me", "")
	code, out, e := run(t, "", "reply", "--re", asked, "checked, fine")
	if code != 0 || !strings.HasPrefix(out, "sent ") || !strings.Contains(e, "warning: the reply goes to lap/claude.dead") {
		t.Errorf("reply: %d %q %q", code, out, e)
	}
	// With --to the sender chose the target, so the same words are refused.
	if code, _, e := run(t, "", "send", "--to", "lap/claude.dead", "--re", asked, "checked, fine"); code == 0 || !strings.Contains(e, "refused: lap/claude.dead") {
		t.Errorf("send --to --re: %d %q", code, e)
	}
}

func TestSendLowersAMessageThatSaysItNeedsNothing(t *testing.T) {
	s := deadSession(t)
	session(t, "desk/claude.me", "")
	code, out, e := run(t, "", "send", "--to", "lap/claude.live", "FYI the build is green, no action needed")
	if code != 0 || !strings.Contains(e, "(sent as fyi; --prio ask to interrupt)") {
		t.Fatalf("lowered: %d %q %q", code, out, e)
	}
	id, _ := sentID(t, out)
	if m := held(t, s, id); m.Prio != msg.PrioFYI {
		t.Errorf("the message went out as %q", m.Prio)
	}
	// --json carries the note too.
	_, out, _ = run(t, "", "send", "--to", "lap/claude.live", "--json", "DONE: merged")
	var sent sentView
	if err := json.Unmarshal([]byte(out), &sent); err != nil || !strings.Contains(strings.Join(sent.Notes, ";"), "sent as fyi") {
		t.Errorf("%v %q", err, out)
	}
	// Chosen priority, a question, a request, or another kind: left as sent.
	for _, args := range [][]string{
		{"--prio", "ask", "FYI the build is green"},
		{"--prio", "urgent", "FYI the build is red"},
		{"FYI the build is green, should I ship?"},
		{"FYI the host is full. Please do not start another install"},
		{"--kind", "question", "FYI who owns the gateway"},
	} {
		code, out, e := run(t, "", append([]string{"send", "--to", "lap/claude.live"}, args...)...)
		if code != 0 || strings.Contains(e, "sent as fyi") {
			t.Fatalf("%v: %d %q %q", args, code, out, e)
		}
		id, _ := sentID(t, out)
		if m := held(t, s, id); m.Prio == msg.PrioFYI {
			t.Errorf("%v went out as fyi", args)
		}
	}
	// A dry run shows the lowering.
	if _, out, _ := run(t, "", "send", "--to", "lap/claude.live", "--dry-run", "FYI the build is green"); !strings.Contains(out, "priority  fyi") || !strings.Contains(out, "lowered to fyi") {
		t.Errorf("dry run:\n%s", out)
	}
}

func TestSendWarnsWhenAQuestionGoesOutAsFYI(t *testing.T) {
	deadSession(t)
	session(t, "desk/claude.me", "")
	code, out, e := run(t, "", "send", "--to", "lap/claude.live", "--prio", "fyi", "who owns vm701 tuning?")
	if code != 0 || !strings.HasPrefix(out, "sent ") || !strings.Contains(e, "warning: this is sent as fyi") || !strings.Contains(e, "--kind question --prio ask") {
		t.Errorf("%d %q %q", code, out, e)
	}
	if _, _, e := run(t, "", "send", "--to", "lap/claude.live", "--prio", "fyi", "lunch at noon"); strings.Contains(e, "warning") {
		t.Errorf("no question, no warning: %q", e)
	}
}

func TestAskStillPicksItsOwnSessionWithAWiderWindow(t *testing.T) {
	deadSession(t)
	session(t, "desk/claude.me", "")
	// ask --within 6h picks the session seen 3h ago itself; send's 30m limit must not refuse that.
	code, _, e := run(t, "", "ask", "lap/claude.dead", "are you there?", "--within", "6h", "--wait-reply", "6s", "--claim-within", "5s")
	if strings.Contains(e, "refused: lap/claude.dead was last seen") {
		t.Errorf("ask is its own check: %d %q", code, e)
	}
}
