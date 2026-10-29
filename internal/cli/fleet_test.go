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
// `fleet update` lists the machines that are behind (or unknown). It sends nothing, --apply
// included: each machine's own aivoicebus tells its person (a task per machine and release, as
// 0.11.4 to 0.13.x sent, went unread).
func TestFleetShowsReleasesAndUpdateSendsNothing(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, thShared, server.WithNode("n1"), server.WithVersion("0.11.3"), server.WithTokenSet(tokenTokens(t))))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", thShared)
	t.Setenv("AIVOICEBUS_ME", "deploy/claude")
	t.Setenv("AIVOICEBUS_MACHINE_TOKENS_DIR", "")
	t.Cleanup(func() { client.SetRelease("") })

	client.SetRelease("0.9.1")
	hear(t, srv.URL, thShared, "imac/codex") // behind
	client.SetRelease("0.11.3")
	hear(t, srv.URL, thAir, "air/claude") // current
	client.SetRelease("")
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
	if !strings.Contains(out, "the nodes run 0.11.3") || !strings.Contains(out, "may be behind: each one's own aivoicebus tells its person") {
		t.Errorf("summary:\n%s", out)
	}
	if !strings.Contains(out, "what their sessions can do") || !strings.Contains(out, "  imac: tool:codex\n") {
		t.Errorf("what each machine can do:\n%s", out)
	}

	// the list names imac and old, not air, and sends nothing
	code, out, _ = run(t, "", "fleet", "update")
	if code != 0 || !strings.Contains(out, "imac: runs 0.9.1") || !strings.Contains(out, "old: runs unknown") || strings.Contains(out, "air:") || !strings.Contains(out, "nothing to send") {
		t.Fatalf("list: %d\n%s", code, out)
	}
	// --apply (an old script, a note from before 0.14.0) still sends nothing, and says why
	code, out, e = run(t, "", "fleet", "update", "--apply")
	if code != 0 || !strings.Contains(out, "--apply sends nothing any more") || !strings.Contains(out, "imac: runs 0.9.1") {
		t.Fatalf("apply: %d\n%s\n%s", code, out, e)
	}
	if code, out, _ = run(t, "", "fleet", "update", "--only-behind"); code != 0 || strings.Contains(out, "old:") || !strings.Contains(out, "imac: runs 0.9.1") {
		t.Fatalf("--only-behind: %d\n%s", code, out)
	}
	for _, addr := range []string{"imac/human", "imac/codex", "old/human", "old/claude", "air/human"} {
		if got := s.Inbox(addr, false); len(got) != 0 {
			t.Errorf("sent to %s: %+v", addr, got)
		}
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
