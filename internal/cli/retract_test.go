package cli

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// meshStore is mesh that also hands back the node's store, for what the CLI cannot show.
func meshStore(t *testing.T) *store.Store {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	s.SetSendLimit(1 << 20)
	srv := httptest.NewServer(server.NewHandler(s, "tok"))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Chdir(t.TempDir())
	return s
}

// sendID sends as me and returns the id from the one line stdout holds.
func sendID(t *testing.T, me, to, body string) string {
	t.Helper()
	t.Setenv("AIVOICEBUS_ME", me)
	code, out, e := run(t, "", "send", "--to", to, body)
	f := strings.Fields(out)
	if code != 0 || len(f) != 4 || f[0] != "sent" || f[2] != "thread" {
		t.Fatalf("send: %d %q %q", code, out, e)
	}
	return f[1]
}

func TestSendSaysHowToUndoItOnStderrAndKeepsStdoutOneLine(t *testing.T) {
	meshStore(t)
	t.Setenv("AIVOICEBUS_ME", "imac/claude.1")
	code, out, e := run(t, "", "send", "--to", "air/codex", "hello")
	f := strings.Fields(out)
	if code != 0 || len(f) != 4 || strings.Count(out, "\n") != 1 {
		t.Fatalf("a script reads the id from the one line on stdout: %d %q", code, out)
	}
	if !strings.Contains(e, "undo: aivoicebus retract "+f[1]+"\n") || strings.Contains(e, "--leak") {
		t.Errorf("%q", e)
	}
}

func TestSendThatOnlyWarnsAboutACredentialShapeAlsoSaysHowToFlagIt(t *testing.T) {
	meshStore(t)
	t.Setenv("AIVOICEBUS_ME", "imac/claude.1")
	code, out, e := run(t, "", "send", "--to", "air/codex", "the password: Xk9mQ2vL8nR4 for staging")
	id := strings.Fields(out)[1]
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, e)
	}
	for _, want := range []string{"warning: this message contains something that looks like a password or token value", "undo: aivoicebus retract " + id, "if that went out: aivoicebus retract --leak " + id} {
		if !strings.Contains(e, want) {
			t.Errorf("missing %q in %q", want, e)
		}
	}
}

func TestSendThatTheBackstopRefusesSaysNothingWasSent(t *testing.T) {
	s := meshStore(t)
	t.Setenv("AIVOICEBUS_ME", "imac/claude.1")
	code, out, e := run(t, "", "send", "--to", "air/codex", "key "+"gh"+"p_"+strings.Repeat("aB3dE", 8))
	if code != 1 || out != "" || !strings.Contains(e, "refused:") || !strings.Contains(e, "(nothing was sent)") || strings.Contains(e, "undo:") {
		t.Errorf("%d %q %q", code, out, e)
	}
	if s.Stats().Messages != 0 {
		t.Error("nothing stored")
	}
}

func TestRetractEndToEnd(t *testing.T) {
	s := meshStore(t)
	id := sendID(t, "imac/claude.1", "air/codex.2", "sent to the wrong session")

	// who it would reach comes before anything is written
	code, out, e := run(t, "", "retract", "--dry-run")
	for _, want := range []string{"last message sent by this session: " + id + " to air/codex.2:", "sent to the wrong session", "dry run: nothing was written", "would retract " + id, "does NOT erase it"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("dry run missing %q (%d): %q %q", want, code, out, e)
		}
	}
	if s.Stats().Retracted != 0 {
		t.Fatal("a dry run writes nothing")
	}

	code, out, e = run(t, "", "retract")
	if code != 0 || e != "" || !strings.Contains(out, "retracted "+id) || !strings.Contains(out, "no other session had read it") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	if !strings.Contains(out, "warning: only 1 node(s) took the write") { // one node, so one took it: said where the rest is
		t.Errorf("%q", out)
	}

	t.Setenv("AIVOICEBUS_ME", "air/codex.2")
	if _, out, _ := run(t, "", "inbox"); !strings.Contains(out, "(no unread messages)") {
		t.Errorf("hidden at once:\n%s", out)
	}
	_, out, _ = run(t, "", "inbox", "--all")
	if !strings.Contains(out, "--- "+id) || !strings.Contains(out, "[retracted]") || !strings.Contains(out, "[withdrawn by imac at ") || strings.Contains(out, "wrong session") {
		t.Errorf("--all lists a tombstone:\n%s", out)
	}
	_, out, _ = run(t, "", "thread", id)
	if !strings.Contains(out, "[retracted]") || strings.Contains(out, "wrong session") {
		t.Errorf("a thread shows it in place:\n%s", out)
	}

	t.Setenv("AIVOICEBUS_ME", "imac/claude.1")
	code, out, _ = run(t, "", "retract", id)
	if code != 0 || !strings.Contains(out, "already retracted by imac/claude.1 at ") {
		t.Errorf("asking again is not an error: %d %q", code, out)
	}
}

func TestRetractTakesItsFlagsBeforeOrAfterTheId(t *testing.T) {
	s := meshStore(t)
	for _, args := range [][]string{{"--leak", "--dry-run", "ID"}, {"ID", "--leak", "--dry-run"}, {"--dry-run", "ID", "--leak"}} {
		id := sendID(t, "imac/claude.1", "air/codex", "x"+strings.Join(args, ""))
		for i, a := range args {
			if a == "ID" {
				args[i] = id
			}
		}
		code, out, e := run(t, "", append([]string{"retract"}, args...)...)
		if code != 0 || !strings.Contains(out, "would retract "+id+", flagged as having held a credential") {
			t.Errorf("%v: %d %q %q", args, code, out, e)
		}
	}
	if s.Stats().Retracted != 0 {
		t.Error("dry runs")
	}
	for _, args := range [][]string{{"abcdef0123", "abcdef0124"}, {"a", "--leak", "b"}} {
		if code, _, e := run(t, "", append([]string{"retract"}, args...)...); code != exitUsage || !strings.Contains(e, "usage: retract [ID] [--leak] [--dry-run] [--last] [--as IDENTITY]") {
			t.Errorf("%v: %d %q", args, code, e)
		}
	}
	if code, _, _ := run(t, "", "retract", "--bogus"); code != 2 {
		t.Errorf("a bad flag exits 2: %d", code)
	}
	if code, _, e := run(t, "", "retract", "-h"); code != 0 || !strings.Contains(e, "--as") && !strings.Contains(e, "-as") {
		t.Errorf("-h: %d %q", code, e)
	}
}

func TestRetractRefusesAnotherMachinesMessageAndSaysWhy(t *testing.T) {
	s := meshStore(t)
	id := sendID(t, "imac/claude.1", "air/codex", "mine")
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	code, out, e := run(t, "", "retract", id)
	if code != 1 || !strings.Contains(e, "was sent from imac; this machine is air; only that machine can retract it") || !strings.Contains(out, `to air/codex: "mine"`) {
		t.Errorf("%d %q %q", code, out, e)
	}
	if s.Stats().Retracted != 0 {
		t.Error("nothing written")
	}
	if code, _, e = run(t, "", "retract", "--as", "nope", id); code != 1 || !strings.Contains(e, "--as") {
		t.Errorf("%d %q", code, e)
	}
}

func TestRetractWithNothingRememberedAsksForAnId(t *testing.T) {
	meshStore(t)
	t.Setenv("AIVOICEBUS_ME", "imac/claude.1")
	if code, out, e := run(t, "", "retract"); code != 1 || out != "" || !strings.Contains(e, "give the message id") {
		t.Errorf("%d %q %q", code, out, e)
	}
}

func TestInboxAllAckDoesNotAcknowledgeAWithdrawnMessage(t *testing.T) {
	s := meshStore(t)
	gone := sendID(t, "imac/claude.1", "air/codex", "withdrawn text")
	kept := sendID(t, "imac/claude.1", "air/codex", "still here")
	if code, _, e := run(t, "", "retract", gone); code != 0 {
		t.Fatalf("%d %q", code, e)
	}
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	if code, out, _ := run(t, "", "inbox", "--all", "--ack"); code != 0 || !strings.Contains(out, "still here") || !strings.Contains(out, "[retracted]") {
		t.Fatalf("%d\n%s", code, out)
	}
	acked := map[string]bool{}
	for _, r := range s.Since(map[string]uint64{}, 1000) {
		if r.Ack != nil && r.Ack.By == "air/codex" {
			acked[r.Ack.ID] = true
		}
	}
	if !acked[kept] || acked[gone] {
		t.Errorf("a message that was read is acknowledged and one that was only a tombstone is not: %v", acked)
	}
	if res, err := s.RetractDryRun(gone, "imac/claude.1", false); err != nil || res.ReadBy != 0 {
		t.Errorf("nobody read it: %+v %v", res, err)
	}
}

func TestReplyingToAWithdrawnMessageWarnsButSends(t *testing.T) {
	s := meshStore(t)
	id := sendID(t, "imac/claude.1", "air/codex", "question?")
	s.Retract(id, "imac/claude.1", false)
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	code, out, e := run(t, "", "send", "--re", id, "answer")
	if code != 0 || !strings.HasPrefix(out, "sent ") || !strings.Contains(e, "warning: message "+id+" was withdrawn by imac") {
		t.Errorf("%d %q %q", code, out, e)
	}
	if got := s.Thread(id); len(got) != 2 {
		t.Errorf("the reply is in the thread: %+v", got)
	}
}

func TestTheUsageLineAndTheCommandTableKnowRetract(t *testing.T) {
	if _, ok := commands["retract"]; !ok {
		t.Error("retract is a command")
	}
	if _, out, _ := run(t, "", "help"); !strings.Contains(out, "\n  retract") {
		t.Errorf("%q", out)
	}
}

func TestRetractWithNoIdTakesOnlyThisSessionsLastMessageUnlessLast(t *testing.T) {
	s := meshStore(t)
	theirs := sendID(t, "imac/claude.1", "air/codex", "the other session's note")
	t.Setenv("AIVOICEBUS_ME", "imac/claude.2") // this session has sent nothing

	code, out, e := run(t, "", "retract")
	if code != 1 || out != "" || s.Stats().Retracted != 0 {
		t.Fatalf("it refuses and writes nothing: %d %q %q", code, out, e)
	}
	for _, want := range []string{"error: nothing sent as imac/claude.2 is remembered", "last message from this machine: " + theirs + " sent as imac/claude.1 to air/codex at ", `"the other session's note"`,
		"retract it with: aivoicebus retract " + theirs, "aivoicebus retract --last"} {
		if !strings.Contains(e, want) {
			t.Errorf("missing %q in %q", want, e)
		}
	}

	code, out, e = run(t, "", "retract", "--dry-run")
	if code != 0 || !strings.Contains(out, "last message from this machine: "+theirs+" sent as imac/claude.1") || !strings.Contains(out, "note: a real run with no id would refuse") || !strings.Contains(out, "would retract "+theirs) || s.Stats().Retracted != 0 {
		t.Errorf("a dry run is always allowed: %d %q %q", code, out, e)
	}

	if code, _, e = run(t, "", "retract", "--last", theirs); code != 1 || !strings.Contains(e, "--last takes the machine's latest message: give it no id") || s.Stats().Retracted != 0 {
		t.Errorf("%d %q", code, e)
	}
	code, out, e = run(t, "", "retract", "--last")
	if code != 0 || !strings.Contains(out, "last message from this machine: "+theirs+" sent as imac/claude.1") || !strings.Contains(out, "retracted "+theirs) || s.Stats().Retracted != 1 {
		t.Errorf("--last takes the machine's: %d %q %q", code, out, e)
	}
	if r := s.Thread(theirs)[0].Retracted; r == nil || r.By != "imac/claude.2" {
		t.Errorf("%+v", r)
	}
}

func TestRetractWithAnEmptyIdIsAUsageErrorNotTheLastMessage(t *testing.T) {
	s := meshStore(t)
	id := sendID(t, "imac/claude.1", "air/codex", "mine, and the last one sent")
	for _, args := range [][]string{{"retract", ""}, {"retract", "", "--leak"}, {"retract", "--leak", ""}, {"retract", "--dry-run", "", "--last"}} {
		code, out, e := run(t, "", args...)
		if code != exitUsage || out != "" || !strings.Contains(e, "usage: retract [ID] [--leak] [--dry-run] [--last] [--as IDENTITY]: the id is empty") {
			t.Errorf("%q: %d %q %q", args, code, out, e)
		}
	}
	if s.Stats().Retracted != 0 || s.Thread(id)[0].Retracted != nil {
		t.Error("a variable that was never set must not take back the last message")
	}
	// the same message is the default when no argument is given at all
	if code, out, _ := run(t, "", "retract"); code != 0 || !strings.Contains(out, "last message sent by this session: "+id) {
		t.Errorf("%d %q", code, out)
	}
}

func TestRetractHelpDocumentsLastAndTheDefault(t *testing.T) {
	_, _, e := run(t, "", "retract", "-h")
	for _, want := range []string{"-last", "latest message sent from this machine by any session on it", "this session's own last message (the default)"} {
		if !strings.Contains(e, want) {
			t.Errorf("missing %q in:\n%s", want, e)
		}
	}
}
