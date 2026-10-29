package cli

import (
	"crypto/ed25519"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// answerTerminal answers the terminal prompt with word (or fails as a shell without a terminal does,
// when err is set) and counts how often it was asked.
func answerTerminal(t *testing.T, word *string, err *error) *int {
	t.Helper()
	asked := 0
	old := readPassphrase
	t.Cleanup(func() { readPassphrase = old })
	readPassphrase = func(string) (string, error) { asked++; return *word, *err }
	return &asked
}

func audited(t *testing.T, action string) string {
	t.Helper()
	es, _ := config.ReadAudit()
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Action == action {
			return es[i].Action + " " + es[i].Detail
		}
	}
	return ""
}

// Turning auto-update on is the person's alone: in an agent's session (the hook's AIVOICEBUS_AGENT, or
// an agent tool's own marker) it is refused before anything is asked, and without a terminal or the
// typed word it is refused too. Off is anyone's, as a pause is.
func TestAutoUpdateOnIsThePersonsAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_ME", "air/agent")
	word, noTerm := "auto", error(nil)
	asked := answerTerminal(t, &word, &noTerm)
	off := func(when string) {
		t.Helper()
		if config.LoadAutoUpdate().On {
			t.Fatalf("%s: auto-update was turned on", when)
		}
	}

	t.Setenv("AIVOICEBUS_AGENT", "claude.a1")
	if code, _, e := run(t, "", "update", "--auto", "on"); code == 0 || !strings.Contains(e, "not for an agent's session") || *asked != 0 {
		t.Errorf("in an agent's session: %d %q, asked %d", code, e, *asked)
	}
	off("AIVOICEBUS_AGENT")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("CLAUDECODE", "1")
	if code, _, e := run(t, "", "update", "--auto", "on"); code == 0 || !strings.Contains(e, "not for an agent's session") || *asked != 0 {
		t.Errorf("under an agent tool's marker: %d %q, asked %d", code, e, *asked)
	}
	off("CLAUDECODE")
	t.Setenv("CLAUDECODE", "")

	noTerm = errors.New("no terminal here (this is typed by a person, at a terminal, never piped): ask the user to run it in a terminal")
	if code, _, e := run(t, "auto\n", "update", "--auto", "on"); code == 0 || !strings.Contains(e, "needs a person at a terminal") {
		t.Errorf("without a terminal (the word piped in): %d %q", code, e)
	}
	off("no terminal")
	noTerm, word = nil, "yes"
	if code, _, e := run(t, "", "update", "--auto", "on"); code == 0 || !strings.Contains(e, "type auto") {
		t.Errorf("the wrong word: %d %q", code, e)
	}
	off("the wrong word")
	if audited(t, "update --auto on") != "" {
		t.Error("a refusal is not audited as turning it on")
	}

	word = "auto"
	code, out, e := run(t, "", "update", "--auto", "on")
	if code != 0 || !strings.Contains(out, "auto-update is on") {
		t.Fatalf("the person: %d %q %q", code, out, e)
	}
	if a := config.LoadAutoUpdate(); !a.On || a.By != "air/human" || a.At.IsZero() {
		t.Fatalf("the setting: %+v", a)
	}
	if audited(t, "update --auto on") == "" {
		t.Error("turning it on is audited")
	}
	n := *asked
	if code, out, _ := run(t, "", "update", "--auto", "on"); code != 0 || !strings.Contains(out, "already on") || *asked != n {
		t.Errorf("again: %d %q, asked again: %v", code, out, *asked != n)
	}
	if _, out, _ := run(t, "", "update", "--auto", "status"); !strings.Contains(out, "auto-update: on since") || !strings.Contains(out, "air/human") || !strings.Contains(out, "no run yet") {
		t.Errorf("status: %q", out)
	}

	t.Setenv("AIVOICEBUS_AGENT", "claude.a1")
	if code, out, e := run(t, "", "update", "--auto", "off"); code != 0 || !strings.Contains(out, "auto-update is off") {
		t.Fatalf("an agent turns it off: %d %q %q", code, out, e)
	}
	off("off")
	if audited(t, "update --auto off") == "" {
		t.Error("turning it off is audited")
	}
	if _, out, _ := run(t, "", "update", "--auto", "status"); !strings.Contains(out, "auto-update: off") {
		t.Errorf("status when off: %q", out)
	}
	for _, args := range [][]string{{"--auto", "on", "--force"}, {"--auto", "run", "--from", "github"}, {"--auto", "sideways"}, {"--auto", "on", "extra"}} {
		if code, _, e := run(t, "", append([]string{"update"}, args...)...); code != 2 || !strings.Contains(e, "update --auto on|off|status|run") {
			t.Errorf("update %v: %d %q", args, code, e)
		}
	}
}

// signedReleaseOf is signedRelease with a binary of the test's own.
func signedReleaseOf(t *testing.T, ver string, bin []byte) (string, ed25519.PublicKey) {
	t.Helper()
	priv, pub, err := release.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, release.AssetName(runtime.GOOS, runtime.GOARCH)), bin, 0o755)
	if _, err := release.Pack(release.PackOptions{Dir: dir, Version: ver, Key: priv}); err != nil {
		t.Fatal(err)
	}
	return dir, pub
}

// autoMesh serves release dir from a node, points the CLI at it as an agent's session (the hook starts
// the run from one), and installs a fake 0.2.0 at ~/.local/bin/aivoicebus, which is what a run replaces.
func autoMesh(t *testing.T, dir string, pub ed25519.PublicKey) string {
	t.Helper()
	withVersion(t, "0.2.0")
	trust(t, pub)
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, "tok", server.WithReleaseDir(dir)))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_ME", "air/claude.a1")
	t.Setenv("AIVOICEBUS_AGENT", "claude.a1")
	bin := filepath.Join(home, ".local", "bin", "aivoicebus")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, fakeBinary("0.2.0"), 0o755)
	return bin
}

func binIs(t *testing.T, bin, ver string) {
	t.Helper()
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "aivoicebus "+ver+" ") {
		t.Fatalf("the installed binary is not %s:\n%s", ver, b)
	}
}

// The run the hook starts does nothing while the setting is off, installs through the verified path
// when it is on, records what it did, and goes by the installed binary afterwards.
func TestAutoRunInstallsWhenOnAndRecordsIt(t *testing.T) {
	dir, pub := signedRelease(t, "0.3.0")
	bin := autoMesh(t, dir, pub)

	if code, out, _ := run(t, "", "update", "--auto", "run"); code != 0 || !strings.Contains(out, "auto-update is off") {
		t.Fatalf("off: %d %q", code, out)
	}
	binIs(t, bin, "0.2.0")
	if !config.LoadAutoState().Tried.IsZero() {
		t.Fatal("a run while off records nothing")
	}

	if err := config.SaveAutoUpdate(config.AutoUpdate{On: true}); err != nil {
		t.Fatal(err)
	}
	code, out, e := run(t, "", "update", "--auto", "run")
	if code != 0 || !strings.Contains(out, "installed aivoicebus 0.3.0") || !strings.Contains(out, "setup-ran-0.3.0") {
		t.Fatalf("on: %d %q %q", code, out, e)
	}
	binIs(t, bin, "0.3.0")
	if st := config.LoadAutoState(); st.Result != config.AutoInstalled || st.From != "0.2.0" || st.To != "0.3.0" || st.Tried.IsZero() || st.Told {
		t.Fatalf("the record: %+v", st)
	}
	if a := audited(t, "update --auto run"); !strings.Contains(a, "installed 0.3.0 (ran 0.2.0)") {
		t.Errorf("the audit: %q", a)
	}
	if _, out, _ := run(t, "", "update", "--auto", "status"); !strings.Contains(out, "installed 0.3.0 (over 0.2.0)") {
		t.Errorf("status: %q", out)
	}

	// This process still says 0.2.0, the installed binary 0.3.0: nothing to do, and .prev stays 0.2.0.
	if code, _, e := run(t, "", "update", "--auto", "run"); code != 0 {
		t.Fatalf("again: %d %q", code, e)
	}
	if st := config.LoadAutoState(); st.Result != config.AutoCurrent {
		t.Errorf("again: %+v", st)
	}
	binIs(t, bin+".prev", "0.2.0")
}

// A release whose setup fails is put back by itself, and left out from then on.
func TestAutoRunRollsBackAFailedSetupAndLeavesThatReleaseOut(t *testing.T) {
	broken := []byte("#!/bin/sh\ncase \"$1\" in\n  version) echo \"aivoicebus 0.3.0 (commit test, test/test)\" ;;\n  setup) echo \"cannot write the hook\" ; exit 1 ;;\nesac\n")
	dir, pub := signedReleaseOf(t, "0.3.0", broken)
	bin := autoMesh(t, dir, pub)
	config.SaveAutoUpdate(config.AutoUpdate{On: true})

	code, out, e := run(t, "", "update", "--auto", "run")
	if code == 0 || !strings.Contains(e, "`aivoicebus setup` of release 0.3.0 failed: cannot write the hook") || !strings.Contains(out, "rolled back to aivoicebus 0.2.0") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	binIs(t, bin, "0.2.0")
	st := config.LoadAutoState()
	if st.Result != config.AutoRolledBack || st.Skip != "0.3.0" || st.To != "0.3.0" || !strings.Contains(st.Error, "cannot write the hook") {
		t.Fatalf("the record: %+v", st)
	}
	if code, out, _ := run(t, "", "update", "--auto", "run"); code != 0 || !strings.Contains(out, "rolled back on this machine") {
		t.Fatalf("again: %d %q", code, out)
	}
	binIs(t, bin, "0.2.0")
	if st := config.LoadAutoState(); st.Result != config.AutoSkipped || st.Skip != "0.3.0" {
		t.Errorf("again: %+v", st)
	}
}

// When the nodes announced a release that the node the run reads does not hold, the run is a failure
// the person hears of, not "up to date": otherwise the machine would stay behind in silence.
func TestAutoRunCallsAnAnnouncedReleaseItCannotGetAFailure(t *testing.T) {
	dir, pub := signedRelease(t, "0.3.0")
	bin := autoMesh(t, dir, pub)
	os.WriteFile(bin, fakeBinary("0.3.0"), 0o755) // already runs what this node holds
	config.SaveAutoUpdate(config.AutoUpdate{On: true})
	_ = config.EditAutoState(func(s *config.AutoState) bool { s.Tried, s.To = time.Now(), "0.4.0"; return true }) // another node announced 0.4.0
	code, _, e := run(t, "", "update", "--auto", "run")
	if code == 0 || !strings.Contains(e, "the nodes announce release 0.4.0 but the one read holds 0.3.0: `aivoicebus mesh`") {
		t.Fatalf("%d %q", code, e)
	}
	if st := config.LoadAutoState(); st.Result != config.AutoFailed || st.To != "0.4.0" || !strings.Contains(st.Error, "announce release 0.4.0") {
		t.Fatalf("the record: %+v", st)
	}
	// Announced and held alike: up to date.
	_ = config.EditAutoState(func(s *config.AutoState) bool { s.To = "0.3.0"; return true })
	if code, _, e := run(t, "", "update", "--auto", "run"); code != 0 || config.LoadAutoState().Result != config.AutoCurrent {
		t.Fatalf("up to date: %d %q %+v", code, e, config.LoadAutoState())
	}
}

// Two runs never install at once: the second finds the lock held and does nothing.
func TestAutoRunWaitsForNoOtherRun(t *testing.T) {
	dir, pub := signedRelease(t, "0.3.0")
	bin := autoMesh(t, dir, pub)
	config.SaveAutoUpdate(config.AutoUpdate{On: true})
	unlock, ok := config.Lock(config.AutoLockPath(), 0)
	if !ok {
		t.Fatal("no lock")
	}
	defer unlock()
	if code, out, _ := run(t, "", "update", "--auto", "run"); code != 0 || !strings.Contains(out, "another automatic update is running") {
		t.Fatalf("%d %q", code, out)
	}
	binIs(t, bin, "0.2.0")
}

// A release the person rolls back by hand is one auto-update leaves out.
func TestRollbackRecordsTheReleaseAutoUpdateLeavesOut(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withVersion(t, "0.2.0")
	dir, pub := signedRelease(t, "0.3.0")
	trust(t, pub)
	bin := installedFake(t, "0.2.0")
	if code, _, e := run(t, "", "update", "--from", "dir:"+dir, "--bin", bin); code != 0 {
		t.Fatalf("update: %q", e)
	}
	if code, _, e := run(t, "", "update", "--rollback", "--bin", bin); code != 0 {
		t.Fatalf("rollback: %q", e)
	}
	if st := config.LoadAutoState(); st.Skip != "0.3.0" {
		t.Errorf("the record: %+v", st)
	}
}

// The person's queue leaves out a fleet update task for a release this machine already runs.
func TestQueueLeavesOutAnUpdateTaskThatIsDone(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "deploy/claude")
	text := "Update aivoicebus on air: the nodes run 0.3.0. In a terminal there: aivoicebus update (it checks the signature before installing), then aivoicebus version."
	if code, _, e := run(t, "", "task", "air", "--person", text); code != 0 {
		t.Fatalf("task: %q", e)
	}
	t.Setenv("AIVOICEBUS_ME", "air/agent") // the person, at a terminal
	withVersion(t, "0.2.0")
	if _, out, _ := run(t, "", "queue"); !strings.Contains(out, "Update aivoicebus on air") {
		t.Fatalf("behind, the task waits: %q", out)
	}
	withVersion(t, "0.3.0")
	if _, out, _ := run(t, "", "queue"); strings.Contains(out, "Update aivoicebus on air") || !strings.Contains(out, "no tasks waiting") {
		t.Errorf("running 0.3.0, the task is done: %q", out)
	}
}
