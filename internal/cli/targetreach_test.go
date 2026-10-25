package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/store"
)

// Sends whose target reaches nobody (review of 2026-10-25, improvements 5 to 7): a selector none of whose
// sessions is live, a session named without its machine, a session that has read nothing for hours.

// graphMesh is the go-fleet-graph week in small: repo graph's two sessions last read 5h and 3h ago
// (air/codex.old, lap/claude.f324), lap/claude.near is live in repo gateway, and the CLI acts as
// desk/claude.me.
func graphMesh(t *testing.T) *store.Store {
	t.Helper()
	s := meshStore(t)
	s.SetClock(func() time.Time { return time.Now().Add(-5 * time.Hour) })
	session(t, "air/codex.old", "repo=graph")
	run(t, "", "inbox")
	s.SetClock(func() time.Time { return time.Now().Add(-3 * time.Hour) })
	session(t, "lap/claude.f324", "repo=graph")
	run(t, "", "inbox")
	s.SetClock(time.Now)
	session(t, "lap/claude.near", "repo=gateway")
	run(t, "", "inbox")
	session(t, "desk/claude.me", "")
	return s
}

func TestASelectorNoLiveSessionMatchesIsRefusedLikeAStaleSession(t *testing.T) {
	s := graphMesh(t)
	code, out, e := run(t, "", "send", "--to", "@repo=graph", "hold the deploy: a checkpoint is running")
	if code == 0 || out != "" {
		t.Fatalf("it was sent: %d %q", code, out)
	}
	for _, want := range []string{"refused: none of the sessions @repo=graph matches is live (seen in the last 30m, or standing by)",
		"lap/claude.f324 (seen 3h ago)", "air/codex.old (seen 5h ago)", "live on lap now: lap/claude.near (repo gateway,", `ask lap "..."`, "--queue (or --force)"} {
		if !strings.Contains(e, want) {
			t.Errorf("stderr is missing %q:\n%s", want, e)
		}
	}
	if n := s.Stats().Messages; n != 0 {
		t.Errorf("a refused send stored %d message(s)", n)
	}
	if code, _, e := run(t, "", "send", "--to", "@repo=graph", "--dry-run", "hold"); code == 0 || !strings.Contains(e, "refused: none of the sessions @repo=graph") {
		t.Errorf("a dry run shows the refusal too: %d %q", code, e)
	}
	session(t, "lap/claude.near", "repo=gateway")
	_, out, _ = run(t, "", "send", "--to", "desk/claude.me", "--queue", "is graph deploying?")
	asked, _ := sentID(t, out)
	session(t, "desk/claude.me", "")
	for _, args := range [][]string{{"task", "@repo=graph", "hold the deploy"}, {"reply", "--to", "@repo=graph", "--re", asked, "hold"}} {
		if code, _, e := run(t, "", args...); code == 0 || !strings.Contains(e, "refused: none of the sessions @repo=graph") {
			t.Errorf("%v: %d %q", args, code, e)
		}
	}
	for _, flag := range []string{"--queue", "--force"} {
		if code, out, e := run(t, "", "send", "--to", "@repo=graph", flag, "hold the deploy"); code != 0 || !strings.HasPrefix(out, "sent ") {
			t.Errorf("%s: %d %q %q", flag, code, out, e)
		}
	}
	// Someone live in the repo: sent as before.
	if code, out, e := run(t, "", "send", "--to", "@repo=gateway", "hold the deploy"); code != 0 {
		t.Errorf("a live session matches: %d %q %q", code, out, e)
	}
	// The moment one of them reads, the selector reaches someone again.
	session(t, "lap/claude.f324", "repo=graph")
	run(t, "", "inbox")
	session(t, "desk/claude.me", "")
	if code, out, e := run(t, "", "send", "--to", "@repo=graph", "hold the deploy"); code != 0 {
		t.Errorf("after a read: %d %q %q", code, out, e)
	}
	// Nobody in 24h keeps its older refusal.
	if code, _, e := run(t, "", "send", "--to", "@repo=nothing", "hold"); code == 0 || !strings.Contains(e, "no session seen in the last 24h matches @repo=nothing") {
		t.Errorf("nobody at all: %d %q", code, e)
	}
}

func TestASessionNamedWithoutItsMachineGoesToTheOneLiveSessionOfThatName(t *testing.T) {
	s := graphMesh(t)
	code, out, e := run(t, "", "send", "--to", "claude.near", "is the gateway up?")
	if code != 0 || !strings.HasPrefix(out, "sent ") || !strings.Contains(e, "claude.near -> lap/claude.near: the one live session of that name") {
		t.Fatalf("send: %d %q %q", code, out, e)
	}
	if id, _ := sentID(t, out); held(t, s, id).To != "lap/claude.near" {
		t.Errorf("held to %q", held(t, s, id).To)
	}
	_, out, e = run(t, "", "task", "claude.near", "check the gateway")
	if id, _ := sentID(t, out); held(t, s, id).To != "lap/claude.near" {
		t.Errorf("task held to %q (%s)", held(t, s, id).To, e)
	}
	if _, out, _ := run(t, "", "send", "--to", "claude.near", "--dry-run", "x"); !strings.Contains(out, "to        lap/claude.near") || !strings.Contains(out, "resolved  claude.near -> lap/claude.near") {
		t.Errorf("dry run:\n%s", out)
	}
	// Not live: refused with the session and the way to keep it for it; --queue does.
	code, _, e = run(t, "", "send", "--to", "claude.f324", "x")
	if code == 0 || !strings.Contains(e, "refused: claude.f324 names a session without its machine") || !strings.Contains(e, "lap/claude.f324") || !strings.Contains(e, "--queue") {
		t.Errorf("not live: %d %q", code, e)
	}
	code, out, e = run(t, "", "send", "--to", "claude.f324", "--queue", "x")
	if id, _ := sentID(t, out); code != 0 || held(t, s, id).To != "lap/claude.f324" {
		t.Errorf("--queue: %d %q %q", code, out, e)
	}
	// Nobody of that name: refused, never sent to a machine called claude.zz00.
	if code, _, e := run(t, "", "send", "--to", "claude.zz00", "--queue", "x"); code == 0 || !strings.Contains(e, "no session of that name has been seen") {
		t.Errorf("nobody: %d %q", code, e)
	}
	// Two live of that name: a guess, refused with both.
	session(t, "air/claude.near", "")
	run(t, "", "inbox")
	session(t, "desk/claude.me", "")
	if code, _, e := run(t, "", "send", "--to", "claude.near", "x"); code == 0 || !strings.Contains(e, "2 live sessions have that name") || !strings.Contains(e, "air/claude.near") {
		t.Errorf("two live: %d %q", code, e)
	}
}

func TestAskResolvesASessionNamedWithoutItsMachine(t *testing.T) {
	graphMesh(t)
	_, _, e := run(t, "", "ask", "claude.near", "is the gateway up?", "--wait-reply", "6s", "--claim-within", "5s")
	if !strings.Contains(e, "ask: claude.near -> lap/claude.near: the one live session of that name") || !strings.Contains(e, "ask: lap/claude.near:") {
		t.Errorf("ask: %q", e)
	}
	if code, _, e := run(t, "", "ask", "claude.zz00", "anyone?"); code != 2 || !strings.Contains(e, "no session of that name has been seen") {
		t.Errorf("ask nobody: %d %q", code, e)
	}
}

// busySession is lap/claude.f324 of graphMesh having sent a progress note just now: the nodes saw it a
// moment ago (so the 30m check passes it), but it last read 3h ago.
func busySession(t *testing.T) (*store.Store, string) {
	t.Helper()
	s := graphMesh(t)
	session(t, "lap/claude.f324", "repo=graph")
	code, out, e := run(t, "", "send", "--to", "desk/claude.me", "--queue", "step 4 of 9 done")
	if code != 0 {
		t.Fatalf("setup: %d %q", code, e)
	}
	id, _ := sentID(t, out)
	session(t, "desk/claude.me", "")
	return s, id
}

func TestSendWarnsWhenTheSessionNamedHasReadNothingForHours(t *testing.T) {
	_, progress := busySession(t)
	code, out, e := run(t, "", "send", "--to", "lap/claude.f324", "pause before the deploy")
	if code != 0 || !strings.HasPrefix(out, "sent ") {
		t.Fatalf("it still sends: %d %q %q", code, out, e)
	}
	for _, want := range []string{"warning: lap/claude.f324 has read nothing for 3h (last read ", "so it may not see this until its next prompt",
		"nobody else in repo graph is live, and no owner is recorded for it (`aivoicebus owner set repo:graph` records one)"} {
		if !strings.Contains(e, want) {
			t.Errorf("stderr is missing %q:\n%s", want, e)
		}
	}
	for _, args := range [][]string{{"task", "lap/claude.f324", "pause"}, {"ask", "lap/claude.f324", "paused?", "--wait-reply", "6s", "--claim-within", "5s"}} {
		if _, _, e := run(t, "", args...); !strings.Contains(e, "lap/claude.f324 has read nothing for 3h") {
			t.Errorf("%s: %q", args[0], e)
		}
	}
	// A reply's target, inferred from what it answers, is warned about too.
	if _, _, e := run(t, "", "reply", "--re", progress, "thanks, pause there"); !strings.Contains(e, "lap/claude.f324 has read nothing for 3h") {
		t.Errorf("reply: %q", e)
	}
	for _, limit := range []string{"0", "4h"} {
		if code, _, e := run(t, "", "send", "--to", "lap/claude.f324", "--read-within", limit, "x"); code != 0 || strings.Contains(e, "read nothing") {
			t.Errorf("--read-within %s: %d %q", limit, code, e)
		}
	}
	if _, _, e := run(t, "", "ask", "lap/claude.f324", "paused?", "--read-within", "0", "--wait-reply", "6s", "--claim-within", "5s"); strings.Contains(e, "read nothing") || !strings.Contains(e, "ask: lap/claude.f324") {
		t.Errorf("ask --read-within 0: %q", e)
	}
	if code, _, e := run(t, "", "send", "--to", "lap/claude.f324", "--read-within", "-1h", "x"); code != 2 || !strings.Contains(e, "--read-within is a duration") {
		t.Errorf("negative: %d %q", code, e)
	}
	// What the tool sends on its own account is not warned about (pause, room, the worker's answers).
	if code, out, e := run(t, "", "pause", "--only", "lap/claude.f324", "--for", "1m", "hold on"); code != 0 || !strings.Contains(out, "paused") || strings.Contains(e, "read nothing") {
		t.Errorf("pause: %d %q %q", code, out, e)
	}
}

func TestTheReadWarningNamesWhoToTryInstead(t *testing.T) {
	busySession(t)
	// Someone else live in the repo: named.
	session(t, "imac/codex.peer", "repo=graph")
	run(t, "", "inbox")
	session(t, "desk/claude.me", "")
	if _, _, e := run(t, "", "send", "--to", "lap/claude.f324", "x"); !strings.Contains(e, "live in repo graph now: imac/codex.peer") {
		t.Errorf("live in repo: %q", e)
	}
}

func TestTheReadWarningPointsAtTheReposOwnerRecord(t *testing.T) {
	busySession(t)
	session(t, "lap/claude.near", "repo=gateway")
	if code, out, e := run(t, "", "owner", "set", "repo:graph"); code != 0 {
		t.Fatalf("owner set: %d %q %q", code, out, e)
	}
	session(t, "desk/claude.me", "")
	if _, _, e := run(t, "", "send", "--to", "lap/claude.f324", "x"); !strings.Contains(e, "nobody else in repo graph is live, and its owner is recorded: `aivoicebus task owner:repo:graph \"...\"`") {
		t.Errorf("owner: %q", e)
	}
}

func TestAFileForASessionThatReadsNothingIsWarnedAbout(t *testing.T) {
	busySession(t)
	services(t)
	src := filepath.Join(t.TempDir(), "deploy.log")
	if err := os.WriteFile(src, []byte("checkpoint at 61%\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file lives five minutes: a session that read nothing for 3h will most likely never open it.
	code, out, e := run(t, "", "send", "--to", "lap/claude.f324", "--file", src, "the log")
	if code != 0 || !strings.Contains(e, "warning: lap/claude.f324 has read nothing for 3h") {
		t.Errorf("%d %q %q", code, out, e)
	}
}
