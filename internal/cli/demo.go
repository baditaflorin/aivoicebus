package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/attach"
	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
	"github.com/baditaflorin/aivoicebus/internal/view"
	"github.com/baditaflorin/aivoicebus/viewer"
)

// demoCmd is `aivoicebus demo`: the bus at work with nothing of this machine's touched. Two
// throwaway nodes on loopback, in this process, replicate to each other; two invented agents talk
// through them with the real client (a task, a claim, progress, a file, an answer, the brakes
// refusing a loop, an end), one narrated line a step; a viewer follows it. Everything lives in a
// temporary directory under a scratch HOME and is gone when the command ends, Ctrl-C included.
func demoCmd(args []string, s Streams) error {
	fs := newFlagSet("demo")
	keep := fs.Bool("keep", false, "leave the nodes and the viewer running after the script, to poke around (Ctrl-C ends it)")
	open := fs.Bool("open", false, "open the viewer in the browser")
	pace := fs.Duration("pace", 700*time.Millisecond, "pause between steps (0: as fast as it goes)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return outbox.Usagef("demo takes no arguments")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }() // the first Ctrl-C cleans up; a second one is the default: it ends the process
	return runDemo(ctx, demoOptions{Keep: *keep, Open: *open, Pace: *pace}, s)
}

type demoOptions struct {
	Keep, Open bool
	Pace       time.Duration
	// Ready, when set, is told the temporary directory and the addresses as soon as they serve
	// (tests use it to check that all of it is gone afterwards).
	Ready func(dir string, addrs []string)
}

// The two invented agents.
const (
	demoAlice      = "laptop/claude.a1b2"
	demoAliceTitle = "Fix the flaky login test"
	demoBob        = "buildbox/codex.c3d4"
	demoBobTitle   = "Build box helper"
	demoReplicate  = 200 * time.Millisecond
)

func runDemo(ctx context.Context, o demoOptions, s Streams) (err error) {
	dir, err := os.MkdirTemp("", "aivoicebus-demo-*") // 0700
	if err != nil {
		return err
	}
	restore, err := demoIsolate(filepath.Join(dir, "home"))
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	d := &demo{dir: dir, s: s, pace: o.Pace}
	defer func() {
		cleanup := d.close()
		restore()
		if rmErr := os.RemoveAll(dir); rmErr != nil && cleanup == nil {
			cleanup = rmErr
		}
		if cleanup == nil {
			cleanup = d.gone()
		}
		if cleanup != nil {
			fmt.Fprintf(s.Err, "demo: cleaning up: %v\n", cleanup)
			if err == nil {
				err = cleanup
			}
			return
		}
		fmt.Fprintf(s.Out, "cleaned up: the 2 nodes and the viewer are stopped (their ports refuse connections) and %s is gone\n", dir)
	}()

	if err := d.start(); err != nil {
		return err
	}
	if o.Ready != nil {
		o.Ready(dir, d.addrs())
	}
	fmt.Fprintf(s.Out, "aivoicebus demo: two throwaway nodes on loopback, two invented agents; nothing of this machine's config, token or mesh is used\n")
	d.say("nodes demo-a %s and demo-b %s, replicating every %s; data in %s", d.nodes[0].url, d.nodes[1].url, demoReplicate, dir)
	d.say("viewer (read-only): %s", d.viewURL)
	d.say("the agents: alice is %s %q, on demo-a; bob is %s %q, on demo-b", demoAlice, demoAliceTitle, demoBob, demoBobTitle)
	if o.Open {
		demoOpen(d.viewURL, s)
	}
	err = d.script(ctx)
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(s.Out, "interrupted")
		return nil
	case err != nil:
		return err
	}
	if !o.Keep {
		fmt.Fprintln(s.Out, "that is the whole script; `aivoicebus demo --keep` leaves it running to look at in the viewer")
		return nil
	}
	tok := filepath.Join(dir, "token")
	if err := os.WriteFile(tok, []byte(d.token+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "\nstill running (--keep). To poke at it from another terminal, with a scratch HOME so nothing of yours changes:\n")
	fmt.Fprintf(s.Out, "  export HOME=%s AIVOICEBUS_URL=%s,%s AIVOICEBUS_TOKEN_FILE=%s AIVOICEBUS_ME=you/demo.0001\n", filepath.Join(dir, "home"), d.nodes[0].url, d.nodes[1].url, tok)
	fmt.Fprintf(s.Out, "  aivoicebus who; aivoicebus thread %s; aivoicebus send --to %s \"hello\"\n", d.task, demoBob)
	fmt.Fprintf(s.Out, "Ctrl-C stops it and removes everything.\n")
	<-ctx.Done()
	return nil
}

// demoIsolate points HOME at a scratch directory and clears every AIVOICEBUS_ setting for the
// demo's lifetime, so no code path it runs can read or write this machine's aivoicebus files (the
// client pins machine keys and reads titles under HOME). It returns what puts them back.
func demoIsolate(home string) (restore func(), err error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	saved := map[string]*string{}
	keep := func(k string) {
		if _, done := saved[k]; done {
			return
		}
		if v, ok := os.LookupEnv(k); ok {
			saved[k] = &v
		} else {
			saved[k] = nil
		}
	}
	keep("HOME")
	os.Setenv("HOME", home)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "AIVOICEBUS_") {
			keep(k)
			os.Unsetenv(k)
		}
	}
	keep("AIVOICEBUS_PIPE_URL")
	os.Setenv("AIVOICEBUS_PIPE_URL", "off") // files go to the demo's own nodes: it works offline
	return func() {
		for k, v := range saved {
			if v == nil {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, *v)
			}
		}
	}, nil
}

type demoNode struct {
	name string
	url  string
	addr string
	st   *store.Store
	srv  *http.Server
}

type demo struct {
	dir     string
	s       Streams
	pace    time.Duration
	token   string
	nodes   []*demoNode
	viewSrv *http.Server
	viewURL string
	viewAt  string
	stop    context.CancelFunc
	wg      sync.WaitGroup
	task    string
	steps   int
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// start brings up the two nodes, their replication and the viewer, each on a free loopback port.
func (d *demo) start() error {
	var err error
	if d.token, err = randomHex(20); err != nil {
		return err
	}
	observer, err := randomHex(20)
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancel(context.Background())
	d.stop = stop
	serve := func(h http.Handler) (*http.Server, string, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, "", err
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
		d.wg.Add(1)
		go func() { defer d.wg.Done(); _ = srv.Serve(ln) }()
		return srv, ln.Addr().String(), nil
	}
	for _, name := range []string{"demo-a", "demo-b"} {
		st, err := store.Open(filepath.Join(d.dir, name), name)
		if err != nil {
			return err
		}
		srv, addr, err := serve(server.NewHandler(st, d.token, server.WithNode(name), server.WithObserverToken(observer)))
		if err != nil {
			return err
		}
		d.nodes = append(d.nodes, &demoNode{name: name, url: "http://" + addr, addr: addr, st: st, srv: srv})
	}
	for i, n := range d.nodes {
		peer := d.nodes[1-i].url
		d.wg.Add(1)
		go func() { defer d.wg.Done(); server.Replicate(ctx, n.st, []string{peer}, d.token, demoReplicate, nil) }()
	}
	v, err := viewer.Open(viewer.Options{Nodes: []string{d.nodes[0].url, d.nodes[1].url}, Token: observer,
		Dir: filepath.Join(d.dir, "viewer"), Every: 300 * time.Millisecond, Title: "aivoicebus demo"})
	if err != nil {
		return err
	}
	key, err := randomHex(16)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	d.viewAt = ln.Addr().String()
	d.viewSrv = &http.Server{Handler: v.Handler(viewer.HTTPOptions{Key: key, Hosts: viewer.LoopbackHosts(d.viewAt)}), ReadHeaderTimeout: 5 * time.Second}
	d.wg.Add(2)
	go func() { defer d.wg.Done(); _ = d.viewSrv.Serve(ln) }()
	go func() { defer d.wg.Done(); v.Run(ctx) }()
	d.viewURL = "http://" + d.viewAt + "/?key=" + key
	return nil
}

func (d *demo) addrs() []string {
	var a []string
	for _, n := range d.nodes {
		a = append(a, n.addr)
	}
	if d.viewAt != "" {
		a = append(a, d.viewAt)
	}
	return a
}

// close stops everything start started and waits for every goroutine of it to return.
func (d *demo) close() error {
	if d.stop != nil {
		d.stop()
	}
	shut, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var errs []error
	for _, srv := range d.servers() {
		if srv.Shutdown(shut) != nil {
			srv.Close() // a read still held open (someone's wait, with --keep) is cut off
		}
	}
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		errs = append(errs, errors.New("a node or the viewer did not stop within 10s"))
	}
	return errors.Join(errs...)
}

func (d *demo) servers() []*http.Server {
	var ss []*http.Server
	for _, n := range d.nodes {
		ss = append(ss, n.srv)
	}
	if d.viewSrv != nil {
		ss = append(ss, d.viewSrv)
	}
	return ss
}

// gone checks what close and the removal claim: no port of the demo answers, the directory is gone.
func (d *demo) gone() error {
	for _, a := range d.addrs() {
		if c, err := net.DialTimeout("tcp", a, time.Second); err == nil {
			c.Close()
			return fmt.Errorf("%s still accepts connections", a)
		}
	}
	if _, err := os.Stat(d.dir); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s is still there", d.dir)
	}
	return nil
}

// say prints one narrated step, after the pause that lets a person (or the viewer) follow.
func (d *demo) say(format string, a ...any) {
	d.steps++
	fmt.Fprintf(d.s.Out, "%2d. %s\n", d.steps, fmt.Sprintf(format, a...))
}

func (d *demo) wait(ctx context.Context) error {
	if d.pace <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d.pace):
		return nil
	}
}

// agent is one invented session: a client on its own node first (it writes there, and the other
// node gets it by replication), with the other node too, as a machine is configured with every node.
func (d *demo) agent(own int, title string, labels msg.Labels) *client.Client {
	return &client.Client{URLs: []string{d.nodes[own].url, d.nodes[1-own].url}, Token: d.token, NoSign: true,
		WriteQuorum: 1, Warn: io.Discard, Title: title, Labels: labels, Timeout: 5 * time.Second}
}

// script is the conversation. Each step is real: the nodes take, replicate and refuse as they would
// for any agent.
func (d *demo) script(ctx context.Context) error {
	alice := d.agent(0, demoAliceTitle, msg.Labels{"machine": "laptop", "tool": "claude", "repo": "webapp"})
	bob := d.agent(1, demoBobTitle, msg.Labels{"machine": "buildbox", "tool": "codex", "repo": "webapp"})
	if _, err := alice.Inbox(demoAlice, false); err != nil { // both sessions say hello to their node
		return err
	}
	if _, err := bob.Inbox(demoBob, false); err != nil {
		return err
	}

	// Bob stands by: a wait on demo-b, held open by the node until something for him arrives.
	type woke struct {
		m   msg.Message
		at  time.Time
		err error
	}
	got := make(chan woke, 1)
	waitCtx, endWait := context.WithCancel(ctx)
	defer endWait()
	go func() {
		m, err := demoWaitFor(waitCtx, bob, demoBob, func(m msg.Message) bool { return m.Kind == msg.KindTask })
		got <- woke{m, time.Now(), err}
	}()
	if err := d.wait(ctx); err != nil {
		return err
	}
	var bobSeen msg.RosterEntry
	for range 50 { // the wait reaches the node a moment after it starts
		if r, err := alice.RosterAll(); err == nil {
			for _, e := range r {
				if e.Me == demoBob && e.Listening {
					bobSeen = e
				}
			}
		}
		if bobSeen.Me != "" {
			break
		}
		if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	if bobSeen.Me == "" {
		return errors.New("bob's wait never showed on the nodes")
	}
	d.say("bob stands by on demo-b (aivoicebus wait); alice runs `who`: %s", view.Session(bobSeen, time.Now()))

	if err := d.wait(ctx); err != nil {
		return err
	}
	sentAt := time.Now()
	task, err := alice.Send(msg.Message{ID: msg.NewID(), From: demoAlice, To: "buildbox", Kind: msg.KindTask, Prio: msg.PrioUrgent,
		Body: "Run the login e2e suite on buildbox and send me the failing test names."})
	if err != nil {
		return fmt.Errorf("alice's task: %w", err)
	}
	d.task = task.ID
	d.say("alice (on demo-a) gives the machine buildbox a task %s: every session there sees it until one claims it", task.ID)
	var w woke
	select {
	case w = <-got:
	case <-time.After(15 * time.Second):
		return errors.New("the task never reached bob")
	case <-ctx.Done():
		return ctx.Err()
	}
	if w.err != nil {
		return fmt.Errorf("bob's wait: %w", w.err)
	}
	d.say("bob's wait on demo-b wakes %s later: demo-b got the task from demo-a by replication", w.at.Sub(sentAt).Round(time.Millisecond))

	reply := func(c *client.Client, from, to, re, kind, body string) (msg.Message, error) {
		return c.Send(msg.Message{ID: msg.NewID(), From: from, To: to, Re: re, Thread: task.ID, Kind: kind, Body: body})
	}
	steps := []struct {
		kind, body, line string
	}{
		{msg.KindClaim, "taking it", "bob claims it: the first claim wins, and no other session on buildbox sees it any more"},
		{msg.KindProgress, "suite 3 of 5 running", "bob says how it goes (progress): a wait on alice's side shows it and goes on"},
	}
	for _, st := range steps {
		if err := d.wait(ctx); err != nil {
			return err
		}
		if _, err := reply(bob, demoBob, demoAlice, task.ID, st.kind, st.body); err != nil {
			return fmt.Errorf("bob's %s: %w", st.kind, err)
		}
		d.say("%s", st.line)
	}

	// A file: encrypted here, held by demo-b for a few minutes, fetched once and checked by alice.
	if err := d.wait(ctx); err != nil {
		return err
	}
	log := []byte("FAIL TestLoginRedirect (0.42s): redirect went to /home, want /dashboard\nFAIL TestCookiePath (0.08s): cookie path /, want /app\nok   37 other tests\n")
	l, _, err := (&attach.Node{URLs: []string{d.nodes[1].url}, Token: d.token}).Put(ctx, log, true)
	if err != nil {
		return fmt.Errorf("bob's file: %w", err)
	}
	l.Name = "e2e-failures.log"
	fm, err := bob.Send(msg.Message{ID: msg.NewID(), From: demoBob, To: demoAlice, Re: task.ID, Thread: task.ID, Prio: msg.PrioUrgent,
		Body: "the full log of the failures\n" + l.Line()})
	if err != nil {
		return fmt.Errorf("bob's file message: %w", err)
	}
	link, ok, err := attach.Find(fm.Body)
	if err != nil || !ok {
		return fmt.Errorf("the file link: %v", err)
	}
	data, err := (&attach.Node{URLs: []string{d.nodes[0].url, d.nodes[1].url}, Token: d.token}).Get(ctx, link)
	if err != nil {
		return fmt.Errorf("alice fetching the file: %w", err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != link.SHA256 {
		return errors.New("the file alice got does not match bob's sha256")
	}
	d.say("bob sends a file (%s, %s): encrypted on his side, held by demo-b, read once; alice fetched it and its sha256 matches", l.Name, attach.HumanSize(int64(len(data))))

	if err := d.wait(ctx); err != nil {
		return err
	}
	ans, err := reply(bob, demoBob, demoAlice, task.ID, msg.KindAnswer, "2 failures: TestLoginRedirect and TestCookiePath; the log is in the file")
	if err != nil {
		return fmt.Errorf("bob's answer: %w", err)
	}
	d.say("bob answers: \"%s\"", ans.Body)

	// The brakes: two polite agents thanking each other for ever. A node refuses the same words said
	// a third time in one thread.
	last := ans.ID
	refused := false
	for turn := 0; turn < 6 && !refused; turn++ {
		if err := d.wait(ctx); err != nil {
			return err
		}
		who, from, to, body, c := "alice", demoAlice, demoBob, "thanks!", alice
		if turn%2 == 1 {
			who, from, to, body, c = "bob", demoBob, demoAlice, "you're welcome!", bob
		}
		m, err := reply(c, from, to, last, "", body)
		var se *client.StatusError
		switch {
		case errors.As(err, &se):
			why := strings.TrimPrefix(strings.TrimSpace(se.Text), store.ErrTurnLimit.Error()+": ")
			d.say("the brakes: the node refused %s's third %q: %s", who, body, view.OneLine(why, 300))
			refused = true
		case err != nil:
			return err
		default:
			d.say("%s: %q", who, m.Body)
			last = m.ID
		}
	}
	if !refused {
		return errors.New("the nodes never refused the loop")
	}

	if err := d.wait(ctx); err != nil {
		return err
	}
	if _, err := reply(alice, demoAlice, demoBob, task.ID, msg.KindEnd, "fixed on fix/login, closing"); err != nil {
		return fmt.Errorf("alice's end: %w", err)
	}
	d.say("alice closes the conversation (end): an end always passes the brakes, and the task is done")
	return nil
}

// demoWaitFor is what `aivoicebus wait` does: the node holds the read open until something new
// arrives; it returns the first message for me that want accepts.
func demoWaitFor(ctx context.Context, c *client.Client, me string, want func(msg.Message) bool) (msg.Message, error) {
	_, gen, err := c.InboxWait(ctx, me, 0, false, 0)
	if err != nil {
		return msg.Message{}, err
	}
	for {
		ms, g, err := c.InboxWait(ctx, me, gen, true, 20*time.Second)
		if err != nil {
			return msg.Message{}, err
		}
		gen = g
		for _, m := range ms {
			if want(m) {
				return m, nil
			}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// demoOpen opens url in the browser and waits for the opener to return, so no process of the
// demo's outlives it.
func demoOpen(url string, s Streams) {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, opener, url).Run(); err != nil {
		fmt.Fprintf(s.Err, "could not open a browser (%v): open the viewer address above yourself\n", err)
	}
}
