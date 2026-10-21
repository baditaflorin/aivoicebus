package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func ackBody(id, by string) string { return `{"id":"` + id + `","by":"` + by + `"}` }

func TestAmendClaimRoute(t *testing.T) {
	srv, s, ready := retractNode(t)
	post := func(tok, body string) (int, string) { return call(t, srv, tok, "POST", "/v1/ack", body) }
	orig := sendAs(t, s, rSender, "deploy at noon")
	reply := func(from string) msg.Message {
		m, err := s.Send(msg.Message{From: from, To: orig.To, Re: orig.ID, Body: "deploy at 13:00 by " + from})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a := reply(rSender)

	t.Run("a session of the sender's machine claims it, and nodes then deliver the new text instead", func(t *testing.T) {
		if code, body := post(tokA, ackBody(msg.AmendPrefix+a.ID, "laptop-a/claude.c3d4")); code != 200 || !strings.Contains(body, `"ok":true`) {
			t.Fatalf("%d %s", code, body)
		}
		got := s.InboxFor("air/codex.q1", false, nil)
		if len(got) != 1 || got[0].ID != a.ID || got[0].Amends == nil || got[0].Amends.Of != orig.ID {
			t.Errorf("%+v", got)
		}
		_, body := call(t, srv, tokA, "GET", "/v1/inbox?me=air/codex.q2", "")
		if !strings.Contains(body, `"amends":{"of":"`+orig.ID+`"}`) {
			t.Errorf("the answer says what it replaces: %s", body)
		}
		_, body = call(t, srv, tokA, "GET", "/v1/thread?id="+orig.ID, "")
		if !strings.Contains(body, `"amended_by":{"by":"`+a.ID+`"`) {
			t.Errorf("and the original links forward: %s", body)
		}
		if code, _ := post(tokA, ackBody(msg.AmendPrefix+a.ID, rSender)); code != 200 {
			t.Errorf("asking again is not an error: %d", code)
		}
	})

	t.Run("it is no read receipt, whoever writes it", func(t *testing.T) {
		if n := s.Stats().Acks; n != 0 {
			t.Errorf("%d receipts", n)
		}
	})

	t.Run("another machine, another message, an unknown message and a malformed claim are refused", func(t *testing.T) {
		b := reply(rSender)
		if code, body := post(tokA, ackBody(msg.AmendPrefix+b.ID, rOther)); code != http.StatusForbidden || !strings.Contains(body, "can amend it") {
			t.Errorf("another machine: %d %s", code, body)
		}
		if code, _ := post(tokM, ackBody(msg.AmendPrefix+b.ID, rOther)); code != http.StatusForbidden {
			t.Errorf("a machine token acting as another machine: %d", code)
		}
		if code, _ := post(tokO, ackBody(msg.AmendPrefix+b.ID, rSender)); code != http.StatusForbidden {
			t.Errorf("the observer: %d", code)
		}
		if code, body := post(tokA, ackBody(msg.AmendPrefix+"0123456789", rSender)); code != http.StatusUnprocessableEntity || !strings.Contains(body, "does not hold that message") {
			t.Errorf("a message this node lacks (the client asks another node): %d %s", code, body)
		}
		if code, body := post(tokA, ackBody(msg.AmendPrefix+orig.ID, rSender)); code != http.StatusBadRequest || !strings.Contains(body, "does not replace") {
			t.Errorf("a message that answers nothing: %d %s", code, body)
		}
		for _, id := range []string{msg.AmendPrefix + "short", msg.AmendPrefix + "ABCDEF0123", msg.AmendPrefix + msg.AmendPrefix + a.ID, msg.AmendPrefix} {
			if code, _ := post(tokA, ackBody(id, rSender)); code != http.StatusBadRequest {
				t.Errorf("%q: %d", id, code)
			}
		}
	})

	t.Run("a node that is still recovering takes no claim", func(t *testing.T) {
		ready.Store(false)
		defer ready.Store(true)
		c := reply(rSender)
		if code, _ := post(tokA, ackBody(msg.AmendPrefix+c.ID, rSender)); code != http.StatusServiceUnavailable {
			t.Errorf("%d", code)
		}
	})
}

func TestSendRefusesToStoreWhatANodeSays(t *testing.T) {
	srv, s, _ := retractNode(t)
	code, body := call(t, srv, tokA, "POST", "/v1/send", `{"from":"`+rSender+`","to":"all","body":"x","amends":{"of":"0123456789","read":"0123456789"},"amended_by":{"by":"0123456789","at":"2026-10-21T04:00:00Z"}}`)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var m msg.Message
	if err := json.Unmarshal([]byte(body), &m); err != nil || m.Amends != nil || m.AmendedBy != nil {
		t.Errorf("a sender does not decide what a node says of its message: %+v %v", m, err)
	}
	if got := s.Thread(m.ID); len(got) != 1 || got[0].Amends != nil {
		t.Errorf("%+v", got)
	}
}

func TestNodesAdvertiseTheAmendFeature(t *testing.T) {
	srv, _, _ := retractNode(t)
	_, body := call(t, srv, tokA, "GET", "/v1/health", "")
	var h msg.Health
	if err := json.Unmarshal([]byte(body), &h); err != nil || !h.Has(msg.FeatureAmend) {
		t.Errorf("%v %s", err, body)
	}
}
