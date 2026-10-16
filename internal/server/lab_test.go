package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// lab is an in-process mesh for failure tests. Every node has a stable address
// that outlives restarts; replication between nodes goes through per-pair links
// that can be cut, which is a network partition. A node can also be crashed
// (unreachable for everyone), restarted on its own disk, or wiped (restarted on
// an empty disk), the way a lost disk is.
type lab struct {
	t     *testing.T
	token string
	nodes []*labNode
	mu    sync.RWMutex
	cut   map[[2]int]bool // directed links that drop traffic: [a, b] means a cannot reach b
	proxy map[[2]int]*httptest.Server
	every time.Duration
}

type labNode struct {
	l      *lab
	i      int
	name   string
	dir    string
	front  *httptest.Server
	mu     sync.RWMutex
	st     *store.Store
	h      http.Handler // nil while the node is down
	board  *PeerBoard
	cancel context.CancelFunc
	done   chan struct{}
}

// drop ends the connection without an answer, as a dead or unreachable host does.
func drop(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
			return
		}
	}
	http.Error(w, "unreachable", http.StatusServiceUnavailable)
}

func newLab(t *testing.T, n int) *lab {
	t.Helper()
	l := &lab{t: t, token: "lab-token-0123456789abcdef0123", cut: map[[2]int]bool{}, proxy: map[[2]int]*httptest.Server{}, every: 15 * time.Millisecond}
	for i := 0; i < n; i++ {
		node := &labNode{l: l, i: i, name: fmt.Sprintf("n%d", i), dir: t.TempDir()}
		node.front = httptest.NewServer(http.HandlerFunc(node.serve))
		l.nodes = append(l.nodes, node)
	}
	for a := range l.nodes {
		for b := range l.nodes {
			if a == b {
				continue
			}
			a, b := a, b
			l.proxy[[2]int{a, b}] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				l.mu.RLock()
				blocked := l.cut[[2]int{a, b}]
				l.mu.RUnlock()
				if blocked {
					drop(w)
					return
				}
				l.nodes[b].serve(w, r)
			}))
		}
	}
	for _, node := range l.nodes {
		node.start()
	}
	t.Cleanup(func() {
		for _, node := range l.nodes {
			node.stop()
			node.front.Close()
		}
		for _, p := range l.proxy {
			p.Close()
		}
	})
	return l
}

func (n *labNode) serve(w http.ResponseWriter, r *http.Request) {
	n.mu.RLock()
	h := n.h
	n.mu.RUnlock()
	if h == nil {
		drop(w)
		return
	}
	h.ServeHTTP(w, r)
}

func (n *labNode) peerURLs() []string {
	var out []string
	for b := range n.l.nodes {
		if b != n.i {
			out = append(out, n.l.proxy[[2]int{n.i, b}].URL)
		}
	}
	return out
}

// start opens the node on its disk the way Run does: take back what the peers
// hold, then serve (writes only once ready) and keep replicating.
func (n *labNode) start() {
	st, err := store.Open(n.dir, n.name)
	if err != nil {
		n.l.t.Fatal(err)
	}
	st.SetSendLimit(1 << 20)
	peers := n.peerURLs()
	board := NewPeerBoard(peers)
	for _, p := range peers {
		PullOnce(st, p, n.l.token)
	}
	h := NewHandler(st, n.l.token, WithNode(n.name), WithPeerBoard(board), WithReady(Ready(st, board, len(peers))))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Replicate(ctx, st, peers, n.l.token, n.l.every, board); close(done) }()
	n.mu.Lock()
	n.st, n.h, n.board, n.cancel, n.done = st, h, board, cancel, done
	n.mu.Unlock()
}

// stop takes the node down: unreachable, and no longer replicating.
func (n *labNode) stop() {
	n.mu.Lock()
	cancel, done := n.cancel, n.done
	n.h, n.cancel, n.done = nil, nil, nil
	n.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (n *labNode) up() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.h != nil
}

func (n *labNode) wipe() {
	n.stop()
	if err := os.RemoveAll(n.dir); err != nil {
		n.l.t.Fatal(err)
	}
	if err := os.MkdirAll(n.dir, 0o700); err != nil {
		n.l.t.Fatal(err)
	}
	n.start()
}

// isolate cuts node i from every other node in both directions; clients can still reach it.
func (l *lab) isolate(i int, on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for j := range l.nodes {
		if j != i {
			l.cut[[2]int{i, j}], l.cut[[2]int{j, i}] = on, on
		}
	}
}

func (l *lab) healAll() {
	l.mu.Lock()
	l.cut = map[[2]int]bool{}
	l.mu.Unlock()
	for _, n := range l.nodes {
		if !n.up() {
			n.start()
		}
	}
}

func (l *lab) fronts(rot int) []string {
	var out []string
	for k := range l.nodes {
		out = append(out, l.nodes[(k+rot)%len(l.nodes)].front.URL)
	}
	return out
}

// snapshot is what one node holds: the version vector, message ids and (id, by) acks.
type snapshot struct {
	vv   map[string]uint64
	msgs map[string]bool
	acks map[string]bool
}

func (n *labNode) snapshot() snapshot {
	n.mu.RLock()
	st := n.st
	n.mu.RUnlock()
	s := snapshot{vv: st.VV(), msgs: map[string]bool{}, acks: map[string]bool{}}
	for _, r := range st.Since(map[string]uint64{}, 1<<30) {
		if r.Msg != nil {
			s.msgs[r.Msg.ID] = true
		}
		if r.Ack != nil {
			s.acks[r.Ack.ID+"|"+r.Ack.By] = true
		}
	}
	return s
}

// store is the node's store as it is now (it is replaced when the node restarts).
func (n *labNode) store() *store.Store {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.st
}

// deriveRetractions applies the retraction rule to a node's records the plain way, looking at all
// of them: the oracle the node's own indexed answer is held to.
func deriveRetractions(recs []msg.Record) map[string]msg.Retraction {
	from := map[string]string{}
	for _, r := range recs {
		if r.Msg != nil {
			from[r.Msg.ID] = r.Msg.From
		}
	}
	out := map[string]msg.Retraction{}
	for _, r := range recs {
		if r.Ack == nil {
			continue
		}
		mid, leak, ok := msg.RetractedID(r.Ack.ID)
		sender, held := from[mid]
		if !ok || !held || !msg.MayRetract(r.Ack.By, sender) {
			continue
		}
		cur, have := out[mid]
		if !have {
			out[mid] = msg.Retraction{By: r.Ack.By, At: r.Ack.At, Leak: leak}
			continue
		}
		cur.Leak = cur.Leak || leak
		if r.Ack.At.Before(cur.At) || (r.Ack.At.Equal(cur.At) && r.Ack.By < cur.By) {
			cur.By, cur.At = r.Ack.By, r.Ack.At
		}
		out[mid] = cur
	}
	return out
}

// retractedOn is what the node's store says of each message id: nil for one that is not retracted.
func retractedOn(st *store.Store, ids []string) map[string]*msg.Retraction {
	out := map[string]*msg.Retraction{}
	for _, id := range ids {
		for _, m := range st.Thread(id) {
			if m.ID == id {
				out[id] = m.Retracted
			}
		}
	}
	return out
}

func sameVV(a, b map[string]uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// converge waits until every node holds the same records, and returns how long that took.
func (l *lab) converge(timeout time.Duration) (time.Duration, bool) {
	start := time.Now()
	for time.Since(start) < timeout {
		ok := true
		first := l.nodes[0].snapshot()
		for _, n := range l.nodes[1:] {
			if !sameVV(first.vv, n.snapshot().vv) {
				ok = false
				break
			}
		}
		if ok {
			return time.Since(start), true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start), false
}

// agent is a client the way a session runs it: every node in its list, quorum writes.
type agent struct {
	id   string
	c    *client.Client
	warn *bytes.Buffer
}

func (l *lab) newAgent(id string, rot int) *agent {
	w := &bytes.Buffer{}
	return &agent{id: id, warn: w, c: &client.Client{URLs: l.fronts(rot), Token: l.token, Warn: w, Timeout: 2 * time.Second}}
}

// send returns the stored message and whether it reached two nodes (so it must survive losing one).
func (a *agent) send(to, body string) (msg.Message, bool, error) {
	a.warn.Reset()
	out, err := a.c.Send(msg.Message{ID: msg.NewID(), From: a.id, To: to, Body: body})
	return out, err == nil && !strings.Contains(a.warn.String(), "only"), err
}

func (a *agent) ack(id string) (durable bool, err error) {
	a.warn.Reset()
	err = a.c.Ack(id, a.id)
	return err == nil && !strings.Contains(a.warn.String(), "only"), err
}

func ids(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestAPartitionHealsAndEveryNodeEndsWithTheSameRecords(t *testing.T) {
	l := newLab(t, 3)
	a := l.newAgent("host0/claude.a", 0)
	for i := 0; i < 5; i++ {
		if _, _, err := a.send("all", fmt.Sprintf("before %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := l.converge(5 * time.Second); !ok {
		t.Fatal("no convergence before the partition")
	}

	l.isolate(2, true)
	b := l.newAgent("host1/claude.b", 0)
	for i := 0; i < 10; i++ {
		if _, _, err := b.send("all", fmt.Sprintf("during %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(l.nodes[2].snapshot().msgs); n != 5 {
		t.Fatalf("the isolated node must not see what was sent during the partition: %d messages", n)
	}
	if h := l.nodes[2].board.Snapshot(); h[0].OK || h[1].OK {
		t.Errorf("the isolated node should report both peers down: %+v", h)
	}

	l.isolate(2, false)
	took, ok := l.converge(10 * time.Second)
	if !ok {
		t.Fatal("the mesh did not converge after the partition healed")
	}
	t.Logf("converged %v after healing", took)
	for _, n := range l.nodes {
		if got := len(n.snapshot().msgs); got != 15 {
			t.Errorf("%s holds %d messages, want 15", n.name, got)
		}
	}
	time.Sleep(100 * time.Millisecond)
	for _, p := range l.nodes[2].board.Snapshot() {
		if !p.OK || p.Lag != 0 {
			t.Errorf("after healing, replication to the node is ok and caught up: %+v", p)
		}
	}
}

func TestAWipedNodeRefusesWritesUntilItHasHeardFromAPeerSoItNeverReusesSequenceNumbers(t *testing.T) {
	l := newLab(t, 3)
	a := l.newAgent("host0/claude.a", 0)
	for i := 0; i < 6; i++ {
		if _, _, err := a.send("all", fmt.Sprintf("old %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := l.converge(5 * time.Second); !ok {
		t.Fatal("no convergence")
	}
	before := l.nodes[1].snapshot().vv["n1"]
	if before == 0 {
		t.Fatal("node n1 should have originated records")
	}

	// the disk of n1 dies while it cannot reach its peers
	l.isolate(1, true)
	l.nodes[1].wipe()
	if vv := l.nodes[1].snapshot().vv; len(vv) != 0 {
		t.Fatalf("a wiped node starts empty: %v", vv)
	}

	// it serves reads, but refuses writes: a write now would be numbered from 1 again
	only := &client.Client{URLs: []string{l.nodes[1].front.URL}, Token: l.token, Timeout: 2 * time.Second, Warn: &bytes.Buffer{}}
	if _, err := only.Send(msg.Message{ID: msg.NewID(), From: "host0/claude.a", To: "all", Body: "must be refused"}); err == nil {
		t.Fatal("a node that has not heard from any peer must not accept a write")
	}
	if _, err := only.Inbox("host0/claude.a", false); err != nil {
		t.Errorf("reads are still served: %v", err)
	}
	// and an agent that has the other nodes too is not stopped: the write lands elsewhere
	if _, _, err := l.newAgent("host0/claude.a", 1).send("all", "goes to the others"); err != nil {
		t.Errorf("writes fail over to the nodes that are ready: %v", err)
	}

	l.isolate(1, false)
	if _, ok := l.converge(10 * time.Second); !ok {
		t.Fatal("no convergence after the wiped node reached its peers")
	}
	snap := l.nodes[1].snapshot()
	if snap.vv["n1"] < before {
		t.Fatalf("n1 recovered its own log from the peers: has %d, had %d", snap.vv["n1"], before)
	}
	// now it is ready, and what it writes next is numbered after what it had
	if _, err := only.Send(msg.Message{ID: msg.NewID(), From: "host0/claude.a", To: "all", Body: "accepted after recovery"}); err != nil {
		t.Fatalf("after recovery the node takes writes: %v", err)
	}
	if _, ok := l.converge(10 * time.Second); !ok {
		t.Fatal("no convergence after the recovered node wrote")
	}
	if got := l.nodes[0].snapshot().vv["n1"]; got <= before {
		t.Errorf("the recovered node's new record must be numbered after its old ones: %d vs %d", got, before)
	}
}

func TestRollingRestartsUnderLoadLoseNothingThatWasAcknowledgedByTwoNodes(t *testing.T) {
	l := newLab(t, 3)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	durable := map[string]bool{}
	for k := 0; k < 2; k++ {
		a := l.newAgent(fmt.Sprintf("host%d/claude.a", k), k)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if m, two, err := a.send("all", fmt.Sprintf("%s load %d", a.id, i)); err == nil && two {
					mu.Lock()
					durable[m.ID] = true
					mu.Unlock()
				}
				time.Sleep(3 * time.Millisecond)
			}
		}()
	}
	for round := 0; round < 2; round++ {
		for _, n := range l.nodes {
			time.Sleep(60 * time.Millisecond)
			n.stop()
			time.Sleep(40 * time.Millisecond)
			n.start()
		}
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	if _, ok := l.converge(10 * time.Second); !ok {
		t.Fatal("no convergence after the rolling restart")
	}
	have := l.nodes[0].snapshot().msgs
	missing := 0
	for id := range durable {
		if !have[id] {
			missing++
		}
	}
	if missing > 0 || len(durable) == 0 {
		t.Errorf("%d of %d messages acknowledged by two nodes are missing after a rolling restart", missing, len(durable))
	}
	t.Logf("%d messages acknowledged by two nodes during the rolling restart, none lost", len(durable))
}

func TestChaos(t *testing.T) {
	seeds := []int64{1, 2, 3}
	steps := 120
	if os.Getenv("AIVOICEBUS_SOAK") != "" {
		seeds, steps = []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, 600
	} else if testing.Short() {
		seeds, steps = seeds[:1], 60
	}
	for _, seed := range seeds {
		seed := seed
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) { chaos(t, seed, steps) })
	}
}

// chaos does everything to a three-node mesh at once for steps operations, then
// heals it and checks that nothing acknowledged by two nodes was lost and that every
// node ends with exactly the same records.
func chaos(t *testing.T, seed int64, steps int) {
	l := newLab(t, 3)
	rnd := newRand(seed)
	agents := []*agent{l.newAgent("host0/claude.a", 0), l.newAgent("host1/codex.b", 1), l.newAgent("host2/claude.c", 2)}
	var sentIDs []string
	sentBy := map[string]string{} // message id -> the identity that sent it
	durableMsgs := map[string]bool{}
	durableAcks := map[string]bool{}
	durableRetr := map[string]bool{} // retracted by the sender's machine, and acknowledged by two nodes
	durableLeak := map[string]bool{} // ... and flagged as a credential
	forged := map[string]bool{}      // message id|identity of claims by another machine written straight into a node, as replication could bring them
	retractors := map[*agent]*client.Client{}
	for _, a := range agents { // a short lookup retry: the chaos is long enough as it is
		retractors[a] = &client.Client{URLs: a.c.URLs, Token: l.token, Warn: a.warn, Timeout: 2 * time.Second, HedgeDelay: 10 * time.Millisecond}
	}
	wiped := false
	n := 0
	for step := 0; step < steps; step++ {
		a := agents[rnd.Intn(len(agents))]
		switch op := rnd.Intn(100); {
		case op < 52: // send
			n++
			targets := []string{"all", "host1", "host0/claude", "@repo=lab", a.id}
			m, two, err := a.send(targets[rnd.Intn(len(targets))], fmt.Sprintf("seed%d message %d", seed, n))
			if err == nil {
				sentIDs = append(sentIDs, m.ID)
				sentBy[m.ID] = a.id
				if two {
					durableMsgs[m.ID] = true
				}
			}
		case op < 64 && len(sentIDs) > 0: // acknowledge
			id := sentIDs[rnd.Intn(len(sentIDs))]
			if two, err := a.ack(id); err == nil && two {
				durableAcks[id+"|"+a.id] = true
			}
		case op < 74 && len(sentIDs) > 0: // retract, as the sender, as another session on its machine, from another machine, or forged
			id := sentIDs[rnd.Intn(len(sentIDs))]
			sender, leak := sentBy[id], rnd.Intn(4) == 0
			switch rnd.Intn(5) {
			case 0, 1, 2: // an authorised retraction, by the sender itself or by a session that never sent anything
				by := sender
				if rnd.Intn(2) == 0 {
					by = msg.HostOf(sender) + "/operator.s" + fmt.Sprint(rnd.Intn(3))
				}
				rc := retractors[a]
				a.warn.Reset()
				if _, err := rc.Retract(id, by, leak, false); err == nil && !strings.Contains(a.warn.String(), "only") {
					durableRetr[id] = true
					durableLeak[id] = durableLeak[id] || leak
				}
			case 3: // another machine asks through the API: refused wherever the message is held, and never written
				other := agents[rnd.Intn(len(agents))].id
				if msg.HostOf(other) != msg.HostOf(sender) {
					retractors[a].Retract(id, other, leak, false)
				}
			default: // another machine's claim that got into a node's log anyway: it must change nothing
				other := agents[rnd.Intn(len(agents))].id
				if nd := l.nodes[rnd.Intn(3)]; nd.up() && msg.HostOf(other) != msg.HostOf(sender) {
					ackID := msg.RetractPrefix + id
					if leak {
						ackID = msg.RetractLeakPrefix + id
					}
					if nd.store().AckWith(msg.Ack{ID: ackID, By: other}) == nil {
						forged[id+"|"+other] = true
					}
				}
			}
		case op < 80: // read: registers the session and its labels
			a.c.Labels = msg.Labels{"repo": "lab"}
			a.c.Inbox(a.id, false)
		case op < 88: // partition a node, or heal everything
			if rnd.Intn(3) == 0 {
				l.mu.Lock()
				l.cut = map[[2]int]bool{}
				l.mu.Unlock()
			} else {
				l.isolate(rnd.Intn(3), true)
			}
		case op < 96: // crash a node, keeping at least two up, or bring one back
			i := rnd.Intn(3)
			upCount := 0
			for _, x := range l.nodes {
				if x.up() {
					upCount++
				}
			}
			switch {
			case !l.nodes[i].up():
				l.nodes[i].start()
			case upCount > 2:
				l.nodes[i].stop()
			}
		default: // lose a disk, but only from a healthy, fully replicated mesh, one node at a time
			if wiped {
				break
			}
			l.healAll()
			if _, ok := l.converge(8 * time.Second); ok {
				l.nodes[rnd.Intn(3)].wipe()
				wiped = true
			}
		}
		time.Sleep(time.Duration(rnd.Intn(3)) * time.Millisecond)
	}

	l.healAll()
	took, ok := l.converge(20 * time.Second)
	if !ok {
		for _, nd := range l.nodes {
			t.Logf("%s vv=%v", nd.name, nd.snapshot().vv)
		}
		t.Fatalf("seed %d: the mesh did not converge after healing", seed)
	}
	t.Logf("seed %d: %d operations, converged %v after healing, %d messages acknowledged by two nodes", seed, steps, took, len(durableMsgs))

	ref := l.nodes[0].snapshot()
	for _, nd := range l.nodes[1:] {
		s := nd.snapshot()
		if strings.Join(ids(s.msgs), ",") != strings.Join(ids(ref.msgs), ",") {
			t.Errorf("seed %d: %s and %s hold different messages (%d vs %d)", seed, l.nodes[0].name, nd.name, len(ref.msgs), len(s.msgs))
		}
		if strings.Join(ids(s.acks), ",") != strings.Join(ids(ref.acks), ",") {
			t.Errorf("seed %d: %s and %s hold different receipts (%d vs %d)", seed, l.nodes[0].name, nd.name, len(ref.acks), len(s.acks))
		}
	}
	for id := range durableMsgs {
		if !ref.msgs[id] {
			t.Errorf("seed %d: message %s was acknowledged by two nodes and is gone", seed, id)
		}
	}
	for k := range durableAcks {
		if !ref.acks[k] {
			t.Errorf("seed %d: receipt %s was acknowledged by two nodes and is gone", seed, k)
		}
	}
	checkRetractions(t, l, seed, ids(ref.msgs), agents, durableRetr, durableLeak, forged)
	t.Logf("seed %d: %d messages retracted by their sender's machine and acknowledged by two nodes, %d forged claims by other machines", seed, len(durableRetr), len(forged))
	for _, nd := range l.nodes {
		recs := nd.st.Since(map[string]uint64{}, 1<<30)
		seen := map[string]uint64{}
		for _, r := range recs {
			seen[r.Origin]++
		}
		for origin, count := range seen {
			if count != nd.snapshot().vv[origin] {
				t.Errorf("seed %d: %s holds %d records from %s but its version vector says %d", seed, nd.name, count, origin, nd.snapshot().vv[origin])
			}
		}
	}
}

// checkRetractions holds a converged mesh to the retraction rule: every node hides the same
// messages with the same retractor, time and flag, which is what the rule gives when applied to the
// node's own records (so a claim by another machine hid nothing), whatever order the records
// arrived in and across wiped and restarted nodes; what two nodes acknowledged is retracted; and what
// is retracted is delivered to nobody, with the same inbox on every node.
func checkRetractions(t *testing.T, l *lab, seed int64, msgIDs []string, agents []*agent, durable, durableLeak, forged map[string]bool) {
	t.Helper()
	ref := retractedOn(l.nodes[0].store(), msgIDs)
	for _, nd := range l.nodes {
		st := nd.store()
		got := retractedOn(st, msgIDs)
		oracle := deriveRetractions(st.Since(map[string]uint64{}, 1<<30))
		for _, id := range msgIDs {
			g, w, r := got[id], oracle[id], ref[id]
			if (g == nil) != (r == nil) || g != nil && (g.By != r.By || !g.At.Equal(r.At) || g.Leak != r.Leak) {
				t.Errorf("seed %d: %s and %s disagree on message %s: %+v vs %+v", seed, l.nodes[0].name, nd.name, id, r, g)
			}
			if _, retracted := oracle[id]; retracted != (g != nil) || g != nil && (g.By != w.By || !g.At.Equal(w.At) || g.Leak != w.Leak) {
				t.Errorf("seed %d: %s says %+v of message %s, the rule applied to its records says %+v", seed, nd.name, g, id, w)
			}
		}
		if st.Stats().Quarantined != 0 {
			t.Errorf("seed %d: %s quarantined a record", seed, nd.name)
		}
		// the only claims by another machine in any log are the ones written straight into a node
		// above: the API refuses them wherever the message is held, so none was ever recorded
		inert := map[string]bool{}
		sender := map[string]string{}
		recs := st.Since(map[string]uint64{}, 1<<30)
		for _, r := range recs {
			if r.Msg != nil {
				sender[r.Msg.ID] = r.Msg.From
			}
		}
		for _, r := range recs {
			if r.Ack == nil {
				continue
			}
			if mid, _, ok := msg.RetractedID(r.Ack.ID); ok {
				if from, held := sender[mid]; held && !msg.MayRetract(r.Ack.By, from) {
					inert[mid+"|"+r.Ack.By] = true
				}
			}
		}
		if strings.Join(ids(inert), ",") != strings.Join(ids(forged), ",") || st.Stats().RetractionsIneffective != len(forged) {
			t.Errorf("seed %d: %s holds %d claims by another machine (counted %d), %d were forged on purpose", seed, nd.name, len(inert), st.Stats().RetractionsIneffective, len(forged))
		}
	}
	for id := range durable {
		if r := ref[id]; r == nil {
			t.Errorf("seed %d: message %s was retracted by its sender's machine, acknowledged by two nodes, and is not retracted", seed, id)
		} else if durableLeak[id] && !r.Leak {
			t.Errorf("seed %d: message %s was flagged as a credential, acknowledged by two nodes, and the flag is gone", seed, id)
		}
	}
	readers := []string{"reader/claude.z"}
	for _, a := range agents {
		readers = append(readers, a.id)
	}
	for _, me := range readers {
		var first, firstAll string
		for i, nd := range l.nodes {
			unread := nd.store().InboxFor(me, false, nil)
			for _, m := range unread {
				if ref[m.ID] != nil {
					t.Errorf("seed %d: %s delivers retracted message %s to %s", seed, nd.name, m.ID, me)
				}
			}
			// a message written to two nodes carries each node's own time, so what is compared is what
			// a reader acts on: which messages, their words, and who withdrew them
			rows := func(ms []msg.Message) string {
				var out []string
				for _, m := range ms {
					r, _ := json.Marshal(m.Retracted)
					out = append(out, m.ID+"|"+m.Body+"|"+string(r))
				}
				sort.Strings(out)
				return strings.Join(out, "\n")
			}
			a, b := rows(unread), rows(nd.store().InboxFor(me, true, nil))
			if i == 0 {
				first, firstAll = a, b
				continue
			}
			if a != first || b != firstAll {
				t.Errorf("seed %d: %s and %s give %s different inboxes:\n%s\n--\n%s", seed, l.nodes[0].name, nd.name, me, first, a)
			}
		}
	}
}

func TestARetractionSurvivesAPartitionAnArrivalOrderThatPutsItBeforeItsMessageAndAWipedNode(t *testing.T) {
	l := newLab(t, 3)
	// the message goes to n1 only, n0 learns it by replication and retracts it: n0's record
	// is numbered before n1's, so a node that pulls both takes the retraction first
	to1 := &client.Client{URLs: []string{l.nodes[1].front.URL}, Token: l.token, Timeout: 2 * time.Second, Warn: &bytes.Buffer{}}
	l.isolate(2, true)
	m, err := to1.Send(msg.Message{ID: msg.NewID(), From: "host0/claude.a", To: "all", Body: "to be taken back"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !l.nodes[0].snapshot().msgs[m.ID] {
		time.Sleep(10 * time.Millisecond)
	}
	to0 := &client.Client{URLs: []string{l.nodes[0].front.URL}, Token: l.token, Timeout: 2 * time.Second, Warn: &bytes.Buffer{}}
	if res, err := to0.Retract(m.ID, "host0/claude.other", true, false); err != nil || res.State != msg.RetractRetracted {
		t.Fatalf("%+v %v", res, err)
	}
	// a node cut off from the others still shows it, as it has not heard of the retraction
	if got := l.nodes[2].store().InboxFor("air/codex.q", false, nil); len(got) != 0 {
		t.Fatalf("n2 has not heard of the message either: %+v", got)
	}

	l.isolate(2, false)
	if _, ok := l.converge(10 * time.Second); !ok {
		t.Fatal("no convergence")
	}
	for _, nd := range l.nodes {
		th := nd.store().Thread(m.ID)
		if len(th) != 1 || th[0].Retracted == nil || th[0].Retracted.By != "host0/claude.other" || !th[0].Retracted.Leak || strings.Contains(th[0].Body, "taken back") {
			t.Errorf("%s: %+v", nd.name, th)
		}
		if got := nd.store().InboxFor("air/codex.q", false, nil); len(got) != 0 {
			t.Errorf("%s delivers it: %+v", nd.name, got)
		}
		if st := nd.store().Stats(); st.Quarantined != 0 || st.Retracted != 1 {
			t.Errorf("%s: %+v", nd.name, st)
		}
	}

	// a node that lost its disk takes the retraction back from its peers
	l.isolate(0, true)
	l.nodes[0].wipe()
	l.isolate(0, false)
	if _, ok := l.converge(10 * time.Second); !ok {
		t.Fatal("no convergence after the wipe")
	}
	if th := l.nodes[0].store().Thread(m.ID); len(th) != 1 || th[0].Retracted == nil || !th[0].Retracted.Leak {
		t.Errorf("the wiped node recovered the retraction with the message: %+v", th)
	}
}

func TestPresenceSpreadsToEveryNodeAndSurvivesARestart(t *testing.T) {
	l := newLab(t, 3)
	// a session that only ever talks to node 0
	c := &client.Client{URLs: []string{l.nodes[0].front.URL}, Token: l.token, Labels: msg.Labels{"repo": "alpha", "branch": "feat/x"}}
	if _, err := c.Inbox("mac/claude.a1b2", false); err != nil {
		t.Fatal(err)
	}
	has := func(n *labNode) bool {
		n.mu.RLock()
		defer n.mu.RUnlock()
		for _, e := range n.st.Roster() {
			if e.Me == "mac/claude.a1b2" && e.Labels["repo"] == "alpha" && e.Labels["branch"] == "feat/x" {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !(has(l.nodes[1]) && has(l.nodes[2])) {
		time.Sleep(20 * time.Millisecond)
	}
	if !has(l.nodes[1]) || !has(l.nodes[2]) {
		t.Fatal("a session that polled one node should be on every node's roster, with its labels")
	}
	// so a selector preview works whichever node the asker reaches
	other := &client.Client{URLs: []string{l.nodes[2].front.URL}, Token: l.token}
	roster, err := other.Roster()
	if err != nil || len(roster) == 0 {
		t.Fatalf("%v %v", err, roster)
	}

	// a restart on the same disk keeps it (the node flushes its roster when it is stopped)
	l.nodes[1].mu.RLock()
	l.nodes[1].st.FlushPresence()
	l.nodes[1].mu.RUnlock()
	l.nodes[1].stop()
	l.isolate(1, true) // and cut off, so only the disk can tell it
	l.nodes[1].start()
	if !has(l.nodes[1]) {
		t.Error("a restarted node must remember who was around")
	}
}
