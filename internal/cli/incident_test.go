package cli

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// The fake secrets of these tests. None may ever appear in what incident prints.
const (
	incShared   = "shared-0123456789abcdefSHARED"
	incMachine  = "machine-0123456789abcdefMACHINE"
	incObserver = "observer-0123456789abcdefOBSERVER"
	incSaved    = "ghp_SaVeD0123456789abcdefghijklmnopqrst"
	incUnknown  = "sk-unknown-0123456789abcdefghijklmnop"
)

var incSecrets = []string{incShared, incMachine, incObserver, incSaved, incUnknown}

// fakeDeploy is a deploy/ directory whose scripts only write down how they were called (one
// line each, to the returned log), print the planted secret if FAKE_ECHO says so, and fail on
// --apply if FAKE_FAIL_APPLY is set.
func fakeDeploy(t *testing.T) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(t.TempDir(), "calls")
	script := `#!/bin/sh
echo "$(basename "$0") $*" >> "$FAKE_LOG"
echo "fake $(basename "$0") $*"
[ -z "$FAKE_ECHO" ] || echo "the token is $FAKE_ECHO" >&2
case " $* " in *" --apply "*) [ -z "$FAKE_FAIL_APPLY" ] || exit 1 ;; esac
exit 0
`
	for _, f := range []string{"rotate-token.sh", "machine-token.sh", "install.sh"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FAKE_LOG", log)
	t.Setenv("FAKE_ECHO", "")
	t.Setenv("FAKE_FAIL_APPLY", "")
	t.Setenv("AIVOICEBUS_DEPLOY_DIR", dir)
	return dir, log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// incidentMesh is one node on the shared token, this machine (imac, the deploy machine) holding
// every kind of credential, and messages that held them. It returns the store and the ids.
func incidentMesh(t *testing.T) (*store.Store, map[string]string) {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	s.SetSendLimit(1 << 20)
	srv := httptest.NewServer(server.NewHandler(s, incShared))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", incShared)
	t.Setenv("AIVOICEBUS_TOKEN_FILE", "")
	t.Setenv("AIVOICEBUS_MACHINE_TOKENS_DIR", "")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Setenv("AIVOICEBUS_ME", "imac/person")
	t.Chdir(t.TempDir())
	conf := filepath.Join(home, ".config", "aivoicebus")
	for f, v := range map[string]string{
		"token":                incShared,
		"observer-token":       incObserver,
		"machine-tokens/lap":   incMachine,
		"credentials/gh/token": incSaved,
	} {
		p := filepath.Join(conf, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Written to the node directly: a node refuses a message holding a token it accepts, and the
	// CLI one holding a secret of this machine, but another machine knows neither.
	ids := map[string]string{}
	for name, body := range map[string]string{
		"shared":   "use this: " + incShared,
		"machine":  "lap's token " + incMachine,
		"observer": "dashboard " + incObserver,
		"secret":   "GH=" + incSaved,
		"unknown":  "the api key " + incUnknown,
		"clean":    "nothing secret here",
	} {
		m, err := s.Send(msg.Message{From: "lap/claude.a1", To: "box/codex.b2", Body: body})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = m.ID
	}
	if err := s.Ack(ids["shared"], "box/codex.b2"); err != nil {
		t.Fatal(err)
	}
	return s, ids
}

func noSecrets(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		for _, v := range incSecrets {
			if strings.Contains(text, v) {
				t.Fatalf("a secret was printed: %q", text)
			}
		}
	}
}

func TestIncidentPlansEachKindAndChangesNothing(t *testing.T) {
	s, ids := incidentMesh(t)
	_, log := fakeDeploy(t)
	cases := []struct {
		args    []string
		want    []string
		scripts []string // the dry runs the plan runs
		found   string   // the message it finds by the secret's value
	}{
		{[]string{"shared"}, []string{"the shared token leaked", "rotate-token.sh add --apply", "rotate-token.sh promote --apply", "retire --apply", "read by box/codex.b2"},
			[]string{"rotate-token.sh add"}, ids["shared"]},
		{[]string{"machine", "lap"}, []string{"the machine token of lap leaked", "machine-token.sh revoke lap --apply", "machine-token.sh add lap --apply"},
			[]string{"machine-token.sh revoke lap"}, ids["machine"]},
		{[]string{"observer"}, []string{"the observer token leaked", "[you] mv ", "--gen-observer-token"}, nil, ids["observer"]},
		{[]string{"secret", "gh"}, []string{"the saved credential gh leaked", "[you] revoke gh", "secret rm gh"}, nil, ids["secret"]},
		{[]string{"message", ids["unknown"]}, []string{"a credential in message " + ids["unknown"], "[you] revoke or rotate", "nobody has read it"}, nil, ids["unknown"]},
	}
	for _, c := range cases {
		os.Remove(log)
		code, out, e := run(t, "", append([]string{"incident"}, c.args...)...)
		if code != 0 {
			t.Fatalf("%v: %d %s %s", c.args, code, out, e)
		}
		noSecrets(t, out, e)
		for _, w := range append(c.want, "PLAN ONLY", c.found) {
			if !strings.Contains(out, w) {
				t.Errorf("%v: no %q in:\n%s", c.args, w, out)
			}
		}
		if strings.Contains(out, ids["clean"]) {
			t.Errorf("%v: a message without the secret is in the plan:\n%s", c.args, out)
		}
		got := calls(t, log)
		if strings.Join(got, "|") != strings.Join(c.scripts, "|") {
			t.Errorf("%v: scripts run %q, want only the dry run %q", c.args, got, c.scripts)
		}
	}
	for _, m := range s.Messages() {
		if m.Retracted != nil {
			t.Errorf("a plan retracted %s", m.ID)
		}
	}
	if es, _ := config.ReadAudit(); len(es) != 0 {
		t.Errorf("a plan wrote to the audit log: %v", es)
	}
}

func TestIncidentApplyRefusesAnAgentAndNoTerminal(t *testing.T) {
	s, ids := incidentMesh(t)
	_, log := fakeDeploy(t)
	asked := typed(t, "incident")

	t.Setenv("AIVOICEBUS_AGENT", "codex.x1")
	code, out, e := run(t, "incident", "machine", "lap", "--apply")
	if code == 0 || !strings.Contains(e, "not for an agent's session") || out != "" {
		t.Errorf("an agent ran it: %d %q %q", code, out, e)
	}
	t.Setenv("AIVOICEBUS_AGENT", "")

	t.Setenv("AIVOICEBUS_DETECT", "")
	t.Setenv("CODEX_THREAD_ID", "0199-abc")
	if code, _, e := run(t, "incident", "machine", "lap", "--apply"); code == 0 || !strings.Contains(e, "inside codex") {
		t.Errorf("an agent tool above it, not saying so: %d %q", code, e)
	}
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("AIVOICEBUS_DETECT", "off")

	// stdin is not a terminal (a pipe saying "incident" is not a person)
	if code, _, e := run(t, "incident\n", "incident", "machine", "lap", "--apply"); code == 0 || !strings.Contains(e, "needs a person at a terminal") {
		t.Errorf("no terminal: %d %q", code, e)
	}
	if len(*asked) != 0 || len(calls(t, log)) != 0 {
		t.Errorf("refused, but asked %v / ran %v", *asked, calls(t, log))
	}
	for _, m := range s.Messages() {
		if m.Retracted != nil {
			t.Errorf("refused, but retracted %s (%v)", m.ID, ids)
		}
	}
}

// fakeTerminal makes stdin count as a person's terminal; typed answers the question.
func fakeTerminal(t *testing.T) {
	t.Helper()
	old := incidentTerminal
	incidentTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { incidentTerminal = old })
}

func TestIncidentApplyRunsTheScriptsOnlyAfterConfirmingThenRetracts(t *testing.T) {
	s, ids := incidentMesh(t)
	_, log := fakeDeploy(t)
	fakeTerminal(t)
	t.Setenv("FAKE_ECHO", incMachine) // a script that prints the secret is masked

	typed(t, "no")
	if code, _, e := run(t, "", "incident", "machine", "lap", "--apply"); code == 0 || !strings.Contains(e, "not confirmed") {
		t.Fatalf("a no: %d %q", code, e)
	}
	if got := calls(t, log); len(got) != 1 || got[0] != "machine-token.sh revoke lap" {
		t.Fatalf("before the yes only the dry run may run: %q", got)
	}

	os.Remove(log)
	asked := typed(t, "incident")
	code, out, e := run(t, "", "incident", "machine", "lap", "--apply", "--msg", ids["clean"])
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, e)
	}
	noSecrets(t, out, e)
	if !strings.Contains(out, "<credential>") {
		t.Errorf("the script's secret was not masked:\n%s", out)
	}
	if len(*asked) != 1 {
		t.Errorf("asked %v", *asked)
	}
	if got := calls(t, log); strings.Join(got, "|") != "machine-token.sh revoke lap|machine-token.sh revoke lap --apply" {
		t.Errorf("scripts: %q", got)
	}
	for _, m := range s.Messages() {
		want := m.ID == ids["machine"] || m.ID == ids["clean"]
		if got := m.Retracted != nil && m.Retracted.Leak; got != want {
			t.Errorf("%s (%q): retracted as a leak %v, want %v", m.ID, m.Body, got, want)
		}
	}
	es, _ := config.ReadAudit()
	var actions []string
	for _, e := range es {
		actions = append(actions, e.Action+": "+e.Detail)
	}
	all := strings.Join(actions, "\n")
	if len(es) != 2 || es[0].Action != "incident-start" || es[1].Action != "incident" || !strings.Contains(es[1].Detail, ids["machine"]) || !strings.Contains(es[1].Detail, "machine lap") {
		t.Errorf("audit:\n%s", all)
	}
	noSecrets(t, all)
}

// The credential comes first: if revoking it fails, nothing is retracted (that would only point
// at a credential that still works).
func TestIncidentStopsBeforeRetractingWhenTheScriptFails(t *testing.T) {
	s, ids := incidentMesh(t)
	_, log := fakeDeploy(t)
	fakeTerminal(t)
	typed(t, "incident")
	t.Setenv("FAKE_FAIL_APPLY", "1")
	code, out, e := run(t, "", "incident", "shared", "--apply")
	if code == 0 || !strings.Contains(e, "nothing was retracted") {
		t.Fatalf("%d %s %s", code, out, e)
	}
	if got := calls(t, log); strings.Join(got, "|") != "rotate-token.sh add|rotate-token.sh add --apply" {
		t.Errorf("scripts after the failing one ran: %q", got)
	}
	for _, m := range s.Messages() {
		if m.Retracted != nil {
			t.Errorf("retracted %s although the token was not rotated (%v)", m.ID, ids)
		}
	}
	if es, _ := config.ReadAudit(); len(es) != 2 || !strings.Contains(es[1].Detail, "nothing retracted") {
		t.Errorf("audit: %v", es)
	}
}

func TestIncidentSecretAndMessageKindsNeedThePersonsStepAndRemoveTheCopy(t *testing.T) {
	s, ids := incidentMesh(t)
	_, log := fakeDeploy(t)
	fakeTerminal(t)
	asked := typed(t, "incident")
	code, out, e := run(t, "", "incident", "secret", "gh", "--apply")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, e)
	}
	noSecrets(t, out, e)
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "[you]") {
		t.Errorf("the person's step must come first: %q", *asked)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config/aivoicebus/credentials/gh/token")); !os.IsNotExist(err) {
		t.Errorf("this machine's copy is still there: %v", err)
	}
	if len(calls(t, log)) != 0 {
		t.Errorf("no script revokes a saved secret: %q", calls(t, log))
	}
	typed(t, "incident")
	if code, out, e := run(t, "", "incident", "message", ids["unknown"], "--apply"); code != 0 {
		t.Fatalf("%d %s %s", code, out, e)
	} else {
		noSecrets(t, out, e)
	}
	for _, m := range s.Messages() {
		want := m.ID == ids["secret"] || m.ID == ids["unknown"]
		if got := m.Retracted != nil && m.Retracted.Leak; got != want {
			t.Errorf("%s: retracted %v, want %v", m.ID, got, want)
		}
	}
}

func TestIncidentUsage(t *testing.T) {
	incidentMesh(t)
	for _, args := range [][]string{{}, {"nothing"}, {"machine"}, {"machine", "Bad Host!"}, {"secret"}, {"message", "xyz"}, {"shared", "extra"}, {"machine", "lap", "--retire"}} {
		if code, _, e := run(t, "", append([]string{"incident"}, args...)...); code == 0 {
			t.Errorf("%v: accepted (%q)", args, e)
		}
	}
	if code, _, e := run(t, "", "incident", "-h"); code != 0 || !strings.Contains(e, "shared") {
		t.Errorf("-h: %d %q", code, e)
	}
}
