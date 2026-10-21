package outbox

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// The sender checks a reply to an approval request before it asks a node: the word alone never
// decides, so it is not sent, and the refusal says who decides and how.
func TestAReplyToAnApprovalRequestMustBeASignedDecision(t *testing.T) {
	c, s := meshWith(t, "desk/claude.a1b2", map[string]string{"lap/codex.worker": ""})
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req, err := s.Send(msg.Message{From: "lap/codex.worker", To: msg.HumanOf("desk/claude.a1b2"), Prio: msg.PrioUrgent,
		Body: msg.ApprovalTag + " the worker on lap would run this task:\nwrite the file\n" + msg.ApproverKeyLine(pub)})
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(re, body string) error {
		_, err := Prepare(c, Request{Re: re, Body: body, TargetSaid: true}, io.Discard)
		return err
	}
	for name, body := range map[string]string{"a bare approve": "approve", "a bare deny": "deny", "approved, in words": "Approved, go ahead"} {
		err := prepare(req.ID, body)
		if err == nil {
			t.Errorf("%s was prepared", name)
			continue
		}
		for _, want := range []string{"refused:", "`aivoicebus approve " + req.ID + "`", "own terminal", "Nothing was sent"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal lacks %q: %v", name, want, err)
			}
		}
	}
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	if err := prepare(req.ID, msg.ApprovalBody("approve", ed25519.Sign(otherPriv, msg.ApprovalStatement(req.ID, "approve", req.From)))); err == nil {
		t.Error("a signature by another key was prepared")
	}
	// the person's decision (what `aivoicebus approve` sends) and ordinary words go through
	if err := prepare(req.ID, msg.ApprovalBody("approve", ed25519.Sign(priv, msg.ApprovalStatement(req.ID, "approve", req.From)))); err != nil {
		t.Errorf("a signed decision: %v", err)
	}
	if err := prepare(req.ID, "which repository is this?"); err != nil {
		t.Errorf("a question: %v", err)
	}
	// a reply to a reply in the thread is judged by the request at its root
	answer, err := s.Send(msg.Message{From: "desk/claude.zz", To: req.From, Re: req.ID, Thread: req.Thread, Body: "which repository is this?"})
	if err != nil {
		t.Fatal(err)
	}
	if err := prepare(answer.ID, "approve"); err == nil || !strings.Contains(err.Error(), "aivoicebus approve "+req.ID) {
		t.Errorf("a bare approve to a reply in the thread: %v", err)
	}
	// and nothing about an ordinary thread changes
	plain, _ := s.Send(msg.Message{From: "lap/codex.worker", To: "desk/claude.a1b2", Body: "ready?"})
	if err := prepare(plain.ID, "approve"); err != nil {
		t.Errorf("the word in an ordinary thread: %v", err)
	}
}

// A pipe link with its key and an approver signature stay out of a message's text; the [attach] line
// that `send --file` writes is where a link goes, and the worker withholds an answer that has one.
func TestSendRefusesAPipeLinkKeyAndASignatureAndPointsToTheRightWay(t *testing.T) {
	c, _ := meshWith(t, "desk/claude.a1b2", map[string]string{"lap/claude.c3d4": ""})
	h := sha256.Sum256([]byte("a key"))
	link := "https://pipe.example.test/#rx3wiitC~" + hex.EncodeToString(h[:])
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	sig := msg.ApprovalBody("approve", ed25519.Sign(priv, []byte("x")))
	for name, c2 := range map[string]struct{ body, want string }{
		"a pasted link":          {"LINK (5 minutes, no backslashes): " + link, "aivoicebus send --file"},
		"the link for a token":   {"here is the machine token: " + link, "aivoicebus token hand"},
		"a signature":            {sig, "aivoicebus approve ID"},
		"a signature with words": {"done, signed:\n" + sig, "aivoicebus approve ID"},
	} {
		_, err := Prepare(c, Request{To: "lap/claude.c3d4", Body: c2.body}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), c2.want) || !strings.Contains(err.Error(), "nothing was sent") {
			t.Errorf("%s: %v", name, err)
		}
		if kind, found := Credential(c2.body); !found || kind == "" {
			t.Errorf("%s: the worker would not withhold it: %q %v", name, kind, found)
		}
	}
	// a file's own carrier is not refused
	if _, err := Prepare(c, Request{To: "lap/claude.c3d4", Body: "the report\n[attach] " + link + " name=r.tgz", File: true}, io.Discard); err != nil {
		t.Errorf("an [attach] line: %v", err)
	}
	if _, err := Prepare(c, Request{To: "lap/claude.c3d4", Body: "see https://pipe.example.test/#rx3wiitC (the key goes by token hand)"}, io.Discard); err != nil {
		t.Errorf("a link without its key: %v", err)
	}
}
