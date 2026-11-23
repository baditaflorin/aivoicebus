package client_test

import (
	"io"
	"reflect"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// soundRig is a client of one node whose sounds are recorded, with a HOME of its own (the messages a
// machine already made a sound for are kept there).
func soundRig(t *testing.T, on bool) (*client.Client, *store.Store, *[]string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	srv, s := node(t, "a", nil)
	heard, restore := client.ListenForSounds(on)
	t.Cleanup(restore)
	return &client.Client{URLs: []string{srv.URL}, Token: "t", Warn: io.Discard}, s, heard
}

func wantHeard(t *testing.T, heard *[]string, want ...string) {
	t.Helper()
	if len(*heard) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(*heard, want) {
		t.Fatalf("heard %v, want %v", *heard, want)
	}
	*heard = nil
}

func TestASendThatWentOutMakesTheSentSoundOnce(t *testing.T) {
	c, _, heard := soundRig(t, true)
	if _, err := c.Send(msg.Message{ID: msg.NewID(), From: "imac/claude.a1", To: "air/codex", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	wantHeard(t, heard, "sent")

	dead := &client.Client{URLs: []string{"http://127.0.0.1:1"}, Token: "t", Warn: io.Discard}
	if _, err := dead.Send(msg.Message{ID: msg.NewID(), From: "imac/claude.a1", To: "air/codex", Body: "lost"}); err == nil {
		t.Fatal("no node to take it")
	}
	wantHeard(t, heard) // a send that did not go out is silent
}

func TestTheBusCheckingItselfIsSilent(t *testing.T) {
	c, _, heard := soundRig(t, true)
	// the throwaway probe `join` and `selftest` send, and a message in the test lane
	if _, err := c.Send(msg.Message{ID: msg.NewID(), From: "imac/" + msg.ProbeSenderAgent, To: "imac/" + msg.ProbeReaderAgent, Body: "probe", Prio: msg.PrioFYI, TTL: 60}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WithLane().Send(msg.Message{ID: msg.NewTestID(), From: "imac/claude.a1", To: "air/codex", Body: "lane", Prio: msg.PrioFYI, TTL: 60}); err != nil {
		t.Fatal(err)
	}
	wantHeard(t, heard)
	if _, err := c.Inbox("imac/"+msg.ProbeReaderAgent, false); err != nil { // the probe reader's look
		t.Fatal(err)
	}
	wantHeard(t, heard)
}

func TestARetractionThatTookMakesTheRetractedSoundButNotADryRun(t *testing.T) {
	c, _, heard := soundRig(t, true)
	sent, err := c.Send(msg.Message{ID: msg.NewID(), From: "imac/claude.a1", To: "air/codex", Body: "oops"})
	if err != nil {
		t.Fatal(err)
	}
	wantHeard(t, heard, "sent")
	if _, err := c.Retract(sent.ID, "imac/claude.a1", false, true); err != nil {
		t.Fatal(err)
	}
	wantHeard(t, heard)
	if res, err := c.Retract(sent.ID, "imac/claude.a1", false, false); err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	wantHeard(t, heard, "retracted")
	if _, err := c.Retract("0000000000", "imac/claude.a1", false, false); err == nil {
		t.Fatal("no such message")
	}
	wantHeard(t, heard)
}

func TestAnArrivalSoundsOncePerMessageOnThisMachine(t *testing.T) {
	c, s, heard := soundRig(t, true)
	if _, err := s.Send(msg.Message{From: "imac/claude.a1", To: "air/codex.z9", Body: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(msg.Message{From: "imac/claude.a1", To: "air/codex.z9", Body: "two"}); err != nil {
		t.Fatal(err)
	}
	if in, err := c.Inbox("air/codex.z9", false); err != nil || len(in) != 2 {
		t.Fatalf("%v %v", in, err)
	}
	wantHeard(t, heard, "received") // two messages, one sound
	// nobody acked them: the next read, and another session of this machine, hear nothing new
	if _, err := c.Inbox("air/codex.z9", false); err != nil {
		t.Fatal(err)
	}
	wantHeard(t, heard)
	if _, err := s.Send(msg.Message{From: "imac/claude.a1", To: "air/codex.z9", Body: "three", Prio: msg.PrioUrgent}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Inbox("air/codex.z9", false); err != nil {
		t.Fatal(err)
	}
	wantHeard(t, heard, "received!") // the new one, and it is urgent
}

func TestReadingEverythingOrYourOwnMessagesIsSilent(t *testing.T) {
	c, s, heard := soundRig(t, true)
	if _, err := s.Send(msg.Message{From: "imac/claude.a1", To: "air/codex.z9", Body: "old news"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Inbox("air/codex.z9", true); err != nil { // `inbox --all` is a look at history
		t.Fatal(err)
	}
	wantHeard(t, heard)
	if _, err := c.Inbox("imac/claude.a1", false); err != nil { // the sender's own inbox: nothing for it
		t.Fatal(err)
	}
	wantHeard(t, heard)
}

func TestAWaitThatReturnsMailMakesTheArrivalSound(t *testing.T) {
	c, s, heard := soundRig(t, true)
	if _, err := s.Send(msg.Message{From: "imac/claude.a1", To: "air/codex.z9", Body: "wake up"}); err != nil {
		t.Fatal(err)
	}
	ms, _, err := c.InboxWait(t.Context(), "air/codex.z9", 0, false, 0)
	if err != nil || len(ms) != 1 {
		t.Fatalf("%v %v", ms, err)
	}
	wantHeard(t, heard, "received")
}

func TestWithSoundsOffNothingIsRecordedOrHeard(t *testing.T) {
	c, s, heard := soundRig(t, false)
	if _, err := s.Send(msg.Message{From: "imac/claude.a1", To: "air/codex.z9", Body: "quiet please"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Inbox("air/codex.z9", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(msg.Message{ID: msg.NewID(), From: "air/codex.z9", To: "imac/claude.a1", Body: "ok"}); err != nil {
		t.Fatal(err)
	}
	wantHeard(t, heard)
}
