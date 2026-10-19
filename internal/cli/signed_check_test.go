package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// mesh --signed-check says READY only when no node holds a message without a valid signature from
// the window, and every node answered; otherwise it names the senders.
func TestSignedCheckNamesUnsignedSenders(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	node := func(name string, unsigned bool) string {
		s, _ := store.Open(t.TempDir(), name)
		m := msg.Message{ID: msg.NewID(), From: "air/claude.a", To: "all", Body: "signed"}
		m.Sign(priv, nil)
		s.Send(m)
		if unsigned {
			s.Send(msg.Message{From: "imac/codex.b", To: "all", Body: "from an old client"})
		}
		srv := httptest.NewServer(server.NewHandler(s, "tok", server.WithNode(name)))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_AGENT", "")

	clean := node("n1", false)
	t.Setenv("AIVOICEBUS_URL", clean+","+node("n2", false))
	if code, out, e := run(t, "", "mesh", "--signed-check"); code != 0 || !strings.Contains(out, "READY: no message without a valid signature in the last 7d on any of 2 node(s)") {
		t.Fatalf("all signed: want READY, exit 0; got %d\n%s\n%s", code, out, e)
	}

	t.Setenv("AIVOICEBUS_URL", clean+","+node("n3", true))
	code, out, _ := run(t, "", "mesh", "--signed-check")
	if code != 1 || !strings.Contains(out, "NOT READY: 1 sender(s)") || !strings.Contains(out, "imac/codex.b") || strings.Contains(out, "air/claude") {
		t.Fatalf("one unsigned sender on one node: want NOT READY naming imac/codex.b, exit 1; got %d\n%s", code, out)
	}
	code, out, _ = run(t, "", "mesh", "--signed-check", "--json")
	var v signedView
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 1 || v.Ready || len(v.Offenders) != 1 || v.Offenders[0].Unsigned != 1 {
		t.Fatalf("--json: %d %v\n%s", code, err, out)
	}

	// A node too old to answer, or one that is down, is no proof: not READY.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(old.Close)
	t.Setenv("AIVOICEBUS_URL", clean+","+old.URL)
	if code, out, _ := run(t, "", "mesh", "--signed-check"); code != 1 || !strings.Contains(out, "unsigned-report") || !strings.Contains(out, "NOT READY: 1 of 2 node(s) did not answer") {
		t.Fatalf("an old node: %d\n%s", code, out)
	}
	if code, _, _ := run(t, "", "mesh", "--signed-check", "--days", "31"); code != 2 {
		t.Errorf("--days 31: exit %d", code)
	}
}
