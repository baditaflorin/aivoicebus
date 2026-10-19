package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// startPerson stands in for a person at a terminal: each question start asks is answered from
// lines, each hidden prompt from secrets; a question nobody expected fails the test.
func startPerson(t *testing.T, lines, secrets []string) (asked *[]string) {
	t.Helper()
	asked = &[]string{}
	oldTerm, oldAsk, oldPass, oldTrust := startTerminal, askLine, readPassphrase, startTrustCodex
	t.Cleanup(func() { startTerminal, askLine, readPassphrase, startTrustCodex = oldTerm, oldAsk, oldPass, oldTrust })
	startTerminal = func(Streams) bool { return true }
	askLine = func(_ Streams, prompt string) (string, error) {
		*asked = append(*asked, prompt)
		if len(lines) == 0 {
			t.Errorf("unexpected question: %q", prompt)
			return "", errNoTerminal
		}
		a := lines[0]
		lines = lines[1:]
		return a, nil
	}
	readPassphrase = func(prompt string) (string, error) {
		*asked = append(*asked, prompt)
		if len(secrets) == 0 {
			t.Errorf("unexpected hidden prompt: %q", prompt)
			return "", errNoTerminal
		}
		a := secrets[0]
		secrets = secrets[1:]
		return a, nil
	}
	startTrustCodex = func([]string, Streams) error { *asked = append(*asked, "trust-codex"); return nil }
	return asked
}

// noPerson makes any question, hidden prompt or Codex start fail the test: nobody is there.
func noPerson(t *testing.T) {
	t.Helper()
	oldAsk, oldPass, oldTrust := askLine, readPassphrase, startTrustCodex
	t.Cleanup(func() { askLine, readPassphrase, startTrustCodex = oldAsk, oldPass, oldTrust })
	askLine = func(_ Streams, p string) (string, error) { t.Errorf("asked %q", p); return "", errNoTerminal }
	readPassphrase = func(p string) (string, error) { t.Errorf("asked for a secret: %q", p); return "", errNoTerminal }
	startTrustCodex = func([]string, Streams) error { t.Error("started Codex"); return nil }
}

// homeHashes is every file under dir with a hash of its content, to prove a run wrote nothing.
func homeHashes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range tree(t, dir) {
		b, _ := os.ReadFile(p)
		out[strings.TrimPrefix(p, dir)] = fmt.Sprintf("%x", sha256.Sum256(b))
	}
	return out
}

func diffSnapshots(a, b map[string]string) []string {
	var d []string
	for p, h := range b {
		if a[p] != h {
			d = append(d, p)
		}
	}
	for p := range a {
		if _, ok := b[p]; !ok {
			d = append(d, p+" (removed)")
		}
	}
	return d
}

func TestStartYesTakesAMachineFromNothingToDoctorAndASecondRunChangesNothing(t *testing.T) {
	sp, srv := newSpy(t, nodeHandler(t))
	home, _ := bareMachine(t)
	trustNode(t, srv.URL)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755) // Codex is installed: its hooks need trust
	noPerson(t)

	code, out, e := run(t, goodToken+"\n", "start", "--yes", "--url", srv.URL)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, e)
	}
	for _, want := range []string{st("done", "mesh"), st("ok", "setup"), st("left", "codex trust"), st("left", "approver"), st("skipped", "notify"), st("ok", "doctor"), st("none", "hello"), "/aivoicebus-standby", "start: done", "What you can do now:", "aivoicebus trust-codex", "aivoicebus approver init"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nFAIL") || strings.Contains(out+e, goodToken) {
		t.Errorf("a FAIL, or the token printed:\n%s\n%s", out, e)
	}
	if n := strings.Count(out, "\n  aivoicebus "); n != 3+2 { // three to try, two left
		t.Errorf("want 3 commands to try and 2 left, got %d lines:\n%s", n, out)
	}
	if fi, err := os.Stat(filepath.Join(home, ".config/aivoicebus/token")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("token file: %v %v", fi, err)
	}

	// again: every step is ok or skipped, nothing is written, nothing is sent
	before, sends := homeHashes(t, home), len(sp.to("/v1/send"))
	code, out, e = run(t, "", "start", "--yes")
	if code != 0 {
		t.Fatalf("second run: exit %d\n%s\n%s", code, out, e)
	}
	for _, want := range []string{st("ok", "mesh"), st("ok", "setup"), st("ok", "doctor")} {
		if !strings.Contains(out, want) {
			t.Errorf("second run: missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\ndone ") {
		t.Errorf("the second run did something again:\n%s", out)
	}
	if d := diffSnapshots(before, homeHashes(t, home)); len(d) > 0 {
		t.Errorf("the second run changed %v", d)
	}
	if n := len(sp.to("/v1/send")); n != sends {
		t.Errorf("the second run sent %d message(s)", n-sends)
	}
}

func TestStartYesWithNoMeshSaysWhatIsLeftAndWritesNothing(t *testing.T) {
	home, _ := bareMachine(t)
	noPerson(t)
	code, out, e := run(t, "", "start", "--yes")
	if code != 1 || !strings.Contains(out, st("left", "mesh")) || !strings.Contains(out, "aivoicebus start ") || strings.Contains(out, "What you can do now") {
		t.Errorf("exit %d\n%s\n%s", code, out, e)
	}
	// setup still ran (--yes), but no token and no node were written
	if _, err := os.Stat(filepath.Join(home, ".config/aivoicebus/token")); err == nil {
		t.Error("a token was written")
	}
}

func TestStartWithoutATerminalOrYesRefusesToBegin(t *testing.T) {
	home, _ := bareMachine(t)
	code, _, e := run(t, "", "start")
	if code != 2 || !strings.Contains(e, "--yes") {
		t.Errorf("exit %d: %s", code, e)
	}
	if left := tree(t, home); len(left) != 0 {
		t.Errorf("wrote %v", left)
	}
}

func TestStartInAnAgentSessionRefusesThePersonOnlySteps(t *testing.T) {
	t.Run("the mesh token", func(t *testing.T) {
		sp, srv := newSpy(t, nodeHandler(t))
		home, _ := bareMachine(t)
		trustNode(t, srv.URL)
		noPerson(t)
		t.Setenv("AIVOICEBUS_AGENT", "claude.ab12")
		code, out, e := run(t, goodToken+"\n", "start", "--yes", "--url", srv.URL)
		if code != 1 || !strings.Contains(out, st("refused", "mesh")) || !strings.Contains(out, "typing the mesh token") {
			t.Errorf("exit %d\n%s\n%s", code, out, e)
		}
		if sp.count() != 0 {
			t.Errorf("the node was contacted: %+v", sp.reqs)
		}
		if _, err := os.Stat(filepath.Join(home, ".config/aivoicebus/token")); err == nil {
			t.Error("a token piped in by an agent was saved")
		}
	})
	t.Run("trust, approver, notify", func(t *testing.T) {
		_, srv := newSpy(t, nodeHandler(t))
		home, _ := bareMachine(t)
		os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
		t.Setenv("AIVOICEBUS_URL", srv.URL)
		writeTokenFile(filepath.Join(home, ".config/aivoicebus/token"), goodToken)
		if code, out, e := run(t, "", "setup", "--yes"); code != 0 {
			t.Fatalf("setup: %d\n%s\n%s", code, out, e)
		}
		noPerson(t)
		t.Setenv("AIVOICEBUS_AGENT", "claude.ab12")
		for _, args := range [][]string{{"start"}, {"start", "--yes"}} {
			code, out, e := run(t, "", args...)
			for _, want := range []string{st("ok", "mesh"), st("refused", "codex trust"), st("refused", "approver"), st("skipped", "notify"), "the person's to do, not an agent's"} {
				if !strings.Contains(out, want) {
					t.Errorf("%v: missing %q in:\n%s\n%s", args, want, out, e)
				}
			}
			if code != 0 {
				t.Errorf("%v: exit %d (WARNs only)\n%s", args, code, out)
			}
		}
		if _, err := os.Stat(config.ApproverPath()); err == nil {
			t.Error("an approver key was made in an agent's session")
		}
	})
}

func TestStartAsksThePersonAndDoesEachStep(t *testing.T) {
	_, srv := newSpy(t, nodeHandler(t))
	home, _ := bareMachine(t) // the node is in no signed list: the URL typed at start's prompt is the confirmation
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	// the node URL; open Codex: yes; make an approver key: the default (yes); then the passphrase
	// twice and no notify URL
	asked := startPerson(t, []string{srv.URL, "y", ""}, []string{"correct horse battery", "correct horse battery", ""})
	code, out, e := run(t, goodToken+"\n", "start") // the token comes the way join takes it
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s\nasked %q", code, out, e, *asked)
	}
	for _, want := range []string{"(as you gave them)", st("done", "mesh"), st("ok", "setup"), st("done", "codex trust"), st("done", "approver"), st("skipped", "notify"), st("ok", "doctor")} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if _, err := config.ApproverPublic(); err != nil {
		t.Errorf("no approver key: %v", err)
	}
	if !strings.Contains(strings.Join(*asked, "|"), "trust-codex") {
		t.Errorf("Codex was not opened for trust: %q", *asked)
	}
	// again: only what is still not done is asked (Codex trust, which the stand-in did not save; the optional notify)
	asked = startPerson(t, []string{"n"}, []string{""})
	if code, out, e = run(t, "", "start"); code != 0 || !strings.Contains(out, st("ok", "approver")) || !strings.Contains(out, st("left", "codex trust")) {
		t.Errorf("second run: exit %d\n%s\n%s", code, out, e)
	}
	if len(*asked) != 2 {
		t.Errorf("second run asked %q", *asked)
	}
}

// bundle builds a client bundle as deploy/make-client-bundle.sh does.
func bundle(t *testing.T, urls, token string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range []struct{ name, body string }{{"aivoicebus-client-bundle/aivoicebus-arm64", "binary"}, {"aivoicebus-client-bundle/urls", urls + "\n"}, {"aivoicebus-client-bundle/token", token + "\n"}} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestStartJoinsWithAJoinLinkAndNeverPrintsIt(t *testing.T) {
	_, srv := newSpy(t, nodeHandler(t))
	home, _ := bareMachine(t)
	const key = "k3y0123456789abcdef"
	share := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bootstrap/"+key+".tgz" {
			http.NotFound(w, r)
			return
		}
		w.Write(bundle(t, srv.URL, goodToken))
	}))
	t.Cleanup(share.Close)
	link := share.URL + "/bootstrap/" + key + ".tgz"
	noPerson(t)

	code, out, e := run(t, link+"\n", "start", "--yes")
	if code != 0 || !strings.Contains(out, st("done", "mesh")) || !strings.Contains(out, "from the join link") {
		t.Fatalf("exit %d\n%s\n%s", code, out, e)
	}
	if strings.Contains(out+e, key) || strings.Contains(out+e, goodToken) {
		t.Errorf("the link or the token was printed:\n%s\n%s", out, e)
	}
	if tok, ok := readSavedToken(filepath.Join(home, ".config/aivoicebus/token")); !ok || tok != goodToken {
		t.Error("the token from the bundle was not saved")
	}

	// an expired link changes nothing and says so
	home2, _ := bareMachine(t)
	noPerson(t)
	code, out, _ = run(t, share.URL+"/bootstrap/gone.tgz\n", "start", "--yes")
	if code != 1 || !strings.Contains(out, "expired") {
		t.Errorf("expired link: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(home2, ".config/aivoicebus/token")); err == nil {
		t.Error("a token was written")
	}
}

func TestStartSaysHelloToAListeningSessionAndShowsTheAnswer(t *testing.T) {
	_, srv := newSpy(t, nodeHandler(t))
	home, _ := bareMachine(t)
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	writeTokenFile(filepath.Join(home, ".config/aivoicebus/token"), goodToken)
	if code, out, e := run(t, "", "setup", "--yes"); code != 0 {
		t.Fatalf("setup: %d\n%s\n%s", code, out, e)
	}
	noPerson(t)

	// a session on another machine stands by and answers the first hello it gets
	const listener = "desk/claude.zz9"
	lc := &client.Client{URLs: []string{srv.URL}, Token: goodToken, NoSign: true}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_, gen, _ := lc.InboxWait(ctx, listener, 0, false, 0)
		for ctx.Err() == nil {
			ms, g, err := lc.InboxWait(ctx, listener, gen, true, 20*time.Second)
			if err != nil {
				return
			}
			gen = g
			for _, m := range ms {
				if strings.Contains(m.Body, "hello from") {
					lc.Send(msg.Message{ID: msg.NewID(), From: listener, To: m.From, Re: m.ID, Thread: m.Thread, Body: "hello back from desk"})
					return
				}
			}
		}
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		r, _ := lc.Roster()
		if len(r) > 0 && r[0].Listening {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the listener never showed as listening")
		}
	}

	code, out, e := run(t, "", "start", "--yes", "--hello-wait", "10s")
	if code != 0 || !strings.Contains(out, "hello back from desk") || !strings.Contains(out, st("ok", "hello")) {
		t.Errorf("exit %d\n%s\n%s", code, out, e)
	}
}

// st is one of start's step lines, as far as its detail.
func st(status, name string) string { return fmt.Sprintf("%-12s %-22s", status, name) }
