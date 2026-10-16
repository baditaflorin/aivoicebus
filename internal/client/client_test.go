package client_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestSyncAllTakesTheUnionSoALaggingNodeCannotEndAnExportEarly(t *testing.T) {
	a, sa := node(t, "a", nil)
	b, sb := node(t, "b", nil)
	for i := 0; i < 5; i++ {
		sa.Send(msg.Message{From: "x/y", To: "all", Body: fmt.Sprint("from a ", i), Prio: msg.PrioFYI})
	}
	for i := 0; i < 3; i++ {
		sb.Send(msg.Message{From: "x/z", To: "all", Body: fmt.Sprint("from b ", i), Prio: msg.PrioFYI})
	}
	c := &client.Client{URLs: []string{b.URL, a.URL}, Token: "t"} // the node with less answers first
	recs, err := c.SyncAll(map[string]uint64{})
	if err != nil || len(recs) != 8 {
		t.Fatalf("%d records, %v", len(recs), err)
	}
	for i := 1; i < len(recs); i++ {
		p, r := recs[i-1], recs[i]
		if p.Origin > r.Origin || (p.Origin == r.Origin && p.Seq >= r.Seq) {
			t.Fatalf("not ordered by origin and sequence at %d: %+v then %+v", i, p, r)
		}
	}
	// beyond what is already held, only the rest
	recs, _ = c.SyncAll(map[string]uint64{"a": 4, "b": 3})
	if len(recs) != 1 || recs[0].Origin != "a" || recs[0].Seq != 5 {
		t.Errorf("%+v", recs)
	}
}

func TestARefusalDoesNotCancelAWriteThatIsStillInFlight(t *testing.T) {
	var h, ca atomic.Int64
	a, sa := node(t, "a", nil)
	b, sb := node(t, "b", delayed(120*time.Millisecond, &h, &ca))
	sa.SetSendLimit(1)
	if _, err := sa.Send(msg.Message{From: "loop/agent", To: "all", Body: "used up this node's allowance"}); err != nil {
		t.Fatal(err)
	}
	var warn strings.Builder
	c := &client.Client{URLs: []string{a.URL, b.URL}, Token: "t", Warn: &warn}
	out, err := c.Send(msg.Message{ID: msg.NewID(), From: "loop/agent", To: "all", Body: "stored by the node that did not refuse"})
	if err != nil {
		t.Fatalf("node b stores it; reporting node a's refusal would claim a message on the board was not sent: %v", err)
	}
	if got := sb.Thread(out.ID); len(got) != 1 {
		t.Errorf("node b should hold the message: %d", len(got))
	}
	if !strings.Contains(warn.String(), "only 1 node") {
		t.Errorf("it is a partial write, and says so: %q", warn.String())
	}
}

// A cancelled hedged attempt may still be mid-response when the call returns. It must not
// touch the client afterwards: the caller reads Server and Latest without a lock.
func TestAHedgedLoserNeverWritesTheClientAfterTheCallReturns(t *testing.T) {
	var h, ca atomic.Int64
	a, _ := node(t, "a", delayed(3*time.Millisecond, &h, &ca))
	b, _ := node(t, "b", nil)
	c := &client.Client{URLs: []string{a.URL, b.URL}, Token: "t", HedgeDelay: time.Millisecond}
	for i := 0; i < 60; i++ {
		if _, err := c.Inbox("x/y", false); err != nil {
			t.Fatal(err)
		}
		_ = c.Server.Version // read without a lock, as callers do; -race flags a late write
		_ = c.Latest
	}
}

func TestAHungNodeAmongTheFirstTwoDoesNotHoldAWriteForItsWholeTimeout(t *testing.T) {
	var h, ca atomic.Int64
	a, _ := node(t, "a", delayed(30*time.Second, &h, &ca)) // answers never, within any timeout here
	b, sb := node(t, "b", nil)
	d, sd := node(t, "d", nil)
	c := &client.Client{URLs: []string{a.URL, b.URL, d.URL}, Token: "t", Timeout: 20 * time.Second, HedgeDelay: 50 * time.Millisecond}
	start := time.Now()
	out, err := c.Send(msg.Message{ID: msg.NewID(), From: "x/y", To: "all", Body: "written around a hung node"})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the write waited for the hung node: %v", took)
	}
	if len(sb.Thread(out.ID)) != 1 || len(sd.Thread(out.ID)) != 1 {
		t.Errorf("the quorum is the two nodes that answered: b=%d d=%d", len(sb.Thread(out.ID)), len(sd.Thread(out.ID)))
	}
}

func TestAFastWriteIsNeverHedged(t *testing.T) {
	var ha, hb, hd atomic.Int64
	a, _ := node(t, "a", counting(&ha))
	b, _ := node(t, "b", counting(&hb))
	d, _ := node(t, "d", counting(&hd))
	c := &client.Client{URLs: []string{a.URL, b.URL, d.URL}, Token: "t", HedgeDelay: 2 * time.Second}
	if _, err := c.Send(msg.Message{ID: msg.NewID(), From: "x/y", To: "all", Body: "quick"}); err != nil {
		t.Fatal(err)
	}
	if ha.Load() != 1 || hb.Load() != 1 || hd.Load() != 0 {
		t.Errorf("two nodes are written, the third is left alone: %d %d %d", ha.Load(), hb.Load(), hd.Load())
	}
}

func TestBadNodeURLFailsOverInsteadOfPanicking(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "a")
	srv := httptest.NewServer(server.NewHandler(s, "t"))
	defer srv.Close()

	c := &client.Client{URLs: []string{"http://bad host:1", srv.URL}, Token: "t"}
	if _, err := c.Who(); err != nil {
		t.Fatal(err)
	}
	c = &client.Client{URLs: []string{"http://bad host:1"}, Token: "t"}
	if _, err := c.Who(); err == nil {
		t.Fatal("expected error")
	}
}

func TestClientErrorsDoNotFailOver(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "a")
	srv := httptest.NewServer(server.NewHandler(s, "right"))
	defer srv.Close()
	c := &client.Client{URLs: []string{srv.URL}, Token: "wrong"}
	_, err := c.Who()
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401, got %v", err)
	}
}

func TestARefusedTokenIsToldApartFromNodesNobodyCanReach(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "a")
	srv := httptest.NewServer(server.NewHandler(s, "right"))
	defer srv.Close()

	if info, err := (&client.Client{URLs: []string{srv.URL}, Token: "right"}).TokenInfo(); err != nil || info.Host != "" || info.Observer {
		t.Errorf("a shared token is accepted and unrestricted: %+v %v", info, err)
	}
	_, err := (&client.Client{URLs: []string{srv.URL}, Token: "wrong"}).TokenInfo()
	if !client.IsRejected(err) || client.IsNotFound(err) {
		t.Errorf("a 401 is a rejected token: %v", err)
	}
	_, err = (&client.Client{URLs: []string{"http://127.0.0.1:1"}, Token: "right"}).TokenInfo()
	if err == nil || client.IsRejected(err) {
		t.Errorf("a node nobody reaches is not a rejection: %v", err)
	}

	// a machine token asking for another machine's inbox is refused with a 403: rejected as well
	ts, err := server.ParseTokens("shared-token-aaaaaaaaaaaaaaaaaaaaaaaa\nlaptop-a:machine-token-mmmmmmmmmmmmmmmmmmmmmm\n")
	if err != nil {
		t.Fatal(err)
	}
	m := httptest.NewServer(server.NewHandler(s, "", server.WithTokenSet(ts)))
	defer m.Close()
	_, err = (&client.Client{URLs: []string{m.URL}, Token: "machine-token-mmmmmmmmmmmmmmmmmmmmmm"}).Inbox("imac/claude", false)
	if !client.IsRejected(err) {
		t.Errorf("a 403 is a rejection too: %v", err)
	}
	// while 4xx answers that are not about the token are not
	if client.IsRejected(&client.StatusError{Code: 400}) || client.IsRejected(&client.StatusError{Code: 429}) || client.IsRejected(nil) {
		t.Error("only 401 and 403 are rejections")
	}
}

func TestTokenInfoOnANodeFromBeforeTheEndpointIsNotFound(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer old.Close()
	_, err := (&client.Client{URLs: []string{old.URL}, Token: "t"}).TokenInfo()
	if !client.IsNotFound(err) {
		t.Errorf("callers fall back to the roster on this: %v", err)
	}
}

func TestSendInboxAckThread(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "a")
	srv := httptest.NewServer(server.NewHandler(s, "t"))
	defer srv.Close()
	var warn strings.Builder
	c := &client.Client{URLs: []string{srv.URL}, Token: "t", Warn: &warn}

	sent, err := c.Send(msg.Message{ID: msg.NewID(), From: "imac/claude", To: "air/codex", Body: "hi"})
	if err != nil || sent.Thread == "" {
		t.Fatalf("send: %v %+v", err, sent)
	}
	if !strings.Contains(warn.String(), "only 1 node(s)") {
		t.Fatalf("a single-node write should warn, got %q", warn.String())
	}
	in, err := c.Inbox("air/codex", false)
	if err != nil || len(in) != 1 || in[0].Body != "hi" {
		t.Fatalf("inbox: %v %+v", err, in)
	}
	if err := c.Ack(sent.ID, "air/codex"); err != nil {
		t.Fatal(err)
	}
	if in, _ = c.Inbox("air/codex", false); len(in) != 0 {
		t.Fatal("ack did not clear the inbox")
	}
	if all, _ := c.Inbox("air/codex", true); len(all) != 1 {
		t.Fatal("--all should still show the acked message")
	}
	if th, err := c.Thread(sent.ID); err != nil || len(th) != 1 {
		t.Fatalf("thread: %v %+v", err, th)
	}
}

// nodes starts n independent nodes (no replication between them).
func nodes(t *testing.T, n int) (stores []*store.Store, urls []string) {
	t.Helper()
	for i := 0; i < n; i++ {
		s, err := store.Open(t.TempDir(), fmt.Sprintf("n%d", i))
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(server.NewHandler(s, "t"))
		t.Cleanup(srv.Close)
		stores, urls = append(stores, s), append(urls, srv.URL)
	}
	return stores, urls
}

func TestARateLimitStopsASendInsteadOfShrinkingTheQuorum(t *testing.T) {
	stores, urls := nodes(t, 3)
	for _, s := range stores {
		s.SetSendLimit(1)
	}
	var warn strings.Builder
	c := &client.Client{URLs: urls, Token: "t", Warn: &warn}
	if _, err := c.Send(msg.Message{ID: msg.NewID(), From: "loop/agent", To: "all", Body: "one"}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Send(msg.Message{ID: msg.NewID(), From: "loop/agent", To: "all", Body: "two"})
	var refused *client.StatusError
	if !errors.As(err, &refused) || refused.Code != 429 || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("the send must fail with the node's 429, got %v", err)
	}
	if n := len(stores[2].Inbox("z/z", true)); n != 0 {
		t.Errorf("the third node took %d message(s) the first two refused, which would defeat the limit", n)
	}
	if warn.Len() != 0 {
		t.Errorf("a refusal is an error, not a partial-write warning: %q", warn.String())
	}
}

func TestRosterAndWhoMergeWhatEveryNodeKnows(t *testing.T) {
	stores, urls := nodes(t, 2)
	// each session reads from just one node, as happens when clients list the nodes in different orders
	stores[0].InboxFor("mac/claude.a", false, msg.Labels{"repo": "x"})
	stores[1].InboxFor("air/codex.b", false, msg.Labels{"repo": "y"})
	c := &client.Client{URLs: urls, Token: "t"}

	roster, err := c.Roster()
	if err != nil || len(roster) != 2 || roster[0].Me != "air/codex.b" || roster[0].Labels["repo"] != "y" || roster[1].Labels["repo"] != "x" {
		t.Fatalf("roster should hold both sessions with their labels: %v %+v", err, roster)
	}
	who, err := c.Who()
	if err != nil || len(who) != 2 {
		t.Fatalf("who should hold both sessions: %v %v", err, who)
	}

	oneDown := &client.Client{URLs: []string{"http://127.0.0.1:1", urls[1]}, Token: "t"}
	if roster, err := oneDown.Roster(); err != nil || len(roster) != 1 || roster[0].Me != "air/codex.b" {
		t.Errorf("a node being down must not hide the others: %v %+v", err, roster)
	}
	allDown := &client.Client{URLs: []string{"http://127.0.0.1:1"}, Token: "t"}
	if _, err := allDown.Roster(); err == nil || !strings.Contains(err.Error(), "no node reachable") {
		t.Errorf("all nodes down: %v", err)
	}
	wrong := &client.Client{URLs: urls, Token: "nope"}
	if _, err := wrong.Roster(); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a bad token should say so: %v", err)
	}
}

func TestAckViaStoresTheReceiptAndAnObserverClientCanOnlyWatch(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "a")
	srv := httptest.NewServer(server.NewHandler(s, "agent-token", server.WithObserverToken("observer-token-0123456789abcdef")))
	defer srv.Close()
	agent := &client.Client{URLs: []string{srv.URL}, Token: "agent-token", Warn: &strings.Builder{}}
	sent, _ := agent.Send(msg.Message{ID: msg.NewID(), From: "imac/claude", To: "all", Body: "hi"})
	if err := agent.AckVia(sent.ID, "air/codex", msg.ViaHook, 250); err != nil {
		t.Fatal(err)
	}

	watcher := &client.Client{URLs: []string{srv.URL}, Token: "observer-token-0123456789abcdef"}
	recs, err := watcher.Sync(map[string]uint64{})
	if err != nil || len(recs) != 2 {
		t.Fatalf("the observer should read the feed: %v %+v", err, recs)
	}
	var ack *msg.Ack
	for _, r := range recs {
		if r.Ack != nil {
			ack = r.Ack
		}
	}
	if ack == nil || ack.Via != msg.ViaHook || ack.Chars != 250 {
		t.Errorf("receipt: %+v", ack)
	}
	if _, err := watcher.Roster(); err != nil {
		t.Errorf("roster: %v", err)
	}
	_, err = watcher.Send(msg.Message{ID: msg.NewID(), From: "evil/agent", To: "all", Body: "forged"})
	var refused *client.StatusError
	if !errors.As(err, &refused) || refused.Code != 403 {
		t.Errorf("an observer must not send: %v", err)
	}
}

// node starts a real node behind a wrapper that can delay or count what it sees.
func node(t *testing.T, name string, wrap func(http.Handler) http.Handler) (*httptest.Server, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir(), name)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler = server.NewHandler(s, "t")
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, s
}

// delayed holds every request for d, or until the client gives up on it.
func delayed(d time.Duration, hits, cancelled *atomic.Int64) func(http.Handler) http.Handler {
	return func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			// Reading the body is what lets the server notice a client that hung up; keep it for the node.
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			select {
			case <-time.After(d):
				inner.ServeHTTP(w, r)
			case <-r.Context().Done():
				cancelled.Add(1)
			}
		})
	}
}

func counting(hits *atomic.Int64) func(http.Handler) http.Handler {
	return func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); inner.ServeHTTP(w, r) })
	}
}

func TestAReadIsAnsweredByTheNextNodeWhenTheFirstHangs(t *testing.T) {
	var hits, cancelled atomic.Int64
	slow, _ := node(t, "slow", delayed(3*time.Second, &hits, &cancelled))
	fast, fs := node(t, "fast", nil)
	fs.Send(msg.Message{From: "a/b", To: "x/y", Body: "from the fast node"})
	c := &client.Client{URLs: []string{slow.URL, fast.URL}, Token: "t", Timeout: 5 * time.Second}

	start := time.Now()
	got, err := c.Inbox("x/y", false)
	took := time.Since(start)
	if err != nil || len(got) != 1 || got[0].Body != "from the fast node" {
		t.Fatalf("%v %+v", err, got)
	}
	if took > 1500*time.Millisecond {
		t.Errorf("one hung node cost the read %v; it should cost about the hedge delay (250ms), not its timeout", took)
	}
	deadline := time.Now().Add(2 * time.Second)
	for cancelled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cancelled.Load() != 1 {
		t.Errorf("the slow node's request should be cancelled once the fast one answered: %d", cancelled.Load())
	}
}

func TestAFastFirstNodeIsNeverHedged(t *testing.T) {
	var second atomic.Int64
	a, _ := node(t, "a", nil)
	b, _ := node(t, "b", counting(&second))
	c := &client.Client{URLs: []string{a.URL, b.URL}, Token: "t"}
	for i := 0; i < 20; i++ {
		if _, err := c.Inbox("x/y", false); err != nil {
			t.Fatal(err)
		}
	}
	if second.Load() != 0 {
		t.Errorf("a node that answers at once must not cost the others any load: %d requests", second.Load())
	}
}

func TestADeadFirstNodeIsReplacedImmediatelyNotAfterTheHedgeDelay(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	b, _ := node(t, "b", nil)
	c := &client.Client{URLs: []string{deadURL, b.URL}, Token: "t", HedgeDelay: 5 * time.Second}
	start := time.Now()
	if _, err := c.Inbox("x/y", false); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("a refused connection should fail over at once, took %v", took)
	}
}

func TestAHedgedReadStillStopsAtADefinitiveRefusal(t *testing.T) {
	var second atomic.Int64
	a, _ := node(t, "a", nil)
	b, _ := node(t, "b", counting(&second))
	c := &client.Client{URLs: []string{a.URL, b.URL}, Token: "wrong", HedgeDelay: 5 * time.Second}
	_, err := c.Inbox("x/y", false)
	var refused *client.StatusError
	if !errors.As(err, &refused) || refused.Code != 401 {
		t.Fatalf("a bad token is a final answer: %v", err)
	}
	if second.Load() != 0 {
		t.Errorf("and is not retried on another node: %d", second.Load())
	}
}

func TestOnlyTheWinningNodesAnswerIsReturned(t *testing.T) {
	var hits, cancelled atomic.Int64
	a, as := node(t, "a", delayed(400*time.Millisecond, &hits, &cancelled))
	b, bs := node(t, "b", nil)
	am, _ := as.Send(msg.Message{From: "a/b", To: "x/y", Body: "A's thread"})
	bm, _ := bs.Send(msg.Message{From: "a/b", To: "x/y", Body: "B's thread", ID: am.ID}) // the same id on both nodes
	_ = bm
	c := &client.Client{URLs: []string{a.URL, b.URL}, Token: "t", Timeout: 5 * time.Second}
	got, err := c.Thread(am.ID)
	if err != nil || len(got) != 1 || got[0].Body != "B's thread" {
		t.Fatalf("the answer must be one node's, whole: %v %+v", err, got)
	}
}

func TestWritesGoToTwoNodesInOneRoundTrip(t *testing.T) {
	var h1, c1, h2, c2 atomic.Int64
	a, as := node(t, "a", delayed(300*time.Millisecond, &h1, &c1))
	b, bs := node(t, "b", delayed(300*time.Millisecond, &h2, &c2))
	third, ts := node(t, "c", nil)
	warn := &bytes.Buffer{}
	c := &client.Client{URLs: []string{a.URL, b.URL, third.URL}, Token: "t", Warn: warn, Timeout: 5 * time.Second, HedgeDelay: 2 * time.Second}

	start := time.Now()
	out, err := c.Send(msg.Message{ID: msg.NewID(), From: "a/b", To: "all", Body: "in parallel"})
	took := time.Since(start)
	if err != nil || out.ID == "" || warn.Len() != 0 {
		t.Fatalf("%v %+v %q", err, out, warn.String())
	}
	if took > 550*time.Millisecond {
		t.Errorf("two 300ms nodes written one after the other would take 600ms; in parallel about 300ms, took %v", took)
	}
	if len(as.Inbox("z/z", true)) != 1 || len(bs.Inbox("z/z", true)) != 1 || len(ts.Inbox("z/z", true)) != 0 {
		t.Error("the write should have reached exactly the first two nodes")
	}
}

func TestTheWriteQuorumIsConfigurableAndAFailedNodeIsReplaced(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	a, as := node(t, "a", nil)
	b, bs := node(t, "b", nil)
	d, ds := node(t, "d", nil)
	e, es := node(t, "e", nil)
	warn := &bytes.Buffer{}
	c := &client.Client{URLs: []string{deadURL, a.URL, b.URL, d.URL, e.URL}, Token: "t", Warn: warn, WriteQuorum: 3}
	if _, err := c.Send(msg.Message{ID: msg.NewID(), From: "a/b", To: "all", Body: "three copies"}); err != nil {
		t.Fatal(err)
	}
	have := len(as.Inbox("z/z", true)) + len(bs.Inbox("z/z", true)) + len(ds.Inbox("z/z", true)) + len(es.Inbox("z/z", true))
	if have != 3 || warn.Len() != 0 {
		t.Errorf("a dead node is replaced by the next, until 3 nodes hold it: %d copies, warning %q", have, warn.String())
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", a.URL+","+b.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "t")
	t.Setenv("AIVOICEBUS_WRITE_QUORUM", "1")
	t.Setenv("AIVOICEBUS_HEDGE_MS", "40")
	env, err := client.FromEnv()
	if err != nil || env.WriteQuorum != 1 || env.HedgeDelay != 40*time.Millisecond {
		t.Errorf("settings from the environment: %v %+v", err, env)
	}
}

func TestWithFewerNodesThanTheQuorumAWriteSucceedsAndSaysSo(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	a, as := node(t, "a", nil)
	warn := &bytes.Buffer{}
	c := &client.Client{URLs: []string{deadURL, a.URL}, Token: "t", Warn: warn}
	if _, err := c.Send(msg.Message{ID: msg.NewID(), From: "a/b", To: "all", Body: "one node left"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "only 1 node(s) took the write") || len(as.Inbox("z/z", true)) != 1 {
		t.Errorf("%q", warn.String())
	}
	allDead := &client.Client{URLs: []string{deadURL}, Token: "t"}
	if _, err := allDead.Send(msg.Message{ID: msg.NewID(), From: "a/b", To: "all", Body: "x"}); err == nil || !strings.Contains(err.Error(), "no node reachable") {
		t.Errorf("no node at all: %v", err)
	}
}

func TestOneClientSharedByManyGoroutinesIsRaceFree(t *testing.T) {
	var h, ca atomic.Int64
	a, _ := node(t, "a", delayed(5*time.Millisecond, &h, &ca))
	b, _ := node(t, "b", nil)
	c := &client.Client{URLs: []string{a.URL, b.URL}, Token: "t", HedgeDelay: time.Millisecond}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.Inbox(fmt.Sprintf("x/y%d", i), false); err != nil {
				t.Error(err)
			}
			if _, err := c.Send(msg.Message{ID: msg.NewID(), From: "a/b", To: "all", Body: fmt.Sprint("m", i)}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
}

// A peer that answers with far more than a sync page is read into memory whole by a node at
// start, before it listens: the response has a size, and the next node is asked instead.
func TestAResponseOverTheSizeCapIsRefusedAndTheNextNodeIsAsked(t *testing.T) {
	flood := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("["))
		el := []byte(`{"origin":"` + strings.Repeat("a", 4000) + `"},`)
		for i := 0; i < (17<<20)/len(el); i++ {
			if _, err := w.Write(el); err != nil {
				return
			}
		}
		w.Write([]byte(`{"origin":"z"}]`))
	}))
	defer flood.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"origin":"fine","seq":1,"ack":{"id":"0000000001","by":"a/b"}}]`))
	}))
	defer good.Close()

	only := &client.Client{URLs: []string{flood.URL}, Token: "t"}
	if _, err := only.Sync(map[string]uint64{}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("an oversized answer is an error, not a page: %v", err)
	}
	both := &client.Client{URLs: []string{flood.URL, good.URL}, Token: "t", HedgeDelay: time.Millisecond}
	recs, err := both.Sync(map[string]uint64{})
	if err != nil || len(recs) != 1 || recs[0].Origin != "fine" {
		t.Errorf("the next node answers instead: %v %+v", err, recs)
	}
}

func TestRosterAndWhoLeaveOutTheJoinProbes(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/roster":
			fmt.Fprintf(w, `[{"me":"box/claude.1","seen":%q},{"me":"box/%s","seen":%q},{"me":"box/%s","seen":%q}]`,
				now.Format(time.RFC3339), msg.ProbeReaderAgent, now.Format(time.RFC3339), msg.ProbeSenderAgent, now.Format(time.RFC3339))
		case "/v1/who":
			fmt.Fprintf(w, `{"box/claude.1":%q,"box/%s":%q}`, now.Format(time.RFC3339), msg.ProbeReaderAgent, now.Format(time.RFC3339))
		}
	}))
	defer srv.Close()
	c := &client.Client{URLs: []string{srv.URL}, Token: "t"}
	if r, err := c.Roster(); err != nil || len(r) != 1 || r[0].Me != "box/claude.1" {
		t.Errorf("roster: %+v %v", r, err)
	}
	if w, err := c.Who(); err != nil || len(w) != 1 {
		t.Errorf("who: %+v %v", w, err)
	}
}
