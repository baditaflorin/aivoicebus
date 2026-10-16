package client_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

const sender = "laptop/claude.a1b2"

// oldNode answers as a release before 0.7.0 does: no /v1/retract (404), and /v1/ack stores
// whatever id it is given. It remembers each ack it took.
type oldNode struct {
	*httptest.Server
	mu   sync.Mutex
	acks []map[string]any
	hits atomic.Int64
}

func newOldNode(t *testing.T) *oldNode {
	t.Helper()
	n := &oldNode{}
	n.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.hits.Add(1)
		switch r.URL.Path {
		case "/v1/ack":
			var a map[string]any
			if json.NewDecoder(r.Body).Decode(&a) != nil {
				http.Error(w, "bad request", 400)
				return
			}
			n.mu.Lock()
			n.acks = append(n.acks, a)
			n.mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(n.Close)
	return n
}

func (n *oldNode) taken() []map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]map[string]any(nil), n.acks...)
}

func holding(t *testing.T, name string) (*httptest.Server, *store.Store, msg.Message) {
	t.Helper()
	srv, s := node(t, name, nil)
	m, err := s.Send(msg.Message{From: sender, To: "all", Body: "a message to take back"})
	if err != nil {
		t.Fatal(err)
	}
	return srv, s, m
}

func TestRetractWritesToTheNodeAndHidesTheMessage(t *testing.T) {
	srv, s, m := holding(t, "a")
	warn := &bytes.Buffer{}
	c := &client.Client{URLs: []string{srv.URL}, Token: "t", Warn: warn}
	res, err := c.Retract(m.ID, "laptop/claude.c3d4", false, false)
	if err != nil || !res.OK || res.State != msg.RetractRetracted || res.Sender != sender {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(warn.String(), "only 1 node(s) took the write") {
		t.Errorf("one node taking it is a success that says so: %q", warn.String())
	}
	if got := s.InboxFor("air/codex.q1", false, nil); len(got) != 0 {
		t.Errorf("%+v", got)
	}
	again, err := c.Retract(m.ID, sender, true, false)
	if err != nil || again.State != msg.RetractEscalated {
		t.Errorf("%+v %v", again, err)
	}
	dry, err := c.Retract(m.ID, sender, true, true)
	if err != nil || dry.State != msg.RetractDryRun || dry.Would != msg.RetractAlready {
		t.Errorf("%+v %v", dry, err)
	}
}

func TestRetractAsksTheNextNodeWhenOneDoesNotHoldTheMessageYet(t *testing.T) {
	empty, emptyStore := node(t, "a", nil)
	with, withStore, m := holding(t, "b")
	warn := &bytes.Buffer{}
	c := &client.Client{URLs: []string{empty.URL, with.URL}, Token: "t", Warn: warn}
	res, err := c.Retract(m.ID, sender, false, false)
	if err != nil || res.State != msg.RetractRetracted {
		t.Fatalf("%+v %v", res, err)
	}
	if got := withStore.Thread(m.ID)[0]; got.Retracted == nil {
		t.Error("the node that holds it applied it")
	}
	if n := emptyStore.VV()["a"]; n != 0 {
		t.Errorf("a node never records a retraction for a message it has not got: %d records", n)
	}
	if !strings.Contains(warn.String(), "only 1 node(s)") {
		t.Errorf("only one node took it, and it says so: %q", warn.String())
	}
}

func TestRetractRepeatsTheLookupForAMessageStillReplicating(t *testing.T) {
	var hits atomic.Int64
	var held *store.Store
	srv, s := node(t, "a", func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/retract" && hits.Add(1) == 3 { // the message arrives between the second and the third try
				held.Merge([]msg.Record{{Origin: "peer", Seq: 1, Msg: &msg.Message{ID: "00aa11bb22", From: sender, To: "all", Thread: "00aa11bb22", Body: "late", At: time.Now().UTC()}}})
			}
			inner.ServeHTTP(w, r)
		})
	})
	held = s
	c := &client.Client{URLs: []string{srv.URL}, Token: "t", HedgeDelay: time.Millisecond, Warn: &bytes.Buffer{}}
	res, err := c.Retract("00aa11bb22", sender, false, false)
	if err != nil || res.State != msg.RetractRetracted || hits.Load() != 3 {
		t.Fatalf("%+v %v after %d tries", res, err, hits.Load())
	}
}

func TestRetractGivesUpOnAMessageNoNodeHoldsAndSaysSo(t *testing.T) {
	var hits atomic.Int64
	a, _ := node(t, "a", counting(&hits))
	b, _ := node(t, "b", nil)
	c := &client.Client{URLs: []string{a.URL, b.URL}, Token: "t", HedgeDelay: time.Millisecond, Warn: &bytes.Buffer{}}
	start := time.Now()
	_, err := c.Retract("00aa11bb22", sender, false, false)
	var se *client.StatusError
	if !errors.As(err, &se) || se.Code != 422 || strings.Contains(err.Error(), "no node reachable") {
		t.Fatalf("a typo or a message no node has is a 422, not an outage: %v", err)
	}
	if hits.Load() != 4 {
		t.Errorf("the first try and three more: %d", hits.Load())
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("a short wait, not a long one: %v", time.Since(start))
	}
}

func TestRetractFallsBackToTheAckAnOldNodeTakes(t *testing.T) {
	old1, old2 := newOldNode(t), newOldNode(t)
	warn := &bytes.Buffer{}
	c := &client.Client{URLs: []string{old1.URL, old2.URL}, Token: "t", Warn: warn}
	res, err := c.Retract("3f2a9c01de", "laptop/claude.c3d4", false, false)
	if err != nil || !res.OK || res.State != msg.RetractRecorded || res.ReadBy != -1 {
		t.Fatalf("%+v %v", res, err)
	}
	for i, n := range []*oldNode{old1, old2} {
		got := n.taken()
		if len(got) != 1 || got[0]["id"] != "retract:3f2a9c01de" || got[0]["by"] != "laptop/claude.c3d4" || got[0]["via"] != nil || got[0]["chars"] != nil {
			t.Errorf("node %d took %v: the carrier is an ordinary ack, id retract:<id>, no receipt", i, got)
		}
	}
	if warn.Len() != 0 {
		t.Errorf("two nodes took it: %q", warn.String())
	}

	leak, err := c.Retract("3f2a9c01de", sender, true, false)
	if err != nil || leak.State != msg.RetractRecorded {
		t.Fatal(err)
	}
	if got := old1.taken(); len(got) != 2 || got[1]["id"] != "retract-leak:3f2a9c01de" {
		t.Errorf("the credential flag is in the id, so it survives a node that cannot read it: %v", got)
	}
}

func TestRetractOnAMeshWithOneNodeThatKnowsItNeedsNoFallback(t *testing.T) {
	old1, old2 := newOldNode(t), newOldNode(t)
	srv, s, m := holding(t, "n")
	warn := &bytes.Buffer{}
	c := &client.Client{URLs: []string{old1.URL, old2.URL, srv.URL}, Token: "t", Warn: warn}
	res, err := c.Retract(m.ID, sender, false, false)
	if err != nil || res.State != msg.RetractRetracted || res.ReadBy < 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(old1.taken())+len(old2.taken()) != 0 {
		t.Error("the node that applies it took it, and it replicates: the old ones are not written")
	}
	if !strings.Contains(warn.String(), "only 1 node(s)") {
		t.Errorf("%q", warn.String())
	}
	if s.Thread(m.ID)[0].Retracted == nil {
		t.Error("applied")
	}
}

func TestRetractFallsBackWhenTheOnlyNewNodeLacksTheMessage(t *testing.T) {
	old := newOldNode(t)
	empty, emptyStore := node(t, "n", nil)
	warn := &bytes.Buffer{}
	c := &client.Client{URLs: []string{old.URL, empty.URL}, Token: "t", Warn: warn}
	res, err := c.Retract("3f2a9c01de", sender, false, false)
	if err != nil || res.State != msg.RetractRecorded {
		t.Fatalf("%+v %v", res, err)
	}
	if len(old.taken()) != 1 || emptyStore.VV()["n"] != 0 {
		t.Errorf("the old node took the plain ack and the new one, which does not hold the message, wrote nothing: %v %v", old.taken(), emptyStore.VV())
	}
	if !strings.Contains(warn.String(), "only 1 node(s)") {
		t.Errorf("%q", warn.String())
	}
}

func TestRetractRefusalsAreFinalAndAreNotRetried(t *testing.T) {
	var hits atomic.Int64
	srv, _, m := holding(t, "a")
	counted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); srv.Config.Handler.ServeHTTP(w, r) }))
	defer counted.Close()
	c := &client.Client{URLs: []string{counted.URL}, Token: "t", HedgeDelay: time.Millisecond}

	_, err := c.Retract(m.ID, "imac/claude.z", false, false)
	var se *client.StatusError
	if !errors.As(err, &se) || se.Code != 403 || !strings.Contains(err.Error(), "laptop") || hits.Load() != 1 {
		t.Errorf("another machine: %v after %d requests", err, hits.Load())
	}
	hits.Store(0)
	if _, err := c.Retract("retract:"+m.ID, sender, false, false); !errors.As(err, &se) || se.Code != 400 || hits.Load() != 1 {
		t.Errorf("a bad id: %v after %d", err, hits.Load())
	}
	hits.Store(0)
	wrong := &client.Client{URLs: []string{counted.URL}, Token: "wrong"}
	if _, err := wrong.Retract(m.ID, sender, false, false); !client.IsRejected(err) || hits.Load() != 1 {
		t.Errorf("a wrong token: %v after %d", err, hits.Load())
	}

	// a rate limit is final too
	a, as, am := holding(t, "x")
	as.SetRetractLimit(1)
	cc := &client.Client{URLs: []string{a.URL}, Token: "t", Warn: &bytes.Buffer{}}
	if _, err := cc.Retract(am.ID, sender, false, false); err != nil {
		t.Fatal(err)
	}
	other, _ := as.Send(msg.Message{From: sender, To: "all", Body: "another"})
	if _, err := cc.Retract(other.ID, sender, false, false); !errors.As(err, &se) || se.Code != 429 {
		t.Errorf("over the allowance: %v", err)
	}
}

func TestRetractDryRunAsksOneNodeAndWritesNothing(t *testing.T) {
	srv, s, m := holding(t, "a")
	other, _, _ := holding(t, "b")
	var hits atomic.Int64
	counted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); srv.Config.Handler.ServeHTTP(w, r) }))
	defer counted.Close()
	warn := &bytes.Buffer{}
	c := &client.Client{URLs: []string{counted.URL, other.URL}, Token: "t", Warn: warn, HedgeDelay: 5 * time.Second}
	res, err := c.Retract(m.ID, sender, false, true)
	if err != nil || res.State != msg.RetractDryRun || res.Would != msg.RetractRetracted {
		t.Fatalf("%+v %v", res, err)
	}
	if hits.Load() != 1 || s.VV()["a"] != 1 || warn.Len() != 0 {
		t.Errorf("one node, no write, no warning about a partial write: %d %v %q", hits.Load(), s.VV(), warn.String())
	}

	old := newOldNode(t)
	oc := &client.Client{URLs: []string{old.URL}, Token: "t"}
	res, err = oc.Retract(m.ID, sender, false, true)
	if err != nil || res.State != msg.RetractDryRun || res.ReadBy != -1 || res.Would != "" || len(old.taken()) != 0 {
		t.Errorf("an old node cannot say what would happen, and a dry run never writes: %+v %v %v", res, err, old.taken())
	}
}

func TestRetractSkipsANodeThatIsDown(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	srv, _, m := holding(t, "a")
	c := &client.Client{URLs: []string{deadURL, srv.URL}, Token: "t", Warn: &bytes.Buffer{}}
	if res, err := c.Retract(m.ID, sender, false, false); err != nil || res.State != msg.RetractRetracted {
		t.Errorf("%+v %v", res, err)
	}
	only := &client.Client{URLs: []string{deadURL}, Token: "t"}
	if _, err := only.Retract(m.ID, sender, false, false); err == nil || !strings.Contains(err.Error(), "no node reachable") {
		t.Errorf("%v", err)
	}
}
