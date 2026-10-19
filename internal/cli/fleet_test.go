package cli

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// fleet shows the release each machine's client said it runs, as the nodes recorded it, and
// `fleet update` asks the person on each machine that is behind (or unknown) to update: a task
// for the person, never sent without --apply.
func TestFleetShowsReleasesAndAsksThePersonToUpdate(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, thShared, server.WithNode("n1"), server.WithVersion("0.11.3"), server.WithTokenSet(tokenTokens(t))))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", thShared)
	t.Setenv("AIVOICEBUS_ME", "deploy/claude")
	t.Setenv("AIVOICEBUS_MACHINE_TOKENS_DIR", "")
	t.Cleanup(func() { client.Release = "" })

	client.Release = "0.9.1"
	hear(t, srv.URL, thShared, "imac/codex") // behind
	client.Release = "0.11.3"
	hear(t, srv.URL, thAir, "air/claude") // current
	client.Release = ""
	hear(t, srv.URL, thShared, "old/claude") // a client that does not say

	code, out, e := run(t, "", "fleet")
	if code != 0 {
		t.Fatalf("fleet: %d\n%s\n%s", code, out, e)
	}
	line := func(host string) string {
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) > 0 && f[0] == host {
				return l
			}
		}
		t.Fatalf("no line for %s:\n%s", host, out)
		return ""
	}
	if l := line("imac"); !strings.Contains(l, "0.9.1") || !strings.Contains(l, "behind 0.11.3") {
		t.Errorf("imac: %q", l)
	}
	if l := line("air"); !strings.Contains(l, "0.11.3") || strings.Contains(l, "<-") {
		t.Errorf("air is current: %q", l)
	}
	if l := line("old"); !strings.Contains(l, "unknown") {
		t.Errorf("old: %q", l)
	}
	if !strings.Contains(out, "the nodes run 0.11.3") || !strings.Contains(out, "may be behind: aivoicebus fleet update") {
		t.Errorf("summary:\n%s", out)
	}

	// the plan names imac and old, not air, and sends nothing
	code, out, _ = run(t, "", "fleet", "update")
	if code != 0 || !strings.Contains(out, "imac: runs 0.9.1") || !strings.Contains(out, "old: runs unknown") || strings.Contains(out, "air:") || !strings.Contains(out, "plan only") {
		t.Fatalf("plan: %d\n%s", code, out)
	}
	if got := s.Inbox("imac/human", false); len(got) != 0 {
		t.Fatalf("a plan sends nothing: %+v", got)
	}

	// --apply --only-behind: one task, for the person on imac
	code, out, e = run(t, "", "fleet", "update", "--apply", "--only-behind")
	if code != 0 || !strings.Contains(out, "sent 1 of 1") {
		t.Fatalf("apply: %d\n%s\n%s", code, out, e)
	}
	got := s.Inbox("imac/human", false)
	if len(got) != 1 || got[0].Kind != msg.KindTask || !strings.HasPrefix(got[0].Body, msg.PersonTag) || !strings.Contains(got[0].Body, "aivoicebus update") || !msg.PersonOnly(got[0]) {
		t.Fatalf("the person's task on imac: %+v", got)
	}
	if got := s.Inbox("old/human", false); len(got) != 0 {
		t.Fatalf("--only-behind leaves unknown machines out: %+v", got)
	}
}

// Only a release-shaped value is kept from the header: anything else a client sends is ignored.
func TestValidReleaseRejectsJunk(t *testing.T) {
	for _, ok := range []string{"0.11.3", "1.2.3-dev", "10.0.12-rc.1"} {
		if !msg.ValidRelease(ok) {
			t.Errorf("%q should be kept", ok)
		}
	}
	for _, bad := range []string{"", "dev", "0.11", "0.11.3 ", "0.11.3\nX: y", "<script>", strings.Repeat("1", 30) + ".0.0"} {
		if msg.ValidRelease(bad) {
			t.Errorf("%q should be ignored", bad)
		}
	}
}
