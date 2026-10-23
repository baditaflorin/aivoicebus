package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestANodeRefusesAnIdAnotherMessageHoldsAndSaysWhy(t *testing.T) {
	srv, s, _ := retractNode(t)
	first := msg.Message{ID: "00aa11bb22", From: "mac/claude.a", To: "air/codex.b", Body: "first words"}
	if code, body, word := post(t, srv, first); code != http.StatusOK || word != "" {
		t.Fatalf("%d %s %q", code, body, word)
	}
	code, body, word := post(t, srv, msg.Message{ID: first.ID, From: "desk/claude.c", To: "air/codex.d", Body: "other words"})
	if code != http.StatusConflict || word != msg.RefusedIDTaken {
		t.Fatalf("another message under a held id is a 409 with its reason, got %d %q %q", code, word, body)
	}
	if strings.Contains(body, "first words") || strings.Contains(body, "mac/claude.a") {
		t.Errorf("the refusal must not hand the sender somebody else's message: %s", body)
	}
	if !strings.Contains(body, "send it again") {
		t.Errorf("an older client shows these words, so they say the next step: %s", body)
	}
	if got := s.Thread(first.ID); len(got) != 1 || got[0].Body != "first words" {
		t.Errorf("the held message is untouched and nothing else is stored: %+v", got)
	}
	if _, metrics := call(t, srv, tokA, "GET", "/metrics", ""); !strings.Contains(metrics, `aivoicebus_refused_total{reason="id_taken"} 1`) {
		t.Errorf("the refusal is on the node's metrics:\n%s", metrics)
	}
	// a retry of the first message is the same message: 200, and it is the held one
	var out msg.Message
	code, body, word = post(t, srv, first)
	if code != http.StatusOK || word != "" || json.Unmarshal([]byte(body), &out) != nil || out.ID != first.ID || out.Body != "first words" {
		t.Errorf("a retry stays idempotent: %d %q %s", code, word, body)
	}
}
