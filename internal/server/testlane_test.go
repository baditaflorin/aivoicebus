package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestTheNodeHidesTheTestLaneUnlessAReadAsksAndItsReaderOptedIn(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "t"))
	defer srv.Close()
	c := &client.Client{URLs: []string{srv.URL}, Token: "t", NoSign: true}

	if h, err := c.Health(); err != nil || !h.Has(msg.FeatureTestLane) {
		t.Fatalf("the node advertises the test lane: %+v %v", h.Features, err)
	}
	m, err := c.Send(msg.Message{ID: msg.NewTestID(), From: "imac/claude.a", To: "air/codex.b", Body: "calc 17*23", TTL: 300})
	if err != nil || !m.IsTest() {
		t.Fatalf("send: %+v %v", m, err)
	}
	optIn := msg.Labels{msg.TestLabel: msg.TestOn}
	read := func(lane bool, labels msg.Labels) (inbox, thread bool) {
		r := c.WithRaw(lane) // the node's own answer, with nothing dropped on this side
		r.Labels = labels
		ms, err := r.Inbox("air/codex.b", true)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range ms {
			inbox = inbox || x.ID == m.ID
		}
		th, err := r.Thread(m.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range th {
			thread = thread || x.ID == m.ID
		}
		return inbox, thread
	}
	for _, tc := range []struct {
		name       string
		lane       bool
		labels     msg.Labels
		inbox, thr bool
	}{
		{"a plain read", false, nil, false, false},
		{"opted in, not asking", false, optIn, false, false},
		{"asking, not opted in", true, nil, false, true}, // a thread read has no reader: it is the lane's alone
		{"asking and opted in", true, optIn, true, true},
	} {
		if in, th := read(tc.lane, tc.labels); in != tc.inbox || th != tc.thr {
			t.Errorf("%s: inbox shows it %v (want %v), thread shows it %v (want %v)", tc.name, in, tc.inbox, th, tc.thr)
		}
	}
	if h, _ := c.Health(); h.Messages != 0 || h.Acks != 0 {
		t.Errorf("health leaves the lane out: %+v", h)
	}
	// the sender cancels it: a retraction is allowed on the lane, and is not counted
	if res, err := c.Retract(m.ID, "imac/claude.a", false, false); err != nil || res.State != msg.RetractRetracted {
		t.Errorf("cancelling a test task: %+v %v", res, err)
	}
	if h, _ := c.Health(); h.Retracted != 0 {
		t.Errorf("health counts a retracted test message: %+v", h)
	}
	r := c.WithLane()
	r.Labels = optIn
	if th, _ := r.Thread(m.ID); len(th) != 1 || th[0].Retracted == nil || strings.Contains(th[0].Body, "calc") {
		t.Errorf("the lane's thread shows a tombstone: %+v", th)
	}
}

func TestTheFeedStillCarriesATestRecordSoReplicationKeepsItsNumbering(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "t"))
	defer srv.Close()
	c := &client.Client{URLs: []string{srv.URL}, Token: "t", NoSign: true}
	if _, err := c.Send(msg.Message{ID: msg.NewTestID(), From: "imac/claude.a", To: "air/codex.b", Body: "in the feed", TTL: 300}); err != nil {
		t.Fatal(err)
	}
	recs, err := c.Sync(map[string]uint64{})
	if err != nil || len(recs) != 1 || recs[0].Msg == nil || !recs[0].Msg.IsTest() || recs[0].Msg.Body != "in the feed" {
		t.Fatalf("a peer must be handed the record while it lives: %+v %v", recs, err)
	}
}
