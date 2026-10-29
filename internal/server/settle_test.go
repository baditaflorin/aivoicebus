package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestSettleClaimRoute(t *testing.T) {
	srv, s, _ := retractNode(t)
	post := func(tok, body string) (int, string) { return call(t, srv, tok, "POST", "/v1/ack", body) }
	m, err := s.Send(msg.Message{From: rOther, To: "laptop-a", Kind: msg.KindTask, Body: "collect the history"})
	if err != nil {
		t.Fatal(err)
	}
	if _, body := call(t, srv, tokA, "GET", "/v1/health", ""); !strings.Contains(body, `"`+msg.FeatureSettle+`"`) {
		t.Errorf("the node advertises settles: %s", body)
	}
	if code, body := post(tokA, ackBody(msg.SettlePrefix+m.ID, "laptop-a/claude.a1")); code != http.StatusBadRequest || !strings.Contains(body, "aivoicebus done") {
		t.Errorf("an agent identity is no person: %d %s", code, body)
	}
	if got := s.InboxFor("laptop-a/codex.q1", false, nil); len(got) != 1 {
		t.Fatalf("a refused settle hid nothing: %+v", got)
	}
	if code, body := post(tokM, ackBody(msg.SettlePrefix+m.ID, "imac/human")); code != http.StatusForbidden {
		t.Errorf("laptop-a's token is not imac's person: %d %s", code, body)
	}
	if code, body := post(tokA, ackBody(msg.SettlePrefix+m.ID, "laptop-a/human")); code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("the person settles: %d %s", code, body)
	}
	if got := s.InboxFor("laptop-a/codex.new", false, nil); len(got) != 0 {
		t.Errorf("a new session of the machine is shown it: %+v", got)
	}
	if n := s.Stats().Acks; n != 0 {
		t.Errorf("a settle is no read receipt: %d", n)
	}
	for name, id := range map[string]string{"not an id": "settle:zzz", "unknown message": msg.SettlePrefix + "0123456789"} {
		if code, _ := post(tokA, ackBody(id, "laptop-a/human")); code == 200 {
			t.Errorf("%s was taken", name)
		}
	}
	if code, _ := post(tokA, ackBody(msg.SettlePrefix+"0123456789", "laptop-a/human")); code != http.StatusUnprocessableEntity {
		t.Errorf("a message another node may hold is asked about elsewhere: %d", code)
	}
	ap, _ := s.Send(msg.Message{From: rOther, To: "laptop-a/human", Kind: msg.KindTask, Body: msg.ApprovalTag + " run it?"})
	if code, body := post(tokA, ackBody(msg.SettlePrefix+ap.ID, "laptop-a/human")); code != http.StatusBadRequest || !strings.Contains(body, "must be answered") {
		t.Errorf("an approval request is never settled: %d %s", code, body)
	}
}
