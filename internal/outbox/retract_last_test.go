package outbox

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// sendAs sends through the checked path as identity from, so the memory of what was sent from
// this machine has it, as it does for a session that sent it for real. The process acts as from
// afterwards.
func sendAs(t *testing.T, c *client.Client, from, to, body string) msg.Message {
	t.Helper()
	t.Setenv("AIVOICEBUS_ME", from)
	p, err := Prepare(c, Request{To: to, Body: body}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Send(c)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWithNoIdOnlyThisSessionsOwnLastMessageIsTakenBack(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	theirs := sendAs(t, c, mePeer, reader1, "the other session's note")
	t.Setenv("AIVOICEBUS_ME", meID) // this session has sent nothing
	n := s.VV()["n1"]

	out, err := retractWith(t, c, "", RetractOptions{})
	if err == nil || out != "" {
		t.Fatalf("it refuses, and says it all in the error: %v %q", err, out)
	}
	has(t, err.Error(), "nothing sent as "+meID+" is remembered, so nothing was taken back",
		"last message from this machine: "+theirs.ID+" sent as "+mePeer+" to "+reader1+" at ", ` UTC: "the other session's note"`,
		"retract it with: aivoicebus retract "+theirs.ID, "--last")
	if s.VV()["n1"] != n || s.Thread(theirs.ID)[0].Retracted != nil || len(s.InboxFor(reader1, false, nil)) != 1 {
		t.Error("nothing is written: the other session's message stands")
	}

	// an agent is told to pass the id, and not about a flag it does not have
	_, err = retractWith(t, c, "", RetractOptions{Agent: true})
	if err == nil || !strings.Contains(err.Error(), "call aivoicebus_retract with id "+theirs.ID) || strings.Contains(err.Error(), "--last") || strings.Contains(err.Error(), "aivoicebus retract") {
		t.Errorf("%v", err)
	}

	// a dry run is always allowed, shows the candidate, and writes nothing
	out = mustWith(t, c, "", RetractOptions{DryRun: true})
	has(t, out, "last message from this machine: "+theirs.ID+" sent as "+mePeer, `"the other session's note"`,
		"note: a real run with no id would refuse: this session sent nothing of its own; pass the id or --last", "dry run: nothing was written", "would retract "+theirs.ID)
	out = mustWith(t, c, "", RetractOptions{DryRun: true, Agent: true})
	lacks(t, out, "--last")
	if s.VV()["n1"] != n {
		t.Error("a dry run writes nothing")
	}

	// --last is the person's choice to take the machine's latest
	out = mustWith(t, c, "", RetractOptions{Last: true})
	has(t, out, "last message from this machine: "+theirs.ID+" sent as "+mePeer, "note: retracting as "+meID+" a message sent as "+mePeer, "retracted "+theirs.ID)
	lacks(t, out, "a real run with no id would refuse")
	if r := s.Thread(theirs.ID)[0].Retracted; r == nil || r.By != meID {
		t.Errorf("%+v", r)
	}
}

func mustWith(t *testing.T, c *client.Client, id string, o RetractOptions) string {
	t.Helper()
	out, err := retractWith(t, c, id, o)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return out
}

func TestOwnLastMessageComesBeforeAMoreRecentOneOfAnotherSession(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	mine := sendAs(t, c, meID, reader1, "mine, older")
	theirs := sendAs(t, c, mePeer, reader1, "theirs, newer")
	t.Setenv("AIVOICEBUS_ME", meID)

	out := mustWith(t, c, "", RetractOptions{})
	has(t, out, "last message sent by this session: "+mine.ID+" to "+reader1+`: "mine, older"`, "retracted "+mine.ID)
	lacks(t, out, "sent as", "a real run")
	if s.Thread(theirs.ID)[0].Retracted != nil {
		t.Error("the other session's message is not touched")
	}

	// --last means the machine's latest, whoever sent it: the person at a terminal asks for that
	third := sendAs(t, c, meID, reader1, "mine again, newest of all")
	older := sendAs(t, c, mePeer, reader1, "theirs, in between")
	_ = third
	t.Setenv("AIVOICEBUS_ME", meID)
	out = mustWith(t, c, "", RetractOptions{Last: true, DryRun: true})
	has(t, out, "last message from this machine: "+older.ID+" sent as "+mePeer)
}

func TestLastTakesNoIdAndAnotherMachinesMessagesAreNeverTheCandidate(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	m := sendAs(t, c, meID, reader1, "mine")
	if _, err := retractWith(t, c, m.ID, RetractOptions{Last: true}); err == nil || !strings.Contains(err.Error(), "--last takes the machine's latest message: give it no id") {
		t.Errorf("%v", err)
	}
	if s.Stats().Retracted != 0 {
		t.Error("nothing is written")
	}
	// a message sent from another machine is not in this machine's memory at all
	sendAs(t, c, "imac/claude.7", reader1, "from the iMac")
	t.Setenv("AIVOICEBUS_ME", "mac2/claude.1")
	if _, err := retractWith(t, c, "", RetractOptions{Last: true}); err == nil || !strings.Contains(err.Error(), "give the message id") {
		t.Errorf("a machine that sent nothing has no last message, with or without --last: %v", err)
	}
}

func TestTheCandidateIsNamedWithoutItsTextWhenItLooksLikeACredential(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	secret := "gh" + "p_" + strings.Repeat("aB3dE", 8)
	// stored directly, as a message the node's own backstop would have refused; remembered as sent by the other session
	m := put(t, s, mePeer, reader1, "use "+zwsp(secret, 6)+" now")
	rememberSent(m)
	t.Setenv("AIVOICEBUS_ME", meID)

	_, err := retractWith(t, c, "", RetractOptions{})
	if err == nil {
		t.Fatal("refused")
	}
	has(t, err.Error(), "last message from this machine: "+m.ID+" sent as "+mePeer, withheld)
	out := mustWith(t, c, "", RetractOptions{DryRun: true})
	for _, f := range fragments(secret, 8) {
		lacks(t, err.Error(), f)
		lacks(t, out, f)
	}
	lacks(t, err.Error()+out, "use ")
}

func TestAnotherMachinesMessageIsNotRetractedThroughTheCandidate(t *testing.T) {
	// the memory says the latest message of this machine was sent as another session; the node holds
	// it as sent from somewhere else entirely (a stale or edited file): the ordinary refusal applies
	c, s := retractNode(t, meID, nil)
	m := put(t, s, elsewho, reader1, "not from here")
	rememberSent(msg.Message{ID: m.ID, From: mePeer, To: reader1})
	t.Setenv("AIVOICEBUS_ME", meID)
	_, err := retractWith(t, c, "", RetractOptions{Last: true})
	if err == nil || !strings.Contains(err.Error(), "only that machine can retract it") || s.Stats().Retracted != 0 {
		t.Errorf("%v", err)
	}
}

func TestThatAnEmptyIdIsNotTheLastMessageIsTheCallersJobAndRetractMeansNoneByIt(t *testing.T) {
	// Retract's own contract: "" is "no id". The CLI and the MCP tool reject an id argument that is
	// present and empty before they get here (see their tests), so a variable that was never set
	// cannot reach this path.
	c, _ := retractNode(t, meID, nil)
	if _, err := retractWith(t, c, "", RetractOptions{}); err == nil || !strings.Contains(err.Error(), "give the message id") {
		t.Errorf("%v", err)
	}
}

// zwsp puts a zero-width space after every n characters of s, as a hostile sender would to keep a
// token from matching a detector that looks at the raw text.
func zwsp(s string, n int) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%n == 0 {
			b.WriteString("\u200b")
		}
		b.WriteRune(r)
	}
	return b.String()
}

func TestOldNodeRecordedStateOnlyClaimsWhatIsKnown(t *testing.T) {
	m := msg.Message{ID: "3f2a9c01de", From: meID, To: reader1, Thread: "3f2a9c01de", Body: "held by an old node"}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_ME", meID)
	t.Chdir(t.TempDir())

	// the old node says a node that applies retractions exists in the mesh: it will apply the record when it holds the message
	o, url := startOldNode(t, m, "0.0.7", msg.PeerHealth{URL: "http://new.test:7777", OK: true, Version: "0.1.0"})
	c := &client.Client{URLs: []string{url}, Token: "tok", Warn: &bytes.Buffer{}}
	out := mustRetract(t, c, m.ID, false, false, "")
	has(t, out, "recorded the retraction of "+m.ID, "nodes that apply retractions will hide it once they hold the message", "node old1 (0.0.7) still delivers it")
	lacks(t, out, "no node of this mesh", "no node applies")
	if len(o.taken()) != 1 {
		t.Error("the plain ack was written")
	}

	// a node that applies retractions and does not hold the message yet answers 422 and an old one 404: the record is
	// written to the old one, and nothing says that no node applies retractions
	empty, err := store.Open(t.TempDir(), "n9")
	if err != nil {
		t.Fatal(err)
	}
	newSrv := httptest.NewServer(server.NewHandler(empty, "tok"))
	t.Cleanup(newSrv.Close)
	o2, oldURL := startOldNode(t, m, "0.0.7")
	c = &client.Client{URLs: []string{newSrv.URL, oldURL}, Token: "tok", Warn: &bytes.Buffer{}}
	out = mustRetract(t, c, m.ID, false, false, "")
	has(t, out, "recorded the retraction of "+m.ID, "as a plain ack", "nodes that apply retractions will hide it once they hold the message")
	lacks(t, out, "no node of this mesh", "no node applies")
	if got := o2.taken(); len(got) != 1 || got[0].ID != "retract:"+m.ID {
		t.Errorf("%+v", got)
	}
	if empty.VV()["n9"] != 0 {
		t.Error("the node that does not hold the message recorded nothing")
	}
}

func TestPeerTextIsOneLineAndBoundedInEveryError(t *testing.T) {
	// a node's answer is data: a sender "host" with an escape sequence and a thousand characters must
	// not redraw the terminal or fill it
	hostile := "x\x1b[2J" + strings.Repeat("h", 1000) + "/claude.1"
	m := msg.Message{ID: "3f2a9c01de", From: hostile, To: "to\x1b[31m" + strings.Repeat("t", 500), Thread: "3f2a9c01de", Body: "hello"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/thread" {
			json.NewEncoder(w).Encode([]msg.Message{m})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_ME", meID)
	t.Chdir(t.TempDir())
	c := &client.Client{URLs: []string{srv.URL}, Token: "tok", Warn: &bytes.Buffer{}}
	for _, as := range []string{"", "me/claude.dead"} {
		out, err := retract(t, c, m.ID, false, false, as)
		if err == nil || !strings.Contains(err.Error(), "only a session on that machine can retract it") && !strings.Contains(err.Error(), "only that machine can retract it") {
			t.Fatalf("%v", err)
		}
		all := out + err.Error()
		if strings.ContainsAny(all, "\x1b") || len([]rune(all)) > 700 || !strings.Contains(all, "…") {
			t.Errorf("as=%q: bounded, on one line, no escape: %d %q", as, len([]rune(all)), all)
		}
	}
}
