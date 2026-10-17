package cli

import (
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
