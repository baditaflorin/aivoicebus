package cli

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/attach"
	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
	"github.com/baditaflorin/aivoicebus/internal/worker"
)

// The scripted scenarios of `aivoicebus selftest`: what agents used to test by hand with real
// messages to real sessions (worker arithmetic, cancel and pause tests, brake tests, file checks).
// Every message goes in the test lane (msg/testlane.go): only a worker that opted in sees it, no
// session's hook or replay ever does, it is left out of stats and view, and the node purges it. They
// send as the machine's probe identity, never as this session, and sign with the machine's key like
// any send, so the worker's allowlist, signature and hold checks are the real ones.

// scenario is one run: the clients it reads and sends with, and the hops it has taken.
type scenario struct {
	c       *client.Client // sends (signed), and reads what the nodes serve to a read that did not ask for the lane
	lane    *client.Client // reads the test lane
	from    string         // the machine's probe sender
	reader  string         // the machine's probe reader (file scenario)
	timeout time.Duration  // how long each step waits
	hops    []selftestHop
}

func newScenario(c *client.Client, timeout time.Duration) *scenario {
	host := identity.Label()
	return &scenario{c: c, lane: c.WithLane(), from: host + "/" + msg.ProbeSenderAgent, reader: host + "/" + msg.ProbeReaderAgent, timeout: timeout}
}

// add records a hop and returns its error, if any.
func (r *scenario) add(name string, d time.Duration, detail string, err error) error {
	h := selftestHop{Name: name, Ms: d.Milliseconds(), OK: err == nil, Detail: detail}
	if err != nil {
		h.Detail = view.OneLine(err.Error(), 300)
	}
	r.hops = append(r.hops, h)
	return err
}

// ready checks that the nodes can carry a test message at all, as every scenario needs.
func (r *scenario) ready() error {
	t := time.Now()
	h, err := r.c.Health()
	switch {
	case err != nil && client.IsRejected(err):
		_ = r.add("health", time.Since(t), "", err)
		return fmt.Errorf("the node refused this machine's token: %v", view.OneLine(err.Error(), 120))
	case err != nil:
		_ = r.add("health", time.Since(t), "", err)
		return errors.New("no node answered /v1/health: check the network, then run `aivoicebus doctor`")
	}
	old, err := r.c.TestLane()
	if err == nil && old != "" {
		err = fmt.Errorf("%s predates the test lane, so it would show a test message to everyone it is addressed to: ask the user to update the nodes first (`aivoicebus nodes` lists them)", old)
	}
	if err != nil {
		_ = r.add("health", time.Since(t), "", err)
		return err
	}
	return r.add("health", time.Since(t), "node "+view.OneLine(h.Node, 40)+" release "+view.OneLine(h.Version, 20)+", every node has the test lane", nil)
}

// send prepares and sends one test message as the probe sender. The checks are the real ones
// (outbox.Prepare), so a refusal reads as a refusal does anywhere else.
func (r *scenario) send(req outbox.Request) (msg.Message, error) {
	req.From, req.Test, req.TargetSaid, req.Force = r.from, true, true, true
	plan, err := outbox.Prepare(r.c, req, io.Discard)
	if err != nil {
		return msg.Message{}, err
	}
	return plan.Send(r.c)
}

// thread reads thread id in the test lane.
func (r *scenario) thread(id string) ([]msg.Message, error) { return r.lane.Thread(id) }

// poll calls look every half second until it says it is done, an error ends it, or the step's time is up.
// It returns false when the time ran out.
func (r *scenario) poll(within time.Duration, look func() (done bool, err error)) (bool, error) {
	deadline := time.Now().Add(within)
	for {
		done, err := look()
		if err != nil || done {
			return done, err
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// taskOutcome is what a test task came to.
type taskOutcome struct {
	claimed time.Duration // until the claim showed in the thread; 0 when none did
	answer  string        // the worker's answer, "" when none came
	took    time.Duration // until the answer
}

// await watches task's thread until the worker (who) answers it, or until stop says so.
func (r *scenario) await(task msg.Message, who string, within time.Duration, stop func(claimed bool) bool) (taskOutcome, error) {
	var out taskOutcome
	start := time.Now()
	_, err := r.poll(within, func() (bool, error) {
		ms, err := r.thread(task.ID)
		if err != nil {
			return false, err
		}
		for _, m := range ms {
			switch {
			case !strings.EqualFold(m.From, who) || m.Retracted != nil:
			case m.Kind == msg.KindClaim && out.claimed == 0:
				out.claimed = time.Since(start)
			case (m.Kind == msg.KindEnd || m.Kind == msg.KindAnswer) && m.Re == task.ID:
				out.answer, out.took = m.Body, time.Since(start)
				return true, nil
			}
		}
		return stop != nil && stop(out.claimed > 0), nil
	})
	return out, err
}

// findWorker is the worker session on host the scenarios talk to: one that opted in and is around.
func (r *scenario) findWorker(host string) (string, error) {
	t := time.Now()
	roster, err := r.c.RosterAll()
	if err != nil {
		return "", r.add("find", time.Since(t), "", fmt.Errorf("could not read the roster: %v", err))
	}
	host = strings.ToLower(strings.TrimSpace(host))
	var best *msg.RosterEntry
	var notOptedIn, stale []string
	for i, e := range roster {
		agent := strings.TrimPrefix(strings.ToLower(e.Me), host+"/")
		if msg.HostOf(e.Me) != host || (agent != "worker" && !strings.HasSuffix(agent, ".worker")) {
			continue
		}
		switch {
		case !msg.OptedIn(e.Labels):
			notOptedIn = append(notOptedIn, e.Me)
		case !e.Listening && time.Since(e.Seen) > outbox.LiveWithin:
			stale = append(stale, e.Me)
		case best == nil || e.Seen.After(best.Seen):
			best = &roster[i]
		}
	}
	switch {
	case best != nil:
		return best.Me, r.add("find", time.Since(t), fmt.Sprintf("%s, seen %s ago, opted in to the test lane", view.Name(best.Me, 60), time.Since(best.Seen).Round(time.Second)), nil)
	case len(notOptedIn) > 0:
		return "", r.add("find", time.Since(t), "", fmt.Errorf("%s is there but has not opted in to the test lane: restart it with `aivoicebus worker --test-lane ...` (or `worker install --test-lane`) on %s", view.Name(notOptedIn[0], 60), host))
	case len(stale) > 0:
		return "", r.add("find", time.Since(t), "", fmt.Errorf("%s opted in but was last seen more than %s ago: is it running? (`aivoicebus worker status` on %s)", view.Name(stale[0], 60), outbox.SpanShort(outbox.LiveWithin), host))
	}
	return "", r.add("find", time.Since(t), "", fmt.Errorf("no worker seen on %s: a worker runs there with `aivoicebus worker install` (or `worker --test-lane` in a terminal); `aivoicebus who %s` lists what is there", host, host))
}

// workerOpts are the extra scenarios of `selftest --worker`.
type workerOpts struct {
	write, cancel, pause bool
	brake                int // the turn limit to check; 0 for none
}

// silentWorkerHint is what a task nobody claimed most likely means.
func silentWorkerHint(me, host string) string {
	return fmt.Sprintf("the worker did not take it: it may not allow this machine (`aivoicebus worker allow add %s` on %s), may be paused, may be busy with another task, or may not have the test lane on", msg.HostOf(me), host)
}

// runWorkerScenarios checks the worker on host: a calculation through the real gates and claim, that a plain
// read never shows the lane, and then what opts asks for.
func runWorkerScenarios(c *client.Client, host string, opts workerOpts, timeout time.Duration) ([]selftestHop, error) {
	r := newScenario(c, timeout)
	if err := r.ready(); err != nil {
		return r.hops, err
	}
	w, err := r.findWorker(host)
	if err != nil {
		return r.hops, err
	}
	// 1. a test task through every gate a task goes through: allowlist, signature, claim, answer
	t0 := time.Now()
	task, err := r.send(outbox.Request{To: w, Kind: msg.KindTask, Body: worker.TestBody("calc", "17*23"), TTL: "5m"})
	if err != nil {
		return r.hops, r.add("send", time.Since(t0), "", err)
	}
	if err := r.add("send", time.Since(t0), "test task "+task.ID+": calc 17*23", nil); err != nil {
		return r.hops, err
	}
	out, err := r.await(task, w, r.timeout, nil)
	if err == nil && out.answer == "" {
		err = errors.New(silentWorkerHint(r.from, host))
	}
	if err != nil {
		return r.hops, r.add("answer", r.timeout, "", err)
	}
	if out.claimed > 0 {
		_ = r.add("claim", out.claimed, "the worker took the task", nil)
	}
	if !strings.Contains(out.answer, "= 391") {
		return r.hops, r.add("answer", out.took, "", fmt.Errorf("the answer is not 17*23 = 391: %s (try again; if it stays, ask the user to check that worker with `aivoicebus worker status` there)", view.OneLine(out.answer, 160)))
	}
	if err := r.add("answer", out.took, "17*23 = 391, computed by the worker's built-in step (no command was run)", nil); err != nil {
		return r.hops, err
	}
	// 2. the nodes hide it from a read that did not ask for the lane
	if err := r.hidden("hidden", task); err != nil {
		return r.hops, err
	}
	// 3. how the worker is set up
	if err := r.step(w, host, "status", "", "status:"); err != nil {
		return r.hops, err
	}
	if opts.write {
		if err := r.step(w, host, "write", "", "write:"); err != nil {
			return r.hops, err
		}
	}
	if opts.cancel {
		if err := r.cancel(w, host); err != nil {
			return r.hops, err
		}
	}
	if opts.pause {
		if err := r.pause(w, host); err != nil {
			return r.hops, err
		}
	}
	if opts.brake > 0 {
		if err := r.brake(w, host, opts.brake); err != nil {
			return r.hops, err
		}
	}
	return r.hops, nil
}

// hidden checks that a read that did not ask for the lane does not show message m or its thread. The read declares the
// opt-in, so the missing request for the lane is all that keeps it hidden.
func (r *scenario) hidden(name string, m msg.Message) error {
	t := time.Now()
	plain := r.c.WithRaw(false)
	plain.Labels = msg.Labels{msg.TestLabel: msg.TestOn}
	ms, err := plain.Thread(m.ID)
	if err != nil {
		return r.add(name, time.Since(t), "", fmt.Errorf("the check that a plain read does not show it failed: %v", err))
	}
	for _, x := range ms {
		if x.IsTest() {
			return r.add(name, time.Since(t), "", errors.New("a node showed a test message to a read that did not ask for the test lane: it does not hide the lane; ask the user to check the nodes (`aivoicebus nodes`)"))
		}
	}
	return r.add(name, time.Since(t), "a read that does not ask for the test lane shows nothing of it", nil)
}

// step sends a test task for the worker's built-in step and checks its answer starts with want.
func (r *scenario) step(w, host, step, arg, want string) error {
	t := time.Now()
	task, err := r.send(outbox.Request{To: w, Kind: msg.KindTask, Body: worker.TestBody(step, arg), TTL: "5m"})
	if err != nil {
		return r.add(step, time.Since(t), "", err)
	}
	out, err := r.await(task, w, r.timeout, nil)
	if err == nil && out.answer == "" {
		err = errors.New(silentWorkerHint(r.from, host))
	}
	if err != nil {
		return r.add(step, time.Since(t), "", err)
	}
	_, text, _ := strings.Cut(out.answer, "test: ")
	if !strings.HasPrefix(text, want) {
		return r.add(step, out.took, "", fmt.Errorf("unexpected answer: %s (ask the user to check that the worker runs this release: `aivoicebus worker status` there)", view.OneLine(out.answer, 200)))
	}
	return r.add(step, out.took, view.OneLine(strings.TrimPrefix(text, want+" "), 220), nil)
}

// cancel sends a long sleep, takes it back once the worker has it, and checks the run stops within seconds.
func (r *scenario) cancel(w, host string) error {
	t := time.Now()
	task, err := r.send(outbox.Request{To: w, Kind: msg.KindTask, Body: worker.TestBody("sleep", "120"), TTL: "5m"})
	if err != nil {
		return r.add("cancel", time.Since(t), "", err)
	}
	out, err := r.await(task, w, r.timeout, func(claimed bool) bool { return claimed })
	if err == nil && out.claimed == 0 && out.answer == "" {
		err = errors.New(silentWorkerHint(r.from, host))
	}
	if err != nil {
		return r.add("cancel", time.Since(t), "", err)
	}
	if out.answer != "" {
		return r.add("cancel", time.Since(t), "", fmt.Errorf("the 120 s sleep was answered before it was cancelled: %s (try again with a longer --timeout, or ask the user to check the worker)", view.OneLine(out.answer, 160)))
	}
	t1 := time.Now()
	if _, err := r.c.Retract(task.ID, r.from, false, false); err != nil {
		return r.add("cancel", time.Since(t1), "", fmt.Errorf("the task could not be taken back: %v", err))
	}
	res, err := r.await(task, w, 30*time.Second, nil)
	switch {
	case err != nil:
		return r.add("cancel", time.Since(t1), "", err)
	case res.answer == "":
		return r.add("cancel", time.Since(t1), "", errors.New("the worker did not stop the sleep within 30 s of the cancel: `aivoicebus cancel` takes a task back, and a worker stops its run within seconds"))
	case !strings.Contains(res.answer, "stopped"):
		return r.add("cancel", time.Since(t1), "", fmt.Errorf("the worker answered, but not that it stopped: %s (ask the user to check that the worker runs this release: `aivoicebus worker status` there)", view.OneLine(res.answer, 160)))
	}
	return r.add("cancel", time.Since(t1), "taken back; the worker stopped the sleep and said so", nil)
}

// pause pauses this one worker (pause --only, in the lane, for a minute), checks a task waits meanwhile, and that the
// worker takes it once the pause is over.
func (r *scenario) pause(w, host string) error {
	t := time.Now()
	until := time.Now().Add(time.Minute)
	if _, err := r.send(outbox.Request{To: w, Prio: msg.PrioUrgent, TTL: "1m", Queue: true, Body: msg.PauseBody(until, "aivoicebus selftest: a one-minute pause of this worker only")}); err != nil {
		return r.add("pause", time.Since(t), "", err)
	}
	task, err := r.send(outbox.Request{To: w, Kind: msg.KindTask, Body: worker.TestBody("calc", "1+1"), TTL: "5m"})
	if err != nil {
		return r.add("pause", time.Since(t), "", err)
	}
	hold := 20 * time.Second
	out, err := r.await(task, w, hold, nil)
	switch {
	case err != nil:
		return r.add("pause", time.Since(t), "", err)
	case out.claimed > 0 || out.answer != "":
		return r.add("pause", time.Since(t), "", errors.New("the worker took a task while it was paused: the kill switch did not hold it; ask the user to check that the worker runs this release (`aivoicebus worker status` there)"))
	}
	_ = r.add("pause", time.Since(t), fmt.Sprintf("the task was not taken in %s while the worker was paused", hold), nil)
	t2 := time.Now()
	out, err = r.await(task, w, time.Until(until)+r.timeout, nil)
	if err == nil && out.answer == "" {
		err = errors.New("the worker did not take the task after the pause ended: " + silentWorkerHint(r.from, host))
	}
	if err != nil {
		return r.add("resume", time.Since(t2), "", err)
	}
	return r.add("resume", time.Since(t2), "the pause ended by itself and the worker took the task", nil)
}

// brake starts a conversation with a turn limit of n messages and checks the nodes refuse message n+1 and let an
// [end] through.
func (r *scenario) brake(w, host string, n int) error {
	t := time.Now()
	if n < 3 || n > 50 {
		return r.add("brake", time.Since(t), "", outbox.Usagef("--brake is 3 to 50 messages: the task and its answer are two of them"))
	}
	root, err := r.send(outbox.Request{To: w, Kind: msg.KindTask, Body: worker.TestBody("echo", "brake"), TTL: "5m", MaxTurns: n})
	if err != nil {
		return r.add("brake", time.Since(t), "", err)
	}
	out, err := r.await(root, w, r.timeout, nil)
	if err == nil && out.answer == "" {
		err = errors.New(silentWorkerHint(r.from, host))
	}
	if err != nil {
		return r.add("brake", time.Since(t), "", err)
	}
	// the thread holds the task and the answer: n-2 more messages fit, the next is refused. Each says
	// something different, so the loop check does not refuse one for another reason.
	note := func(k int) (msg.Message, error) {
		return r.send(outbox.Request{To: w, Thread: root.Thread, Re: root.ID, Body: fmt.Sprintf("selftest brake note %d: %s", k, randomWords(k)), TTL: "5m"})
	}
	for k := 3; k <= n; k++ {
		if _, err := note(k); err != nil {
			return r.add("brake", time.Since(t), "", fmt.Errorf("message %d of %d was refused too early: %v", k, n, view.OneLine(err.Error(), 200)))
		}
	}
	_, err = note(n + 1)
	switch {
	case err == nil:
		return r.add("brake", time.Since(t), "", fmt.Errorf("the nodes accepted message %d of a conversation limited to %d: the turn brake did not hold; ask the user to check the nodes (`aivoicebus nodes`)", n+1, n))
	case !strings.Contains(err.Error(), "turn limit"):
		return r.add("brake", time.Since(t), "", fmt.Errorf("message %d was refused, but not for the turn limit: %v", n+1, view.OneLine(err.Error(), 200)))
	}
	if _, err := r.send(outbox.Request{To: w, Thread: root.Thread, Re: root.ID, Kind: msg.KindEnd, Body: "selftest brake: closing", TTL: "5m"}); err != nil {
		return r.add("brake", time.Since(t), "", fmt.Errorf("an [end] was refused after the limit, and it must always go: %v", view.OneLine(err.Error(), 200)))
	}
	return r.add("brake", time.Since(t), fmt.Sprintf("a limit of %d messages: %d went, message %d was refused (turn limit), and the [end] went through", n, n, n+1), nil)
}

// randomWords is k-dependent noise of unrelated words, so two notes share almost none.
func randomWords(k int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("selftest brake %d %d", k, time.Now().UnixNano())))
	var b strings.Builder
	for i := 0; i < 5; i++ {
		b.WriteString(hex.EncodeToString(sum[i*4:i*4+4]) + " ")
	}
	return strings.TrimSpace(b.String())
}

// parseSize reads "300k", "1m" or "300000" as bytes.
func parseSize(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := 1
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimSuffix(s, "m")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n*mult > attach.MaxFile {
		return 0, outbox.Usagef("--file is a size from 1 byte to %s, like 300k or 1m", attach.HumanSize(attach.MaxFile))
	}
	return n * mult, nil
}

// testBytes is size bytes of data both ends can reproduce: the sha256 chain of a counter.
func testBytes(size int) []byte {
	out := make([]byte, 0, size+sha256.Size)
	var block [8]byte
	for i := uint64(0); len(out) < size; i++ {
		binary.BigEndian.PutUint64(block[:], i)
		sum := sha256.Sum256(append([]byte("aivoicebus selftest file "), block[:]...))
		out = append(out, sum[:]...)
	}
	return out[:size]
}

// runFileScenario sends a file of the given size through via (pipe or node) from the probe sender to the probe reader on
// this machine, the way `send --file` and `get` do, and checks the reader gets exactly those bytes. The message that
// carries the link is a test message, and the file lives five minutes at most.
func runFileScenario(c *client.Client, size int, via string, timeout time.Duration) ([]selftestHop, error) {
	r := newScenario(c, timeout)
	if err := r.ready(); err != nil {
		return r.hops, err
	}
	data := testBytes(size)
	want := sha256.Sum256(data)
	wantHex := hex.EncodeToString(want[:])
	ctx, cancel := context.WithTimeout(context.Background(), 4*timeout)
	defer cancel()
	name := "selftest-" + strconv.Itoa(size) + ".bin"

	fs, err := fileStore(c, via)
	if err != nil {
		return r.hops, r.add("put", 0, "", err)
	}
	if err := outbox.CheckContent(data); err != nil {
		return r.hops, r.add("put", 0, "", fmt.Errorf("the test bytes look like a credential to the send check (try another size): %v", err))
	}
	// the reader opts in by declaring it, as a session does, before anything is addressed to it
	reader := r.lane
	reader.Labels = msg.Labels{msg.TestLabel: msg.TestOn}
	if _, err := reader.Inbox(r.reader, false); err != nil {
		return r.hops, r.add("put", 0, "", fmt.Errorf("could not read as the probe reader: %v", err))
	}
	t := time.Now()
	l, _, err := fs.Put(ctx, data, attach.Meta{Name: name}, true)
	if err != nil {
		return r.hops, r.add("put", time.Since(t), "", fmt.Errorf("the %s did not take the file: %v", fs.Name(), err))
	}
	l.Name = name
	l = sealLink(c, l, r.reader, false, io.Discard)
	if err := r.add("put", time.Since(t), fmt.Sprintf("%s encrypted here and held by the %s (sha256 %s)", attach.HumanSize(int64(size)), fs.Name(), wantHex[:12]), nil); err != nil {
		return r.hops, err
	}
	t = time.Now()
	carrier, err := r.send(outbox.Request{To: r.reader, Body: withLine("aivoicebus selftest file", l), File: true, TTL: "5m"})
	if err != nil {
		return r.hops, r.add("send", time.Since(t), "", err)
	}
	if err := r.add("send", time.Since(t), "the link went in test message "+carrier.ID, nil); err != nil {
		return r.hops, err
	}
	// the reader finds the message, opens the link and checks what it got
	t = time.Now()
	var got msg.Message
	found, err := r.poll(timeout, func() (bool, error) {
		ms, err := reader.Inbox(r.reader, false)
		if err != nil {
			return false, err
		}
		for _, m := range ms {
			if m.ID == carrier.ID {
				got = m
				return true, nil
			}
		}
		return false, nil
	})
	if err == nil && !found {
		err = errors.New("the message with the link did not reach the probe reader in the test lane in time: try again in a minute, then run `aivoicebus doctor`")
	}
	if err != nil {
		return r.hops, r.add("visible", time.Since(t), "", err)
	}
	if err := r.add("visible", time.Since(t), "the reader's test-lane inbox showed the message", nil); err != nil {
		return r.hops, err
	}
	t = time.Now()
	link, found, ferr := attach.Find(got.Body)
	if ferr != nil || !found {
		return r.hops, r.add("get", time.Since(t), "", errors.New("the message arrived without a usable link: try again, and if it stays ask the user to check the nodes (`aivoicebus nodes`)"))
	}
	prov := attach.ProviderFor(link)
	if prov != nil {
		prov = withToken(prov, c)
	}
	store, ok := prov.(attach.FileStore)
	if !ok {
		return r.hops, r.add("get", time.Since(t), "", errors.New("the link points at a service this machine is not set up to use: run it again with --via node (`aivoicebus selftest --file 300k --via node`)"))
	}
	open, err := attach.Unseal(link)
	if err != nil {
		return r.hops, r.add("get", time.Since(t), "", err)
	}
	f, err := store.GetFile(ctx, open)
	if err != nil {
		return r.hops, r.add("get", time.Since(t), "", fmt.Errorf("the file could not be fetched: %v", err))
	}
	sum := sha256.Sum256(f.Data)
	switch {
	case len(f.Data) != size:
		return r.hops, r.add("get", time.Since(t), "", fmt.Errorf("the file came back as %d bytes, not %d: try again, and if it stays ask the user to check the nodes (`aivoicebus nodes`)", len(f.Data), size))
	case hex.EncodeToString(sum[:]) != wantHex:
		return r.hops, r.add("get", time.Since(t), "", fmt.Errorf("the file came back with sha256 %s, not %s: try again with --via node, and if it stays ask the user to check the nodes (`aivoicebus nodes`)", hex.EncodeToString(sum[:])[:12], wantHex[:12]))
	}
	if err := r.add("get", time.Since(t), fmt.Sprintf("%s fetched, decrypted and compared: sha256 %s matches", attach.HumanSize(int64(len(f.Data))), wantHex[:12]), nil); err != nil {
		return r.hops, err
	}
	_ = c.AckVia(carrier.ID, r.reader, msg.ViaCLI, 0)
	return r.hops, r.hidden("hidden", carrier)
}
