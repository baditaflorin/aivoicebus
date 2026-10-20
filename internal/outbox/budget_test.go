package outbox

import (
	"io"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// --budget-chars goes only on the message that starts a conversation, only to nodes that enforce it.
func TestABudgetGoesOnlyToNodesThatEnforceIt(t *testing.T) {
	c, _ := retractNode(t, meID, &msg.Health{Node: "n1", Version: "0.11.5", Peers: []msg.PeerHealth{{URL: "http://p.test", OK: true, Version: msg.BudgetSince}}})
	_, err := Prepare(c, Request{To: reader1, Body: "a short talk", BudgetChars: 500}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "does not enforce --budget-chars") || !strings.Contains(err.Error(), "0.11.5") {
		t.Fatalf("an old node: %v", err)
	}
	c, _ = retractNode(t, meID, &msg.Health{Node: "n1", Version: msg.BudgetSince, Peers: []msg.PeerHealth{{URL: "http://p.test", OK: true, Version: msg.BudgetSince}}})
	p, err := Prepare(c, Request{To: reader1, Body: "a short talk", BudgetChars: 500}, io.Discard)
	if err != nil || p.Msg.BudgetChars != 500 {
		t.Fatalf("new nodes: %v %+v", err, p.Msg)
	}
	for _, r := range []Request{
		{To: reader1, Body: "a reply", Re: "0123456789", BudgetChars: 500},
		{To: reader1, Body: strings.Repeat("x", 20), BudgetChars: 10},
		{To: reader1, Body: "too big", BudgetChars: msg.MaxBudgetChars + 1},
	} {
		if _, err := Prepare(c, r, io.Discard); err == nil || !strings.Contains(err.Error(), "budget-chars") {
			t.Errorf("%+v: %v", r, err)
		}
	}
}
