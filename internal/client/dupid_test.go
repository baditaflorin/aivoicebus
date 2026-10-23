package client_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// seeded is a node that already holds a message of somebody else's under id.
func seeded(t *testing.T, id string, wrap func(http.Handler) http.Handler) (*client.Client, *store.Store) {
	t.Helper()
	srv, s := node(t, "a", wrap)
	if _, err := s.Send(msg.Message{ID: id, From: "desk/claude.c", To: "air/codex.d", Body: "somebody else's words", TTL: 300}); err != nil {
		t.Fatal(err)
	}
	return &client.Client{URLs: []string{srv.URL}, Token: "t", NoSign: true}, s
}

func TestSendDrawsAFreshIdWhenTheNodeHoldsAnotherMessageUnderIt(t *testing.T) {
	c, s := seeded(t, "00aa11bb22", nil)
	out, err := c.Send(msg.Message{ID: "00aa11bb22", From: "mac/claude.a", To: "air/codex.b", Body: "my words"})
	if err != nil {
		t.Fatalf("one collision is the sender's to fix, not the user's: %v", err)
	}
	if out.ID == "00aa11bb22" || !msg.ValidID(out.ID) || msg.IsTestID(out.ID) || out.From != "mac/claude.a" || out.Body != "my words" {
		t.Fatalf("what came back is my message under a new ordinary id: %+v", out)
	}
	if got := s.Thread(out.ID); len(got) != 1 || got[0].Body != "my words" {
		t.Errorf("the node holds my message: %+v", got)
	}
	if got := s.Thread("00aa11bb22"); len(got) != 1 || got[0].Body != "somebody else's words" {
		t.Errorf("and the other one is still the other one: %+v", got)
	}
}

func TestAMessageMarkedForTheSuccessorKeepsTheMarkUnderItsNewId(t *testing.T) {
	id := msg.NewSuccessorID()
	c, _ := seeded(t, id, nil)
	out, err := c.Send(msg.Message{ID: id, From: "mac/claude.asker", To: "mac/claude.7430", Body: "for the one that ended"})
	if err != nil || out.ID == id || !msg.IsSuccessorID(out.ID) || !out.ForSuccessor() {
		t.Fatalf("the mark is what lets the nodes hand it on: %+v %v", out, err)
	}
}

func TestAMessageThatNamesItselfAsItsThreadNamesItsNewId(t *testing.T) {
	c, s := seeded(t, "00aa11bb22", nil)
	out, err := c.Send(msg.Message{ID: "00aa11bb22", Thread: "00aa11bb22", From: "mac/claude.a", To: "air/codex.b", Body: "my words"})
	if err != nil || out.ID == "00aa11bb22" || out.Thread != out.ID {
		t.Fatalf("its thread is itself, not the other message's: %+v %v", out, err)
	}
	if got := s.Thread("00aa11bb22"); len(got) != 1 {
		t.Errorf("the other message's thread did not grow: %+v", got)
	}
}

func TestATestMessageStaysATestMessageUnderItsNewId(t *testing.T) {
	id := msg.NewTestID()
	c, s := seeded(t, id, nil)
	out, err := c.Send(msg.Message{ID: id, From: "mac/claude.a", To: "air/codex.b", Body: "calc 17*23", TTL: 300})
	if err != nil {
		t.Fatal(err)
	}
	if out.ID == id || !msg.IsTestID(out.ID) || !out.IsTest() {
		t.Fatalf("a test id is replaced by a test id, or the message would reach everyone it is addressed to: %+v", out)
	}
	if got := s.ThreadLane(out.ID, true); len(got) != 1 || got[0].Body != "calc 17*23" {
		t.Errorf("the node holds it in the lane: %+v", got)
	}
}

func TestTheNewIdIsSignedAgain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, _ := seeded(t, "00aa11bb22", nil)
	c.NoSign = false
	out, err := c.Send(msg.Message{ID: "00aa11bb22", From: "mac/claude.a", To: "air/codex.b", Body: "my words"})
	if err != nil {
		t.Fatal(err)
	}
	if signed, valid := out.VerifySig(); !signed || !valid {
		t.Fatalf("the signature covers the id, so the message under its new id is signed again: signed %v valid %v %+v", signed, valid, out)
	}
}

func TestAMessageSignedBeforeItWasSentIsSignedAgainForItsNewId(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	priv, box, err := config.MachineKeys()
	if err != nil {
		t.Fatal(err)
	}
	c, _ := seeded(t, "00aa11bb22", nil)
	c.NoSign = false
	m := msg.Message{ID: "00aa11bb22", From: "mac/claude.a", To: "air/codex.b", Body: "my words"}
	m.Sign(priv, msg.BoxPublic(box)) // the signature of the id that is taken
	out, err := c.Send(m)
	if err != nil || out.ID == "00aa11bb22" {
		t.Fatalf("%+v %v", out, err)
	}
	if signed, valid := out.VerifySig(); !signed || !valid {
		t.Errorf("the old id's signature must not travel with the new id: signed %v valid %v", signed, valid)
	}
}

// What a node of a release before this one does with a held id: it answers 200 with the message it holds.
func oldNodeSaysOK(held msg.Message, requests *[]msg.Message, mu *sync.Mutex) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/send" {
				next.ServeHTTP(w, r)
				return
			}
			b, _ := io.ReadAll(r.Body)
			var m msg.Message
			_ = json.Unmarshal(b, &m)
			mu.Lock()
			*requests = append(*requests, m)
			mu.Unlock()
			if m.ID == held.ID {
				_ = json.NewEncoder(w).Encode(held)
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(b)))
			next.ServeHTTP(w, r)
		})
	}
}

func TestAnOldNodesAnswerWithAnotherMessageIsNotTakenForSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	held := msg.Message{ID: "00aa11bb22", From: "desk/claude.c", To: "air/codex.d", Thread: "00aa11bb22", Body: "somebody else's words"}
	var requests []msg.Message
	var mu sync.Mutex
	srv, s := node(t, "old", oldNodeSaysOK(held, &requests, &mu))
	c := &client.Client{URLs: []string{srv.URL}, Token: "t"}
	out, err := c.Send(msg.Message{ID: held.ID, From: "mac/claude.a", To: "air/codex.b", Body: "my words"})
	if err != nil || out.ID == held.ID || out.Body != "my words" {
		t.Fatalf("the node said ok with somebody else's message: that is a collision, and the second try is mine: %+v %v", out, err)
	}
	if len(requests) != 2 || requests[0].ID != held.ID || requests[1].ID != out.ID {
		t.Fatalf("one retry, under the new id: %+v", requests)
	}
	if signed, valid := requests[1].VerifySig(); !signed || !valid {
		t.Errorf("signed again for the new id: %v %v", signed, valid)
	}
	if got := s.Thread(out.ID); len(got) != 1 || got[0].Body != "my words" {
		t.Errorf("%+v", got)
	}
}

func TestOnlyOneRetryAndThenTheRefusalIsExplained(t *testing.T) {
	var posts int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posts++
		mu.Unlock()
		w.Header().Set(msg.HeaderRefused, msg.RefusedIDTaken)
		http.Error(w, "taken", http.StatusConflict)
	}))
	t.Cleanup(srv.Close)
	c := &client.Client{URLs: []string{srv.URL}, Token: "t", NoSign: true}
	_, err := c.Send(msg.Message{ID: "00aa11bb22", From: "mac/claude.a", To: "air/codex.b", Body: "my words"})
	var refused *client.StatusError
	if !errors.As(err, &refused) || refused.Refused() != msg.RefusedIDTaken {
		t.Fatalf("a second refusal is the answer: %v", err)
	}
	if !strings.Contains(err.Error(), "aivoicebus send") || strings.Contains(err.Error(), "\n") {
		t.Errorf("it says what to do in one line: %q", err)
	}
	if posts != 2 {
		t.Errorf("one retry, not a loop: %d requests", posts)
	}
}

func TestAnOldNodeThatAnswersWithAnotherMessageTwiceIsAnError(t *testing.T) {
	held := msg.Message{ID: "00aa11bb22", From: "desk/claude.c", To: "air/codex.d", Body: "somebody else's words"}
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		held.ID = func() string { b, _ := io.ReadAll(r.Body); var m msg.Message; _ = json.Unmarshal(b, &m); return m.ID }()
		_ = json.NewEncoder(w).Encode(held) // always somebody else's, under whatever id was asked
	}))
	t.Cleanup(srv.Close)
	c := &client.Client{URLs: []string{srv.URL}, Token: "t", NoSign: true}
	if out, err := c.Send(msg.Message{ID: "00aa11bb22", From: "mac/claude.a", To: "air/codex.b", Body: "my words"}); err == nil {
		t.Fatalf("told it was sent when it was not: %+v", out)
	} else if !strings.Contains(err.Error(), "aivoicebus send") {
		t.Errorf("%q", err)
	}
	if posts != 2 {
		t.Errorf("%d requests", posts)
	}
}

func TestOnlyACollisionIsRedrawn(t *testing.T) {
	t.Run("a retry of the same message is not one", func(t *testing.T) {
		c, s := seeded(t, "00aa11bb22", nil)
		m := msg.Message{ID: "00aa11bb23", From: "mac/claude.a", To: "air/codex.b", Body: "my words"}
		first, err := c.Send(m)
		if err != nil || first.ID != m.ID {
			t.Fatalf("%+v %v", first, err)
		}
		again, err := c.Send(m)
		if err != nil || again.ID != m.ID {
			t.Fatalf("the same message again keeps its id: %+v %v", again, err)
		}
		if got := s.Thread(m.ID); len(got) != 1 {
			t.Errorf("%+v", got)
		}
	})
	t.Run("the same words within the node's window come back as the earlier message", func(t *testing.T) {
		c, _ := seeded(t, "00aa11bb22", nil)
		first, _ := c.Send(msg.Message{ID: msg.NewID(), From: "mac/claude.a", To: "air/codex.b", Body: "my words"})
		again, err := c.Send(msg.Message{ID: msg.NewID(), From: "mac/claude.a", To: "air/codex.b", Body: "my words"})
		if err != nil || again.ID != first.ID {
			t.Fatalf("a repeat of the words is the node's dedupe, not an id collision: %+v %v", again, err)
		}
	})
	t.Run("a message that was retracted comes back withdrawn and is not sent again", func(t *testing.T) {
		c, s := seeded(t, "00aa11bb22", nil)
		m := msg.Message{ID: "00aa11bb23", From: "mac/claude.a", To: "air/codex.b", Body: "withdrawn words"}
		if _, err := c.Send(m); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Retract(m.ID, m.From, false); err != nil {
			t.Fatal(err)
		}
		out, err := c.Send(m)
		if err != nil || out.ID != m.ID || out.Retracted == nil {
			t.Fatalf("a withdrawn message stays withdrawn: %+v %v", out, err)
		}
	})
}

// Only an answer under the id that was sent can be a collision: a node that answers with some other
// message under another id has done something else, and what it said is what Send returns.
func TestAnAnswerUnderAnotherIdIsNotACollision(t *testing.T) {
	var posts int
	other := msg.Message{ID: "ffeeddccbb", From: "desk/claude.c", To: "air/codex.d", Body: "somebody else's words"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		_ = json.NewEncoder(w).Encode(other)
	}))
	t.Cleanup(srv.Close)
	c := &client.Client{URLs: []string{srv.URL}, Token: "t", NoSign: true}
	out, err := c.Send(msg.Message{ID: "00aa11bb22", From: "mac/claude.a", To: "air/codex.b", Body: "my words"})
	if err != nil || out.ID != other.ID || posts != 1 {
		t.Fatalf("%+v %v after %d requests", out, err, posts)
	}
}
