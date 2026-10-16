package cli

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestSendPrioAndTTLFlagsEndToEnd(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	session(t, "other/agent", "")
	for _, args := range [][]string{
		{"send", "--to", "air/codex", "--prio", "urgent", "build is broken"},
		{"send", "--to", "air/codex", "--prio", "FYI", "--ttl", "2h", "lunch at noon"},
		{"send", "--to", "air/codex", "--ttl", "1d", "plain with a ttl"},
	} {
		if code, out, e := run(t, "", args...); code != 0 {
			t.Fatalf("%v: %d %s %s", args, code, out, e)
		}
	}
	session(t, "air/codex", "")
	_, out, _ := run(t, "", "inbox")
	for _, want := range []string{"build is broken", "[urgent]", "lunch at noon", "[fyi]", "plain with a ttl"} {
		if !strings.Contains(out, want) {
			t.Errorf("inbox missing %q:\n%s", want, out)
		}
	}
}

func TestSendRejectsBadPrioAndTTLBeforeSending(t *testing.T) {
	mesh(t)
	session(t, "a/b", "")
	for args, want := range map[string]string{
		"send --to x --prio loud hi": "--prio:",
		"send --to x --ttl soon hi":  "--ttl:",
		"send --to x --ttl 10s hi":   "--ttl:",
		"send --to x --ttl 31d hi":   "--ttl:",
		"send --to x --ttl 1.5d hi":  "--ttl:",
	} {
		if code, _, e := run(t, "", strings.Fields(args)...); code != exitUsage || !strings.Contains(e, want) {
			t.Errorf("%q: %d %q, want an error containing %q", args, code, e, want)
		}
	}
	session(t, "x", "")
	if _, out, _ := run(t, "", "inbox"); !strings.Contains(out, "(no unread messages)") {
		t.Errorf("a rejected send leaked a message:\n%s", out)
	}
}

func TestExpiredMessagesAreHiddenFromInboxAndMarkedInAll(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	var nanos atomic.Int64
	nanos.Store(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	s.SetClock(func() time.Time { return time.Unix(0, nanos.Load()).UTC() })
	srv := httptest.NewServer(server.NewHandler(s, "tok"))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Chdir(t.TempDir())

	session(t, "other/agent", "")
	if code, _, e := run(t, "", "send", "--to", "a/b", "--ttl", "1h", "stale news"); code != 0 {
		t.Fatal(e)
	}
	session(t, "a/b", "")
	if _, out, _ := run(t, "", "inbox"); !strings.Contains(out, "stale news") {
		t.Fatalf("before expiry the message is delivered:\n%s", out)
	}
	nanos.Add(int64(2 * time.Hour)) // two hours later on the node's clock; the same session reads again
	if _, out, _ := run(t, "", "inbox"); !strings.Contains(out, "(no unread messages)") {
		t.Errorf("an expired message must not be delivered, even though it was never acked:\n%s", out)
	}
	if _, out, _ := run(t, "", "inbox", "--all"); !strings.Contains(out, "stale news") || !strings.Contains(out, "[expired]") {
		t.Errorf("--all lists it, marked:\n%s", out)
	}
}

func TestSendDryRunSaysWhatWouldHappenAndStoresNothing(t *testing.T) {
	stores := nodes(t, 1, nil)
	session(t, "mac/claude.aaaa", "repo=alpha,tool=claude")
	run(t, "", "inbox")
	session(t, "other/agent", "")

	code, out, e := run(t, "", "send", "--dry-run", "--to", "@repo=alpha", "--kind", "question", "who owns the flaky test?")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, e)
	}
	for _, want := range []string{"dry run: nothing was sent", "priority  ask, question", "reaches   1 session(s)", "expires   in 3d"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if stores[0].Stats().Messages != 0 {
		t.Error("a dry run stores nothing")
	}
	if code, _, e := run(t, "", "send", "--dry-run", "--to", "@repo=nobody", "x"); code != 1 || !strings.Contains(e, "no session seen") {
		t.Errorf("a dry run fails the same way the real send would: %d %q", code, e)
	}
}

func TestSendForceAndKindAreWiredThrough(t *testing.T) {
	stores := nodes(t, 1, nil)
	for i := 0; i < 5; i++ {
		session(t, fmt.Sprintf("host%d/claude.000%d", i, i), "repo=alpha")
		run(t, "", "inbox")
	}
	session(t, "other/agent", "")
	if code, _, e := run(t, "", "send", "--to", "@repo=alpha", "--prio", "urgent", "stop"); code != 1 || !strings.Contains(e, "would interrupt 5 sessions") {
		t.Fatalf("%d %q", code, e)
	}
	if code, out, e := run(t, "", "send", "--to", "@repo=alpha", "--prio", "urgent", "--force", "stop"); code != 0 {
		t.Fatalf("%d %q %q", code, out, e)
	}
	run(t, "", "send", "--to", "@repo=alpha", "--kind", "question", "who?")
	got := stores[0].InboxFor("host0/claude.0000", false, msg.Labels{"repo": "alpha"})
	kinds := map[string]bool{}
	for _, m := range got {
		kinds[m.Kind+"/"+m.Priority()] = true
		if m.Reach == nil || *m.Reach != 5 {
			t.Errorf("the sender's count of readers is stored with the message: %+v", m.Reach)
		}
	}
	if !kinds["/urgent"] || !kinds["question/ask"] {
		t.Errorf("%v", kinds)
	}
}

func TestQuietCommandShowsSetsAndClearsTheWindow(t *testing.T) {
	mesh(t)
	if _, out, _ := run(t, "", "quiet"); !strings.Contains(out, "quiet hours: off") {
		t.Errorf("%q", out)
	}
	if code, out, _ := run(t, "", "quiet", "22:00-07:00"); code != 0 || !strings.Contains(out, "22:00-07:00") || !strings.Contains(out, "urgent messages still arrive") {
		t.Errorf("%d %q", code, out)
	}
	if _, out, _ := run(t, "", "quiet"); !strings.Contains(out, "22:00-07:00") {
		t.Errorf("it is remembered: %q", out)
	}
	if code, _, e := run(t, "", "quiet", "tonight"); code != exitUsage || !strings.Contains(e, "not a window") {
		t.Errorf("%d %q", code, e)
	}
	if _, out, _ := run(t, "", "quiet", "off"); !strings.Contains(out, "off") {
		t.Errorf("%q", out)
	}
	if _, out, _ := run(t, "", "quiet"); !strings.Contains(out, "quiet hours: off") {
		t.Errorf("%q", out)
	}
}
