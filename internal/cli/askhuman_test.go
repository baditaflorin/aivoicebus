package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestAskHumanGetsTheTerminalAnswer(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	t.Setenv("AIVOICEBUS_ME", boss)
	go func() { // the person answers in a terminal (what `aivoicebus answer ID` sends)
		for i := 0; i < 100; i++ {
			ms, _ := c.Inbox(msg.HumanOf(boss), false)
			for _, m := range ms {
				if strings.HasPrefix(m.Body, msg.AskPrefix) {
					c.Send(msg.Message{ID: msg.NewID(), From: msg.HumanOf(boss), To: m.From, Re: m.ID, Thread: m.Thread, Kind: msg.KindAnswer, Body: msg.HumanAnswerPrefix + "yes"})
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	code, out, e := run(t, "", "ask-human", "deploy to staging?", "--choices", "yes,no", "--wait", "10s", "--json")
	var r askResult
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || !r.Answered || r.Answer != "yes" || r.Via != "terminal" {
		t.Fatalf("%d %v %s %s", code, err, out, e)
	}
	if code, _, _ := run(t, "", "ask-human", "anyone?", "--wait", "2s"); code != exitTimeout {
		t.Errorf("unanswered: %d", code)
	}
}

func TestAnswerIsThePersonsAndChecksTheChoice(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	q, _ := c.Send(msg.Message{ID: msg.NewID(), From: boss, To: msg.HumanOf(boss), Body: msg.AskBody("merge it?", []string{"yes", "no"}, "")})
	t.Setenv("AIVOICEBUS_ME", boss)
	t.Setenv("AIVOICEBUS_AGENT", "claude.z9")
	if code, _, e := run(t, "", "answer", q.ID); code == 0 || !strings.Contains(e, "not for an agent") {
		t.Errorf("an agent answered: %d %s", code, e)
	}
	t.Setenv("AIVOICEBUS_AGENT", "")
	if _, list, _ := run(t, "", "answer"); !strings.Contains(list, q.ID) || !strings.Contains(list, "[yes/no]") {
		t.Errorf("list: %s", list)
	}
	old := readTTYLine
	t.Cleanup(func() { readTTYLine = old })
	readTTYLine = func(string) (string, error) { return "maybe", nil }
	if code, _, _ := run(t, "", "answer", q.ID); code == 0 {
		t.Error("a choice that is not offered went through")
	}
	readTTYLine = func(string) (string, error) { return "no", nil }
	if code, _, e := run(t, "", "answer", q.ID); code != 0 {
		t.Fatalf("answer: %s", e)
	}
	ms, _ := c.Thread(q.ID)
	if last := ms[len(ms)-1]; last.From != msg.HumanOf(boss) || last.Body != msg.HumanAnswerPrefix+"no" {
		t.Errorf("%+v", last)
	}
}

// A wait shows a [progress] message and goes on to the answer.
func TestProgressDoesNotEndAWait(t *testing.T) {
	mesh(t)
	c, _ := client.FromEnv()
	go func() {
		for i := 0; i < 100; i++ {
			ms, _ := c.Inbox(sa, false)
			for _, m := range ms {
				if m.Kind == msg.KindTask || m.Body == "how goes it" {
					c.Send(msg.Message{ID: msg.NewID(), From: sa, To: m.From, Re: m.ID, Thread: m.Thread, Kind: msg.KindProgress, Body: "step 1/2"})
					time.Sleep(1500 * time.Millisecond)
					c.Send(msg.Message{ID: msg.NewID(), From: sa, To: m.From, Re: m.ID, Thread: m.Thread, Kind: msg.KindAnswer, Body: "done"})
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "send", "--to", sa, "how goes it", "--wait-reply", "15s")
	if code != 0 || !strings.Contains(e, "step 1/2") || !strings.Contains(out, "done") {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
}
