package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// nodes starts n independent nodes (no replication between them) and points the
// CLI at all of them.
func nodes(t *testing.T, n int, tweak func(*store.Store)) (stores []*store.Store) {
	t.Helper()
	var urls []string
	for i := 0; i < n; i++ {
		s, _ := store.Open(t.TempDir(), fmt.Sprintf("n%d", i))
		if tweak != nil {
			tweak(s)
		}
		srv := httptest.NewServer(server.NewHandler(s, "tok"))
		t.Cleanup(srv.Close)
		urls = append(urls, srv.URL)
		stores = append(stores, s)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", strings.Join(urls, ","))
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Chdir(t.TempDir())
	return stores
}

func TestWhoExpandsHereAndGroupsTheWaySendDoes(t *testing.T) {
	nodes(t, 1, nil)
	session(t, "mac/claude.1", "machine=mac,repo=alpha")
	run(t, "", "inbox")
	session(t, "air/codex.2", "machine=air,repo=beta")
	run(t, "", "inbox")
	session(t, "mac/claude.9", "")
	home := os.Getenv("HOME")
	os.MkdirAll(home+"/.config/aivoicebus", 0o755)
	os.WriteFile(home+"/.config/aivoicebus/groups", []byte("both=@machine=mac|air\n"), 0o644)

	_, out, _ := run(t, "", "who", "@here")
	if !strings.Contains(out, "mac/claude.1") || strings.Contains(out, "air/codex.2") {
		t.Errorf("@here is this machine: %q", out)
	}
	_, out, _ = run(t, "", "who", "@+both")
	if !strings.Contains(out, "mac/claude.1") || !strings.Contains(out, "air/codex.2") {
		t.Errorf("a saved group: %q", out)
	}
	if code, _, e := run(t, "", "who", "@+nobody"); code != 1 || !strings.Contains(e, `no group named "nobody"`) {
		t.Errorf("%d %q", code, e)
	}
}

func TestSendSaysWhoItReachesAndWhenItExpires(t *testing.T) {
	nodes(t, 1, nil)
	session(t, "mac/claude.aaaa", "repo=alpha,tool=claude")
	run(t, "", "inbox")
	session(t, "other/agent", "")

	code, out, e := run(t, "", "send", "--to", "@repo=alpha", "heads up")
	if code != 0 || !strings.HasPrefix(out, "sent ") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	for _, want := range []string{"reaches 1 session(s) seen in the last 24h", "expires in 3d"} {
		if !strings.Contains(e, want) {
			t.Errorf("a selector send should say %q: %q", want, e)
		}
	}
	if _, _, e = run(t, "", "send", "--to", "@repo=alpha", "--prio", "fyi", "news"); !strings.Contains(e, "expires in 1d") {
		t.Errorf("fyi lives a day: %q", e)
	}
	if _, _, e = run(t, "", "send", "--to", "@repo=alpha", "--ttl", "2h", "short"); !strings.Contains(e, "reaches 1") || strings.Contains(e, "expires in") {
		t.Errorf("an explicit ttl is not second-guessed: %q", e)
	}
	if _, _, e = run(t, "", "send", "--to", "mac/claude.aaaa", "direct"); !strings.Contains(e, "reaches 1") || strings.Contains(e, "expires in") {
		t.Errorf("a message to one session has no default expiry: %q", e)
	}
}

func TestSendRefusesASelectorThatMatchesNobodyUnlessQueued(t *testing.T) {
	nodes(t, 1, nil)
	session(t, "mac/claude.aaaa", "repo=alpha")
	run(t, "", "inbox")
	session(t, "other/agent", "")

	code, out, e := run(t, "", "send", "--to", "@repo=alhpa", "typo")
	if code != 1 || out != "" || !strings.Contains(e, "no session seen in the last 24h matches @repo=alhpa") || !strings.Contains(e, "--queue") {
		t.Fatalf("a selector nobody matches is refused with the way out: %d %q %q", code, out, e)
	}
	session(t, "mac/claude.aaaa", "repo=alpha")
	if _, out, _ := run(t, "", "inbox", "--all"); strings.Contains(out, "typo") {
		t.Error("the refused message was sent anyway")
	}

	session(t, "other/agent", "")
	code, out, e = run(t, "", "send", "--to", "@repo=alhpa", "--queue", "on purpose")
	if code != 0 || !strings.HasPrefix(out, "sent ") || !strings.Contains(e, "no matching session seen in the last 24h; it waits for one to start") {
		t.Errorf("--queue sends it and says it will wait: %d %q %q", code, out, e)
	}
	if code, _, e = run(t, "", "send", "--to", "nobody/claude.zzzz", "to a name nobody has"); code == 0 || !strings.Contains(e, "refused: nobody/claude.zzzz has not been seen by the nodes") {
		t.Errorf("a named recipient the nodes never heard of is refused, which also catches a typo: %d %q", code, e)
	}
	if code, _, e = run(t, "", "send", "--to", "nobody/claude.zzzz", "--queue", "to a name nobody has"); code != 0 || !strings.Contains(e, "no matching session seen") {
		t.Errorf("--queue sends it, and the typo is pointed out: %d %q", code, e)
	}
}

func TestSendToANodeWithoutARosterClaimsNothingAboutReach(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	inner := server.NewHandler(s, "tok")
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/roster" { // a node from before 0.4.0
			http.NotFound(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer old.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", old.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	session(t, "other/agent", "")
	code, out, e := run(t, "", "send", "--to", "@repo=alpha", "hello")
	if code != 0 || !strings.HasPrefix(out, "sent ") || strings.Contains(e, "reaches") || strings.Contains(e, "no matching") {
		t.Fatalf("when the nodes cannot say, send must not guess: %d %q %q", code, out, e)
	}
}

func TestWhoMergesWhatEveryNodeKnows(t *testing.T) {
	var all []string
	for i := 0; i < 2; i++ {
		s, _ := store.Open(t.TempDir(), fmt.Sprintf("n%d", i))
		srv := httptest.NewServer(server.NewHandler(s, "tok"))
		t.Cleanup(srv.Close)
		all = append(all, srv.URL)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Chdir(t.TempDir())

	// two sessions that read from different nodes
	t.Setenv("AIVOICEBUS_URL", all[0])
	session(t, "mac/claude.aaaa", "repo=alpha")
	run(t, "", "inbox")
	t.Setenv("AIVOICEBUS_URL", all[1])
	session(t, "air/codex.bbbb", "repo=beta")
	run(t, "", "inbox")

	t.Setenv("AIVOICEBUS_URL", all[0]+","+all[1])
	_, out, _ := run(t, "", "who")
	if !strings.Contains(out, "mac/claude.aaaa") || !strings.Contains(out, "air/codex.bbbb") {
		t.Errorf("who should show both sessions whichever node knows them:\n%s", out)
	}
	_, out, _ = run(t, "", "who", "@repo=beta")
	if !strings.Contains(out, "air/codex.bbbb") || strings.Contains(out, "mac/claude.aaaa") || !strings.Contains(out, "repo=beta") {
		t.Errorf("a selector preview should see labels declared to either node:\n%s", out)
	}
}

func TestSendingTooMuchIsRefusedWithTheReason(t *testing.T) {
	nodes(t, 2, func(s *store.Store) { s.SetSendLimit(2) })
	session(t, "loop/agent", "")
	for i := 0; i < 2; i++ {
		if code, _, e := run(t, "", "send", "--to", "imac/claude.zzzz", "--queue", fmt.Sprintf("msg %d", i)); code != 0 {
			t.Fatalf("send %d: %d %q", i, code, e)
		}
	}
	code, out, e := run(t, "", "send", "--to", "imac/claude.zzzz", "--queue", "msg 3")
	if code != 1 || out != "" || !strings.Contains(e, "429") || !strings.Contains(e, "rate limit") || !strings.Contains(e, "ask the user") {
		t.Fatalf("a flood is stopped and the agent is told what to do: %d %q %q", code, out, e)
	}
	if strings.Contains(e, "warning: only") {
		t.Errorf("a refusal must not be reported as a partial write: %q", e)
	}
}

func TestInboxAckAndAckLeaveReceiptsSayingHowTheMessageWasRead(t *testing.T) {
	stores := nodes(t, 1, nil)
	session(t, "other/agent", "")
	run(t, "", "send", "--to", "mac/claude.aaaa", "--queue", "first, read through the inbox")
	run(t, "", "send", "--to", "mac/claude.aaaa", "--queue", "second, acknowledged by hand")
	var ids []string
	for _, r := range stores[0].Since(map[string]uint64{}, 100) {
		if r.Msg != nil {
			ids = append(ids, r.Msg.ID)
		}
	}
	session(t, "mac/claude.aaaa", "")
	if code, out, e := run(t, "", "inbox", "--ack"); code != 0 || !strings.Contains(out, "first") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	// both were shown and acked by `inbox --ack`; acknowledge one again by hand
	if code, _, e := run(t, "", "ack", ids[1]); code != 0 {
		t.Fatalf("ack: %d %s", code, e)
	}
	var viaInbox, viaCLI int
	for _, r := range stores[0].Since(map[string]uint64{}, 100) {
		if r.Ack == nil {
			continue
		}
		switch r.Ack.Via {
		case "inbox":
			viaInbox++
			if r.Ack.Chars == 0 {
				t.Errorf("an inbox receipt carries the characters shown: %+v", r.Ack)
			}
		case "cli":
			viaCLI++
		}
	}
	if viaInbox != 2 || viaCLI != 1 {
		t.Errorf("receipts: %d via inbox, %d via cli; want 2 and 1", viaInbox, viaCLI)
	}
}

func TestSendRefusesACredentialBeforeAnythingLeavesAndWarnsAboutWeakShapes(t *testing.T) {
	stores := nodes(t, 1, nil)
	session(t, "other/agent", "")
	code, out, e := run(t, "", "send", "--to", "mac/claude.aaaa", "deploy with ghp_"+"abcdefghijklmnopqrstuvwxyz0123456789")
	if code != 1 || out != "" || !strings.Contains(e, "refused: this message looks like it contains a GitHub token") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	if n := len(stores[0].Since(map[string]uint64{}, 100)); n != 0 {
		t.Errorf("nothing was sent: %d records", n)
	}
	code, out, e = run(t, "", "send", "--to", "mac/claude.aaaa", "--queue", "AIVOICEBUS_TOKEN=Zm9vYmFyYmF6cXV4MTIzNDU2 is set")
	if code != 0 || !strings.HasPrefix(out, "sent ") || !strings.Contains(e, "warning: this message contains something that looks like a password or token value") {
		t.Errorf("a weak shape warns and still sends: %d %q %q", code, out, e)
	}
}
