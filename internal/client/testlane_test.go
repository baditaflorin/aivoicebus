package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// aNodeThatPredatesTheLane serves a test message to every read, as a node that does not know the lane does, and
// remembers the query of each request.
func aNodeThatPredatesTheLane(t *testing.T, features []string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var queries []string
	test := msg.Message{ID: msg.NewTestID(), From: "a/b", To: "c/d", Body: "a test message", TTL: 300}
	plain := msg.Message{ID: msg.NewID(), From: "a/b", To: "c/d", Body: "an ordinary message"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RequestURI())
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(msg.Health{Node: "old", Version: "0.12.0", Features: features})
		default:
			_ = json.NewEncoder(w).Encode([]msg.Message{test, plain})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), queries...) }
}

func TestAReadThatDidNotAskForTheLaneDropsTestMessagesEvenFromANodeThatServesThem(t *testing.T) {
	srv, queries := aNodeThatPredatesTheLane(t, nil)
	c := &client.Client{URLs: []string{srv.URL}, Token: "t"}
	reads := map[string]func() ([]msg.Message, error){
		"inbox": func() ([]msg.Message, error) { return c.Inbox("c/d", false) },
		"peek":  func() ([]msg.Message, error) { return c.Peek("c/d") },
		"thread": func() ([]msg.Message, error) {
			return c.Thread("0123456789")
		},
	}
	for name, read := range reads {
		ms, err := read()
		if err != nil || len(ms) != 1 || ms[0].Body != "an ordinary message" {
			t.Errorf("%s: a hook or display must never get the test message: %v %+v", name, err, ms)
		}
	}
	for _, q := range queries() {
		if strings.Contains(q, "lane=") {
			t.Errorf("a read that does not ask for the lane must not say so: %s", q)
		}
	}
	// a client that asks for the lane keeps them, and says so to the node
	lc := c.WithLane()
	if ms, err := lc.Inbox("c/d", false); err != nil || len(ms) != 2 {
		t.Errorf("a lane read keeps both: %v %d", err, len(ms))
	}
	if ms, err := lc.Thread("0123456789"); err != nil || len(ms) != 2 {
		t.Errorf("a lane thread read keeps both: %v %d", err, len(ms))
	}
	var laneQueries int
	for _, q := range queries() {
		if strings.Contains(q, "lane=test") {
			laneQueries++
		}
	}
	if laneQueries != 2 {
		t.Errorf("the lane reads carry lane=test: %v", queries())
	}
	// Raw keeps whatever a node serves, for checking the node
	if ms, err := c.WithRaw(false).Inbox("c/d", false); err != nil || len(ms) != 2 {
		t.Errorf("a raw read keeps everything: %v %d", err, len(ms))
	}
}

func TestTestLaneNamesTheNodeThatLacksIt(t *testing.T) {
	good, _ := aNodeThatPredatesTheLane(t, []string{msg.FeatureTestLane, msg.FeatureTasks})
	old, _ := aNodeThatPredatesTheLane(t, []string{msg.FeatureTasks})
	c := &client.Client{URLs: []string{good.URL, old.URL}, Token: "t"}
	if which, err := c.TestLane(); err != nil || !strings.Contains(which, "old") {
		t.Errorf("one node lacks it and is named: %q %v", which, err)
	}
	c = &client.Client{URLs: []string{good.URL}, Token: "t"}
	if which, err := c.TestLane(); err != nil || which != "" {
		t.Errorf("every node has it: %q %v", which, err)
	}
	c = &client.Client{URLs: []string{"http://127.0.0.1:1"}, Token: "t"}
	if _, err := c.TestLane(); err == nil {
		t.Error("with no node answering nothing can be said")
	}
}
