package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// post sends m as the shared token would, and returns the status, the body and the refusal word.
func post(t *testing.T, srv *httptest.Server, m msg.Message) (int, string, string) {
	t.Helper()
	b, _ := json.Marshal(m)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/send", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+tokA)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b2, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b2), resp.Header.Get(msg.HeaderRefused)
}

// approvalNode holds one approval request, as a write-mode worker sends it (from lap/codex.worker
// to the person on desk), naming the approver key when withKey.
func approvalNode(t *testing.T, withKey bool) (srv *httptest.Server, s *store.Store, req msg.Message, priv ed25519.PrivateKey) {
	t.Helper()
	srv, s, _ = retractNode(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body := msg.ApprovalTag + " the worker on lap would run this task from desk/claude.a1b2:\nwrite the file\n\nA person approves in a terminal."
	if withKey {
		body += "\n" + msg.ApproverKeyLine(pub)
	}
	req, err := s.Send(msg.Message{From: "lap/codex.worker", To: msg.HumanOf("desk/claude.a1b2"), Prio: msg.PrioUrgent, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return srv, s, req, priv
}

func signed(priv ed25519.PrivateKey, req msg.Message, word string) string {
	return msg.ApprovalBody(word, ed25519.Sign(priv, msg.ApprovalStatement(req.ID, word, req.From)))
}

// reply is an agent's or a person's reply in the request's thread, to the request or to a reply.
func reply(req msg.Message, re, body string) msg.Message {
	return msg.Message{From: "desk/claude.a1b2", To: req.From, Re: re, Thread: req.Thread, Body: body}
}

func TestANodeStoresOnlyASignedDecisionInAnApprovalThread(t *testing.T) {
	srv, s, req, priv := approvalNode(t, true)
	before := s.Stats().Messages
	refused := func(name, body string) {
		t.Helper()
		code, text, word := post(t, srv, reply(req, req.ID, body))
		if code != http.StatusForbidden || word != msg.RefusedApproval {
			t.Errorf("%s: %d %q %q", name, code, word, text)
		}
		for _, want := range []string{"aivoicebus approve " + req.ID, "own terminal", "Nothing was sent"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: the refusal lacks %q: %s", name, want, text)
			}
		}
	}
	refused("a bare approve", "approve")
	refused("a bare deny", "deny")
	refused("approved in words", "Approved, go ahead")
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	refused("a signature by another key", signed(otherPriv, req, "approve"))
	refused("a signature for another word", strings.Replace(signed(priv, req, "deny"), "deny", "approve", 1))
	if s.Stats().Messages != before {
		t.Errorf("a refused reply was stored: %d messages, %d before", s.Stats().Messages, before)
	}

	// the person's decision is stored, whichever word
	for _, word := range []string{"approve", "deny"} {
		if code, text, _ := post(t, srv, reply(req, req.ID, signed(priv, req, word))); code != 200 {
			t.Errorf("a signed %s: %d %s", word, code, text)
		}
	}
	// and an ordinary reply is none of the guard's business
	if code, text, _ := post(t, srv, reply(req, req.ID, "which repository is this?")); code != 200 {
		t.Errorf("a question: %d %s", code, text)
	}
	if _, body := call(t, srv, tokA, "GET", "/metrics", ""); !strings.Contains(body, "aivoicebus_approvals_refused_total 5") {
		t.Errorf("the metric does not count the refusals:\n%s", body)
	}
}

// A reply to a reply in the thread counts as well: the worker takes a decision from anywhere in it.
func TestAReplyToAReplyInAnApprovalThreadIsGuardedToo(t *testing.T) {
	srv, _, req, priv := approvalNode(t, true)
	code, text, _ := post(t, srv, reply(req, req.ID, "which repository is this?"))
	var first msg.Message
	if code != 200 || json.Unmarshal([]byte(text), &first) != nil {
		t.Fatalf("%d %s", code, text)
	}
	if code, text, word := post(t, srv, reply(req, first.ID, "approve")); code != http.StatusForbidden || word != msg.RefusedApproval {
		t.Errorf("a bare approve to a reply: %d %q %s", code, word, text)
	}
	if code, text, _ := post(t, srv, reply(req, first.ID, signed(priv, req, "approve"))); code != 200 {
		t.Errorf("a signed approve to a reply: %d %s", code, text)
	}
}

// A request that names no key (a worker older than this release) is judged on its shape: the word
// alone is refused, and a signature line is left for the worker to verify.
func TestARequestWithoutAKeyRefusesTheBareWordOnly(t *testing.T) {
	srv, _, req, priv := approvalNode(t, false)
	if code, text, word := post(t, srv, reply(req, req.ID, "approve")); code != http.StatusForbidden || word != msg.RefusedApproval {
		t.Errorf("a bare approve: %d %q %s", code, word, text)
	}
	if code, text, _ := post(t, srv, reply(req, req.ID, signed(priv, req, "approve"))); code != 200 {
		t.Errorf("a signature the node cannot check is stored for the worker to check: %d %s", code, text)
	}
}

// Elsewhere the word is just a word, and the two authority shapes are kept out of every message.
func TestAnApproverSignatureAndAPipeLinkKeyAreRefusedOutsideTheirPlace(t *testing.T) {
	srv, s, req, priv := approvalNode(t, true)
	h := sha256.Sum256([]byte("a key"))
	link := "https://pipe.example.test/#rx3wiitC~" + hex.EncodeToString(h[:])
	plain := func(body string) msg.Message {
		return msg.Message{From: "desk/claude.a1b2", To: "lap/claude.c3d4", Body: body}
	}
	if code, text, _ := post(t, srv, plain("approve")); code != 200 {
		t.Errorf("the word in an ordinary message: %d %s", code, text)
	}
	before := s.Stats().Messages
	for name, body := range map[string]string{
		"a pasted pipe link": "LINK (5 minutes): " + link,
		"a signature":        signed(priv, req, "approve"),
	} {
		code, text, word := post(t, srv, plain(body))
		if code != http.StatusUnprocessableEntity || word != msg.RefusedSecret {
			t.Errorf("%s: %d %q %s", name, code, word, text)
		}
		want := "aivoicebus send --file"
		if strings.Contains(name, "signature") {
			want = "aivoicebus approve ID"
		}
		if !strings.Contains(text, want) {
			t.Errorf("%s: the refusal does not point to `%s`: %s", name, want, text)
		}
	}
	// a signed decision in some other thread than the request's is a signature in an ordinary message
	if code, _, word := post(t, srv, msg.Message{From: "desk/claude.a1b2", To: req.From, Body: signed(priv, req, "approve")}); code != http.StatusUnprocessableEntity || word != msg.RefusedSecret {
		t.Errorf("a signature sent outside the thread: %d %q", code, word)
	}
	if s.Stats().Messages != before {
		t.Error("a refused message was stored")
	}
	// the carrier send --file writes is the way to send a link
	if code, text, _ := post(t, srv, plain("the report\n[attach] "+link+" name=report.tgz size=10")); code != 200 {
		t.Errorf("an [attach] line: %d %s", code, text)
	}
	// a pipe link is refused in an approval thread as anywhere else: in a question, and next to a decision
	if code, _, word := post(t, srv, reply(req, req.ID, "which repository? "+link)); code != http.StatusUnprocessableEntity || word != msg.RefusedSecret {
		t.Errorf("a link in a reply: %d %q", code, word)
	}
	if code, _, _ := post(t, srv, reply(req, req.ID, signed(priv, req, "approve")+"\n"+link)); code == 200 {
		t.Error("a link next to a decision was stored")
	}
}
