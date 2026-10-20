package client_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// What nodes of every release answered (their raw text, before msg.HeaderRefused), and what this
// client makes of each: a reason a script can act on, and words with a next step.
var oldRefusals = []struct {
	code   int
	text   string
	reason string
	want   string // in the explanation
}{
	{401, "unauthorized", msg.RefusedToken, "aivoicebus doctor"},
	{403, "this token may only act as laptop-b/...", msg.RefusedDenied, "shared token"},
	{403, "the observer token is read-only and cannot do this", msg.RefusedReadOnly, "shared token"},
	{403, "a machine token (laptop-b/...) may not read this; use the shared or observer token", msg.RefusedReadOnly, "shared token"},
	{403, "refused: this node takes only signed messages (serve --require-signed), and this one is unsigned", msg.RefusedUnsigned, "aivoicebus update"},
	{403, "only a session on laptop-b, the machine that sent this message, can retract it", msg.RefusedNotSender, "Ask its sender"},
	// seen live on 2026-10-18 from a 0.10 node, raw
	{409, "the conversation reached its turn limit or deadline", msg.RefusedTurns, "aivoicebus reply --end --re"},
	{409, "the conversation reached its turn limit or deadline: its deadline passed at 2026-10-18T10:00:00Z: close it with `reply --end`", msg.RefusedDeadline, "aivoicebus send"},
	{409, "the conversation reached its turn limit or deadline: this sender said the same thing 3 times in this thread already (numbers and wording aside): it looks like a loop", msg.RefusedLoop, "ask the user"},
	{409, "the conversation reached its turn limit or deadline: this message is 57 characters and the conversation has 28 left of its character budget of 80 (--budget-chars): say it in fewer words", msg.RefusedBudget, "this message is 57 characters, and 28 of its 80 are left"},
	{422, "this message looks like it contains a GitHub token. Messages are kept in clear text", msg.RefusedSecret, "GitHub token"},
	{422, "this node does not hold that message (it may not have reached it yet, or the id is wrong)", msg.RefusedUnknown, "aivoicebus inbox --all"},
	{429, "rate limit: 60 messages an hour per sender is the most", msg.RefusedRateSender, "Try again"},
	{429, "rate limit: 300 messages an hour from one machine is the most", msg.RefusedRateMachine, "Try again"},
	{429, "rate limit: 24 messages between the same two sessions in 2m0s is the most", msg.RefusedRatePair, "ask the user"},
	{429, "rate limit: 24 replies between the same two sessions in 2m0s, and this one is 80% the same words as one this sender sent before (the pair brake)", msg.RefusedRatePair, "Say what is new"},
	{429, "rate limit: 48 replies between the same two sessions in 2m0s is the most, even when each says something new (the pair brake)", msg.RefusedRatePair, "Slow down"},
	{429, "rate limit: 60 retractions an hour per sender is the most", msg.RefusedRateRetract, "Try again"},
	{400, "bad request", msg.RefusedBadRequest, "aivoicebus update"},
	{400, "bad request: that id is a retraction notice, not a message: there is nothing to reply to", msg.RefusedNotice, "aivoicebus inbox --all"},
	{400, "bad request: that message id is reserved for a retraction notice; leave the id out", msg.RefusedReservedID, "aivoicebus send"},
	{413, "the file is larger than a node takes (7 MiB before encryption)", msg.RefusedTooLarge, "smaller file"},
	{404, "404 page not found", "", "update the nodes"},
	{418, "something no release says", "", "aivoicebus doctor"},
}

func TestRefusalsSayWhatToDo(t *testing.T) {
	for _, r := range oldRefusals {
		e := &client.StatusError{Code: r.code, Status: http.StatusText(r.code), Text: r.text}
		got := e.Error()
		if e.Refused() != r.reason {
			t.Errorf("%d %q: reason %q, want %q", r.code, r.text, e.Refused(), r.reason)
		}
		if !view.HasNextStep(got) || !strings.Contains(got, r.want) {
			t.Errorf("%d %q:\n  says %q\n  want a next step with %q", r.code, r.text, got, r.want)
		}
	}
	// a reason named by a newer node wins over the text, and every reason says what to do
	for _, reason := range []string{msg.RefusedToken, msg.RefusedDenied, msg.RefusedReadOnly, msg.RefusedUnsigned, msg.RefusedSecret,
		msg.RefusedTurns, msg.RefusedDeadline, msg.RefusedBudget, msg.RefusedLoop, msg.RefusedRateSender, msg.RefusedRateMachine, msg.RefusedRatePair,
		msg.RefusedRateRetract, msg.RefusedUnknown, msg.RefusedNotSender, msg.RefusedReservedID, msg.RefusedNotice,
		msg.RefusedBadRequest, msg.RefusedTooLarge, msg.RefusedGone, msg.RefusedRecovering, msg.RefusedShareFull, msg.RefusedConflict,
		msg.RefusedShareMissing, "from-a-future-release"} {
		e := &client.StatusError{Code: 400, Status: "400 Bad Request", Text: "words", Reason: reason}
		if got := e.Error(); !view.HasNextStep(got) || strings.Contains(got, "\n") {
			t.Errorf("reason %s: %q has no next step", reason, got)
		}
	}
}

// The reason and the wait reach the error from the node's headers.
func TestRefusalReadsTheNodesReasonAndWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(msg.HeaderRefused, msg.RefusedRateMachine)
		w.Header().Set("Retry-After", "600")
		http.Error(w, "whatever this node says", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := (&client.Client{URLs: []string{srv.URL}, Token: "t"}).Thread("0123456789")
	var se *client.StatusError
	if !errors.As(err, &se) || se.Refused() != msg.RefusedRateMachine {
		t.Fatalf("%v", err)
	}
	if got := err.Error(); !strings.Contains(got, "in 10 minutes") || !strings.Contains(got, "this machine") || strings.Contains(got, "whatever") {
		t.Fatalf("%q", got)
	}
}

func TestNoNodeSaysWhatToDo(t *testing.T) {
	_, err := (&client.Client{URLs: []string{"http://127.0.0.1:1"}, Token: "t"}).Thread("0123456789")
	if err == nil || !client.IsUnreachable(err) || !view.HasNextStep(err.Error()) {
		t.Fatalf("%v", err)
	}
	if !view.HasNextStep(client.ErrNoURL.Error()) {
		t.Fatal(client.ErrNoURL)
	}
}
