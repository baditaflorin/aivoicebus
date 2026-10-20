package cli

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/agentcfg"
	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// hear makes the nodes at url hear from me with token: a send and an inbox read.
func hear(t *testing.T, url, token, me string) {
	t.Helper()
	c := &client.Client{URLs: []string{url}, Token: token}
	if _, err := c.Send(msg.Message{ID: msg.NewID(), From: me, To: "nobody/x", Body: "hello from " + me}); err != nil {
		t.Fatalf("send as %s: %v", me, err)
	}
	if _, err := c.Inbox(me, false); err != nil {
		t.Fatalf("inbox as %s: %v", me, err)
	}
}

// The report names the machines on the shared token, from what the nodes saw, and refuses a
// rotation while one that is neither a node nor the deploy machine is still on it.
func TestTokenReportNamesSharedAndMachineTokenUsers(t *testing.T) {
	a, b := twoNodes(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_URL", a+","+b)
	t.Setenv("AIVOICEBUS_TOKEN", thShared)
	t.Setenv("AIVOICEBUS_ME", "deploy/claude")
	t.Setenv("AIVOICEBUS_MACHINE_TOKENS_DIR", "")
	writeSecret(t, filepath.Join(home, ".config/aivoicebus/machine-tokens/air"), thAir)

	hear(t, a, thAir, "air/claude")       // its own token, on node a
	hear(t, b, thShared, "imac/codex")    // the shared token, on node b only
	hear(t, a, thShared, "deploy/claude") // the deploy machine keeps the shared token
	hear(t, b, thShared, "a/ops")         // a node's own machine (named like the node)

	code, out, errOut := run(t, "", "token", "report", "--check")
	if code != 1 {
		t.Fatalf("imac is on the shared token: --check must exit 1, got %d\n%s\n%s", code, out, errOut)
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
	if l := line("imac"); !strings.Contains(l, "client") || !strings.Contains(l, "shared") || !strings.Contains(l, "deploy/machine-token.sh add imac") {
		t.Errorf("imac: %q", l)
	}
	if l := line("air"); !strings.Contains(l, "machine") || !strings.Contains(l, "made") || strings.Contains(l, "<-") {
		t.Errorf("air: %q", l)
	}
	if l := line("deploy"); !strings.Contains(l, "deploy") || strings.Contains(l, "<-") {
		t.Errorf("the deploy machine does not block: %q", l)
	}
	if l := line("a"); !strings.Contains(l, " node ") || strings.Contains(l, "<-") {
		t.Errorf("a node does not block: %q", l)
	}
	if !strings.Contains(out, "machine tokens made here") || !strings.Contains(out, "rotate the shared token now? NO: rotating the shared token now would cut off imac") {
		t.Errorf("verdict:\n%s", out)
	}

	// imac gets its own token: now the rotation is safe, and --json says so.
	hear(t, a, thImac, "imac/codex")
	code, out, _ = run(t, "", "token", "report", "--check", "--json")
	var v tokenReportView
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 0 || !v.RotateSafe {
		t.Fatalf("every client on a machine token: want exit 0 and rotate_safe, got %d %v\n%s", code, err, out)
	}
	for _, m := range v.Machines {
		if m.Host == "imac" && (m.Kind != "machine" || m.SeenBy != "a") {
			t.Errorf("the newest answer wins: %+v", m)
		}
	}
}

// A machine the roster knows but no node has a record of (a node upgraded from a release that kept
// the record in memory only, or whose token-seen.json was lost) is not proven: it blocks. So does
// a node that does not answer.
func TestTokenReportBlocksOnWhatItCannotProve(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	before := httptest.NewServer(server.NewHandler(s, thShared, server.WithNode("n1"), server.WithTokenSet(tokenTokens(t))))
	hear(t, before.URL, thShared, "old/claude")
	before.Close()
	os.Remove(filepath.Join(s.Dir(), "token-seen.json")) // the record is gone; the roster still knows the machine
	after := httptest.NewServer(server.NewHandler(s, thShared, server.WithNode("n1"), server.WithTokenSet(tokenTokens(t))))
	t.Cleanup(after.Close)

	c := &client.Client{URLs: []string{after.URL}, Token: thShared}
	v, err := buildTokenReport(c, time.Now(), 7, "deploy", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if v.RotateSafe || len(v.Machines) != 1 || v.Machines[0].Kind != "" || !strings.Contains(v.Machines[0].Blocks, "not proven") {
		t.Fatalf("unknown token kind must block: %+v", v)
	}

	hear(t, after.URL, thAir, "air/claude")
	c.URLs = []string{after.URL, "http://127.0.0.1:1"} // old is the deploy machine here: only the node that is down blocks
	v, err = buildTokenReport(c, time.Now(), 7, "old", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if v.RotateSafe || !strings.Contains(v.Verdict, "not every node answered") {
		t.Fatalf("a node that does not answer must block: %+v", v)
	}

	// A machine last heard from before the window is not listed.
	v, _ = buildTokenReport(&client.Client{URLs: []string{after.URL}, Token: thShared}, time.Now().Add(8*24*time.Hour), 7, "deploy", nil, t.TempDir())
	if len(v.Machines) != 0 || !v.RotateSafe {
		t.Fatalf("nothing heard in the window: %+v", v)
	}
}

// The machine you deploy from keeps the shared token, and holds machine-tokens/: there the
// token-kind line is OK, and nowhere does it tell anyone to give the deploy machine its own.
func TestTokenKindIsRightOnTheDeployMachine(t *testing.T) {
	mesh(t) // the shared token "tok"
	c, ok := tokenKindCheck()
	if !ok || c.Level != agentcfg.LevelWarn || !strings.Contains(c.Detail, "Unless this is the machine you deploy from") {
		t.Fatalf("a client on the shared token is warned, with the deploy exception named: %+v", c)
	}
	dir := filepath.Join(config.Dir(), "machine-tokens")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "laptop-b"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, ok = tokenKindCheck()
	if !ok || c.Level != agentcfg.LevelOK || !strings.Contains(c.Detail, "this machine deploys") || strings.Contains(c.Detail, "machine-token.sh add") {
		t.Fatalf("the deploy machine's shared token is right: %+v", c)
	}
}
