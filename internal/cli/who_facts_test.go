package cli

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// declare is a session's inbox read, which is how its labels reach the node.
func declare(t *testing.T, me string, l msg.Labels) {
	t.Helper()
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	c.Labels = l
	if _, err := c.Inbox(me, false); err != nil {
		t.Fatalf("inbox as %s: %v", me, err)
	}
}

// factsMesh: keymac holds the signing key (two sessions, the newer one's facts count), plain says
// it has nothing, old runs a client that advertises no facts.
func factsMesh(t *testing.T) {
	t.Helper()
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "asker/claude.q1")
	declare(t, "keymac/claude.k0", msg.Labels{msg.HasLabel: "none"})
	declare(t, "keymac/claude.k1", msg.Labels{msg.HasLabel: "signing-key+fleet-guard-0.3.13", msg.SSHLabel: "0mcp+0own"})
	declare(t, "plain/codex.p1", msg.Labels{msg.HasLabel: "none", "repo": "x"})
	declare(t, "old/claude.o1", msg.Labels{"repo": "x"})
}

func lineOf(t *testing.T, out, first string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == first {
			return l
		}
	}
	t.Fatalf("no line for %s:\n%s", first, out)
	return ""
}

// who --has answers per machine: yes, no, or unknown when its sessions said nothing, which is
// never taken for a no (the key hunt went wrong exactly there).
func TestWhoHasAnswersPerMachineAndNeverTakesSilenceForNo(t *testing.T) {
	factsMesh(t)
	code, out, e := run(t, "", "who", "--has", "signing-key")
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	if l := lineOf(t, out, "keymac"); !strings.Contains(l, "yes") {
		t.Errorf("keymac: %q", l)
	}
	if l := lineOf(t, out, "plain"); !strings.Contains(l, " no") {
		t.Errorf("plain: %q", l)
	}
	if l := lineOf(t, out, "old"); !strings.Contains(l, "unknown") {
		t.Errorf("old: %q", l)
	}
	if !strings.Contains(out, "\nsigning-key: keymac\n") || !strings.Contains(out, "unknown: old: no session there says what it has") {
		t.Errorf("summary:\n%s", out)
	}
	if strings.Index(out, "keymac") > strings.Index(out, "plain") || strings.Index(out, "plain") > strings.Index(out, "old ") {
		t.Errorf("yes first, then no, then unknown:\n%s", out)
	}
}

func TestWhoHasMatchesVersionsAndAliasesAndSaysWhichAnswered(t *testing.T) {
	factsMesh(t)
	if _, out, _ := run(t, "", "who", "--has", "ssh:0own_bastion"); !strings.Contains(lineOf(t, out, "keymac"), "yes (0own)") || !strings.Contains(lineOf(t, out, "plain"), "no") {
		t.Errorf("ssh:\n%s", out)
	}
	if _, out, _ := run(t, "", "who", "--has", "fleet-guard:0.3.*"); !strings.Contains(lineOf(t, out, "keymac"), "yes (0.3.13)") {
		t.Errorf("fleet-guard:\n%s", out)
	}
	if _, out, _ := run(t, "", "who", "--has", "fleet-guard:0.4.*"); !strings.Contains(lineOf(t, out, "keymac"), "no (0.3.13)") || !strings.Contains(out, "no machine heard from in the last 24h says it has fleet-guard:0.4.*") {
		t.Errorf("another version:\n%s", out)
	}
	code, out, _ := run(t, "", "who", "--has", "deploy-token", "--json")
	var got struct {
		Fact     string
		Machines []factAnswer
	}
	if err := json.Unmarshal([]byte(out), &got); code != 0 || err != nil || got.Fact != "deploy-token" || len(got.Machines) < 3 {
		t.Fatalf("%d %v %+v\n%s", code, err, got, out)
	}
	for _, m := range got.Machines {
		want := map[string]string{"keymac": "no", "plain": "no", "old": "unknown"}[m.Machine]
		if want != "" && m.Answer != want {
			t.Errorf("%s: %s, want %s", m.Machine, m.Answer, want)
		}
		if m.Machine == "keymac" && m.From != "keymac/claude.k1" {
			t.Errorf("keymac's answer comes from its newest facts: %+v", m)
		}
	}
	// a selector narrows the machines asked about
	if _, out, _ := run(t, "", "who", "--has", "signing-key", "@repo=x"); strings.Contains(out, "keymac") || !strings.Contains(out, "plain") {
		t.Errorf("@repo=x:\n%s", out)
	}
}

func TestWhoHasRefusesWhatIsNoFact(t *testing.T) {
	factsMesh(t)
	for _, args := range [][]string{{"who", "--has", "token"}, {"who", "--has", "ssh"}, {"who", "--has", "signing-key", "--tools"}} {
		code, _, e := run(t, "", args...)
		if code != 2 || !(strings.Contains(e, "the facts are signing-key, deploy-token") || strings.Contains(e, "do not go together")) {
			t.Errorf("%v: %d %s", args, code, e)
		}
	}
}

// fleet shows each machine's facts in a column, and - for one that said nothing.
func TestFleetShowsWhatEachMachineHas(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, thShared, server.WithNode("n1"), server.WithVersion("0.11.3"), server.WithTokenSet(tokenTokens(t))))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", thShared)
	t.Setenv("AIVOICEBUS_ME", "deploy/claude")
	t.Setenv("AIVOICEBUS_MACHINE_TOKENS_DIR", "")
	hear(t, srv.URL, thAir, "air/claude")
	hear(t, srv.URL, thShared, "imac/codex")
	declare(t, "air/claude", msg.Labels{msg.HasLabel: "deploy-token+fleet-ctx-0.2.0", msg.SSHLabel: "0own"})

	code, out, e := run(t, "", "fleet")
	if code != 0 || !strings.Contains(out, "FACTS") {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	if l := lineOf(t, out, "air"); !strings.Contains(l, "deploy-token fleet-ctx:0.2.0 ssh:0own") {
		t.Errorf("air: %q", l)
	}
	if l := lineOf(t, out, "imac"); strings.Contains(l, "deploy-token") || !strings.Contains(l, " - ") {
		t.Errorf("imac said nothing: %q", l)
	}
	code, out, _ = run(t, "", "fleet", "--json")
	var got struct {
		Facts map[string]msg.Facts `json:"facts"`
	}
	if err := json.Unmarshal([]byte(out), &got); code != 0 || err != nil || !got.Facts["air"].DeployToken || got.Facts["air"].Tools["fleet-ctx"] != "0.2.0" || len(got.Facts) != 1 {
		t.Fatalf("%d %v %+v", code, err, got)
	}
}
