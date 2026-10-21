package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// approve signs the decision with the approver key, which needs its passphrase from a person at a
// terminal: without one, or with the wrong passphrase, nothing is sent.
func TestApproveNeedsThePassphraseAndSignsTheDecision(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", boss)
	if code, _, e := run(t, "", "approver", "init"); code == 0 || !strings.Contains(e, "needs a person at a terminal") {
		t.Fatalf("init without a terminal: %d %s", code, e) // the real prompt reads /dev/tty
	}
	answers := []string{}
	old := readPassphrase
	t.Cleanup(func() { readPassphrase = old })
	readPassphrase = func(string) (string, error) { a := answers[0]; answers = answers[1:]; return a, nil }
	answers = []string{"correct horse battery", "correct horse battery"}
	if code, out, e := run(t, "", "approver", "init"); code != 0 || !strings.Contains(out, "--approver-key ") {
		t.Fatalf("init: %d %s %s", code, out, e)
	}
	pub, _ := config.ApproverPublic()
	c, _ := client.FromEnv()
	req, _ := c.Send(msg.Message{ID: msg.NewID(), From: "lap/codex.worker", To: boss, Body: "[approval needed] the worker on lap would run ..."})
	answers = []string{"wrong passphrase!"}
	if code, _, e := run(t, "", "approve", req.ID); code == 0 || !strings.Contains(e, "wrong passphrase") {
		t.Fatalf("wrong passphrase: %d %s", code, e)
	}
	answers = []string{"correct horse battery"}
	if code, _, e := run(t, "", "approve", req.ID); code != 0 {
		t.Fatalf("approve: %d %s", code, e)
	}
	ms, _ := c.Thread(req.ID)
	last := ms[len(ms)-1]
	if word, ok := msg.CheckApproval(last.Body, req.ID, "lap/codex.worker", pub); !ok || word != "approve" || last.From != boss {
		t.Fatalf("the decision is not signed for that worker: %q %v %q", word, ok, last.Body)
	}
	if _, ok := msg.CheckApproval(last.Body, req.ID, "other/codex.worker", pub); ok {
		t.Error("a decision for one worker counts for another")
	}
	plain, _ := c.Send(msg.Message{ID: msg.NewID(), From: "lap/x", To: boss, Body: "hello"})
	answers = []string{"correct horse battery"}
	if code, _, _ := run(t, "", "approve", plain.ID); code != exitUsage {
		t.Error("approve signed something that is not an approval request")
	}
}

// A request that names the approver key (every request from this release on) is checked the same
// way by the sender and the node: the person's `approve`, signed with that key, goes through; against
// a request that names another key, the same signature is refused instead of sent.
func TestApproveAgainstARequestThatNamesTheKey(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", boss)
	answers := []string{"correct horse battery", "correct horse battery"}
	old := readPassphrase
	t.Cleanup(func() { readPassphrase = old })
	readPassphrase = func(string) (string, error) { a := answers[0]; answers = answers[1:]; return a, nil }
	if code, _, e := run(t, "", "approver", "init"); code != 0 {
		t.Fatalf("init: %d %s", code, e)
	}
	pub, _ := config.ApproverPublic()
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	c, _ := client.FromEnv()
	req := func(key ed25519.PublicKey) msg.Message {
		m, err := c.Send(msg.Message{ID: msg.NewID(), From: "lap/codex.worker", To: msg.HumanOf(boss), Prio: msg.PrioUrgent,
			Body: "[approval needed] the worker on lap would run this task:\nwrite the file\n" + msg.ApproverKeyLine(key)})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	good, bad := req(pub), req(other)
	answers = []string{"correct horse battery"}
	if code, _, e := run(t, "", "approve", good.ID); code != 0 {
		t.Fatalf("approve a request that names this key: %d %s", code, e)
	}
	ms, err := c.Thread(good.ID)
	if err != nil || len(ms) < 2 {
		t.Fatalf("the thread of the approved request: %d messages, %v", len(ms), err)
	}
	if last := ms[len(ms)-1]; last.From != boss || !strings.HasPrefix(last.Body, "approve\n[approver-sig v1 ") {
		t.Errorf("the decision was not stored: %+v", last)
	}
	answers = []string{"correct horse battery"}
	if code, _, e := run(t, "", "approve", bad.ID); code == 0 || !strings.Contains(e, "does not verify") {
		t.Errorf("approve against a request that names another key: %d %s", code, e)
	}
	if ms, err := c.Thread(bad.ID); err != nil || len(ms) != 1 {
		t.Errorf("a decision the key cannot have made was stored, or the thread could not be read: %d messages, %v", len(ms), err)
	}
}
