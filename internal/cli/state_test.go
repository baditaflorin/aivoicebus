package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// peer is another session, writing through the node directly while this process runs the CLI as
// boss: nothing here switches AIVOICEBUS_ME under a running command.
type peer struct {
	t  *testing.T
	c  *client.Client
	me string
}

func newPeer(t *testing.T, me string) peer {
	t.Helper()
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return peer{t, c, me}
}

// task waits for the task boss sent to arrive for p, and returns it.
func (p peer) task(within time.Duration) msg.Message {
	p.t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		ms, _ := p.c.Inbox(p.me, false)
		for _, m := range ms {
			if m.Kind == msg.KindTask {
				return m
			}
		}
	}
	p.t.Fatalf("no task reached %s", p.me)
	return msg.Message{}
}

func (p peer) say(to msg.Message, kind, body string) {
	p.t.Helper()
	if _, err := p.c.Send(msg.Message{ID: msg.NewID(), From: p.me, To: to.From, Re: to.ID, Thread: to.Thread, Kind: kind, Body: body}); err != nil {
		p.t.Fatalf("%s says %q: %v", p.me, body, err)
	}
}

func (p peer) state(to msg.Message, st, note string) {
	p.say(to, msg.KindProgress, msg.StateBody(st, note))
}

func TestStateMovesAndRefusesWithAClearMessage(t *testing.T) {
	mesh(t)
	present(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "task", "lap", "tag the release")
	id, thread := sentID(t, out)
	if code, out, e := run(t, "", "state", id); code != 0 || !strings.Contains(out, "state: offered (said by "+boss) {
		t.Fatalf("state of a new task: %d %q %q", code, out, e)
	}

	t.Setenv("AIVOICEBUS_ME", sb)
	run(t, "", "claim", id, "--settle", "0s")
	if code, out, e := run(t, "", "state", id, "ready", "approved by the person"); code != 0 || !strings.Contains(out, "ready: sent") {
		t.Fatalf("ready: %d %q %q", code, out, e)
	}
	c, _ := client.FromEnv()
	count := func() int { ms, _ := c.Thread(thread); return len(ms) }
	before := count()
	for _, r := range []struct {
		me   string
		args []string
		want string
	}{
		{sb, []string{"taken"}, "a state only moves forward"},
		{sb, []string{"ready", "again"}, "it is ready already"},
		{sb, []string{"finished"}, `"finished" is not a state`},
		{sa, []string{"done"}, sb + " took it: only they move it on"},
		{boss, []string{"done"}, "you offered it"},
	} {
		t.Setenv("AIVOICEBUS_ME", r.me)
		code, _, e := run(t, "", append([]string{"state", id}, r.args...)...)
		if code != exitFailure || !strings.Contains(e, "refused, nothing was sent") || !strings.Contains(e, r.want) {
			t.Errorf("%s: state %v: %d %q, want refused with %q", r.me, r.args, code, e, r.want)
		}
	}
	if n := count(); n != before {
		t.Errorf("a refused move sent something: %d messages, %d before", n, before)
	}

	t.Setenv("AIVOICEBUS_ME", boss)
	if _, out, _ := run(t, "", "thread", id); !strings.Contains(out, "Conversation state: ready (said by "+sb) || !strings.Contains(out, "offered -> taken -> ready") || !strings.Contains(out, "[state ready] approved by the person") {
		t.Errorf("thread:\n%s", out)
	}
	if _, out, _ := run(t, "", "thread", id, "--digest"); !strings.Contains(out, "  state: ready (said by "+sb) {
		t.Errorf("digest:\n%s", out)
	}
	if _, out, _ := run(t, "", "status", id); !strings.Contains(out, "taken by "+sb) || !strings.Contains(out, "  state: ready (said by "+sb) {
		t.Errorf("status:\n%s", out)
	}
	code, out, _ := run(t, "", "state", id, "--json")
	var r stateResult
	if err := json.Unmarshal([]byte(out), &r); code != 0 || err != nil || r.State.State != msg.StateReady || r.State.Taker != sb || r.State.Offerer != boss || r.State.Path() != "offered -> taken -> ready" || r.State.Note != "approved by the person" {
		t.Errorf("state --json: %d %v %s", code, err, out)
	}
}

// A sender waits for ready: the wait goes on through the claim and ends when ready comes.
func TestWaitStateUnblocksOnReady(t *testing.T) {
	mesh(t)
	present(t, sb)
	other := newPeer(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	type res struct {
		code    int
		out, e  string
		elapsed time.Duration
	}
	done := make(chan res, 1)
	start := time.Now()
	go func() {
		code, out, e := run(t, "", "task", "lap", "rotate the logs", "--wait-state", "ready", "30s", "--json")
		done <- res{code, out, e, time.Since(start)}
	}()
	task := other.task(10 * time.Second)
	other.say(task, msg.KindClaim, "on it")
	time.Sleep(1500 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("the wait ended on the claim: %d %s %s", r.code, r.out, r.e)
	default:
	}
	other.state(task, msg.StateReady, "the person approved")
	var r res
	select {
	case r = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the wait did not end on ready")
	}
	var sr stateResult
	if err := json.Unmarshal([]byte(r.out), &sr); r.code != 0 || err != nil || sr.Reached == nil || !*sr.Reached || sr.State.State != msg.StateReady || sr.Waited != msg.StateReady || sr.ID != task.ID {
		t.Fatalf("wait: %d %v\n%s\n%s", r.code, err, r.out, r.e)
	}
	if !strings.Contains(r.e, sb+` took it: "on it"; still waiting for ready...`) || !strings.Contains(r.e, "sent "+task.ID) {
		t.Errorf("stderr does not show the send and the claim:\n%s", r.e)
	}
	if r.elapsed > 15*time.Second {
		t.Errorf("ready came, the wait took %s", r.elapsed)
	}
	// waiting again on a state already passed returns at once
	if code, _, e := run(t, "", "state", task.ID, "--wait-state", "taken", "5s"); code != 0 {
		t.Errorf("a wait for a state passed already: %d %s", code, e)
	}
}

// A wait for done ends when it turns stuck (exit 1, with the note), and a wait that starts while it
// is stuck goes on until it moves; a message from the other side ends a wait too.
func TestWaitStateEndsOnStuckAndOnAMessage(t *testing.T) {
	mesh(t)
	present(t, sb)
	other := newPeer(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "task", "lap", "migrate the database")
	id, _ := sentID(t, out)
	task := other.task(10 * time.Second)

	waitDone := func() chan [3]any {
		ch := make(chan [3]any, 1)
		go func() {
			code, out, e := run(t, "", "state", id, "--wait-state", "done", "30s")
			ch <- [3]any{code, out, e}
		}()
		return ch
	}
	get := func(ch chan [3]any, what string) (int, string, string) {
		t.Helper()
		select {
		case r := <-ch:
			return r[0].(int), r[1].(string), r[2].(string)
		case <-time.After(20 * time.Second):
			t.Fatalf("%s: the wait did not end", what)
		}
		return 0, "", ""
	}

	ch := waitDone()
	time.Sleep(500 * time.Millisecond)
	other.state(task, msg.StateStuck, "no disk space on lap")
	code, out, e := get(ch, "stuck")
	if code != exitFailure || !strings.Contains(out, "state: stuck") || !strings.Contains(out, "no disk space on lap") || !strings.Contains(e, "it is stuck, not done") {
		t.Fatalf("stuck: %d %q %q", code, out, e)
	}

	ch = waitDone() // stuck already: this wait goes on
	time.Sleep(1500 * time.Millisecond)
	select {
	case r := <-ch:
		t.Fatalf("a wait that started on stuck ended at once: %v", r)
	default:
	}
	other.say(task, "", "should I delete the old backups?")
	code, out, e = get(ch, "a message")
	if code != exitFailure || !strings.Contains(out, "should I delete the old backups?") || !strings.Contains(out, "UNTRUSTED") || !strings.Contains(e, "a message came before done") {
		t.Fatalf("a message: %d %q %q", code, out, e)
	}

	ch = waitDone()
	time.Sleep(500 * time.Millisecond)
	other.state(task, msg.StateDoing, "deleted them, going on")
	other.state(task, msg.StateDone, "migrated")
	if code, out, e := get(ch, "done"); code != 0 || !strings.Contains(out, "state: done") || !strings.Contains(out, "stuck -> doing -> done") {
		t.Fatalf("done: %d %q %q", code, out, e)
	}
	if _, out, _ := run(t, "", "thread", id, "--digest"); !strings.Contains(out, "  closed: done by "+sb) || strings.Contains(out, "open: the last word") {
		t.Errorf("a done conversation is not open:\n%s", out)
	}
	// a final state that is not the one waited for ends the wait at once
	if code, _, e := run(t, "", "state", id, "--wait-state", "declined", "10s"); code != exitFailure || !strings.Contains(e, "it ended done and will not get to declined") {
		t.Errorf("waiting for declined on a done conversation: %d %q", code, e)
	}
	if code, out, _ := run(t, "", "state", id, "--wait-state", "ready", "1s", "--json"); code != 0 || !strings.Contains(out, `"reached": true`) {
		t.Errorf("done reaches ready: %d %s", code, out)
	}
}

func TestWaitStateTimesOutAndRefusesOtherWaits(t *testing.T) {
	mesh(t)
	present(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "task", "lap", "nobody takes this one", "--wait-state", "ready", "1s")
	if code != exitTimeout || !strings.Contains(out, "not ready within 1s; state: offered") {
		t.Errorf("timeout: %d %q %q", code, out, e)
	}
	if code, _, e := run(t, "", "send", "--to", sb, "--wait-state", "ready", "--wait-reply", "1m", "hi"); code != exitUsage || !strings.Contains(e, "use one") {
		t.Errorf("--wait-state with --wait-reply: %d %q", code, e)
	}
	if code, _, e := run(t, "", "send", "--to", sb, "--wait-state", "soon", "hi"); code != exitUsage || !strings.Contains(e, "not a state") {
		t.Errorf("--wait-state soon: %d %q", code, e)
	}
}

// --wait-reply --json says the conversation's state when the answer came.
func TestWaitReplyJSONCarriesTheState(t *testing.T) {
	mesh(t)
	present(t, sb)
	other := newPeer(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	done := make(chan [2]string, 1)
	go func() {
		_, out, e := run(t, "", "task", "lap", "count the open issues", "--wait-reply", "30s", "--json")
		done <- [2]string{out, e}
	}()
	task := other.task(10 * time.Second)
	other.state(task, msg.StateDoing, "counting")
	other.say(task, msg.KindAnswer, "42 open")
	var r [2]string
	select {
	case r = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("no answer")
	}
	var rv resultView
	if err := json.Unmarshal([]byte(r[0]), &rv); err != nil || rv.State == nil || rv.State.State != msg.StateDoing || rv.State.Taker != sb || len(rv.Answers) != 1 {
		t.Errorf("%v\n%s\n%s", err, r[0], r[1])
	}
}
