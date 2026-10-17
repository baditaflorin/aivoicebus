package cli

import (
	"os"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"strings"
	"testing"
	"time"
)

// pause stops the worker (no new task, the running one stopped); resume needs a person, and only
// the machine that paused lifts it.
func TestPauseStopsWorkersAndOnlyThePausingMachineResumes(t *testing.T) {
	mark := workerSetup(t, "desk")
	t.Setenv("AIVOICEBUS_ME", boss)
	if code, out, e := run(t, "", "pause", "lap", "--for", "30m", "a loop between two agents"); code != 0 || !strings.Contains(out, "paused lap") {
		t.Fatalf("pause: %d %s %s", code, out, e)
	}
	sendTask(t, boss, "lap", "write the mark")
	if _, out, _ := runWorker(t, "--exec", "touch "+mark); !strings.Contains(out, "paused by desk/claude.z9") {
		t.Errorf("paused worker: %s", out)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("a paused worker ran a task")
	}
	t.Setenv("AIVOICEBUS_ME", boss)
	if code, _, e := run(t, "", "resume", "lap"); code == 0 || !strings.Contains(e, "needs a person at a terminal") {
		t.Errorf("resume without a terminal: %d %s", code, e)
	}
	old := readPassphrase
	t.Cleanup(func() { readPassphrase = old })
	readPassphrase = func(string) (string, error) { return "resume", nil }
	t.Setenv("AIVOICEBUS_ME", "box/claude.x")
	run(t, "", "resume", "lap") // another machine: does not lift it
	if _, out, _ := runWorker(t, "--exec", "touch "+mark); !strings.Contains(out, "paused by") {
		t.Errorf("another machine's resume lifted the pause: %s", out)
	}
	t.Setenv("AIVOICEBUS_ME", "desk/claude.other") // the machine that paused
	if code, _, e := run(t, "", "resume", "lap"); code != 0 {
		t.Fatalf("resume: %d %s", code, e)
	}
	runWorker(t, "--exec", "touch "+mark)
	if _, err := os.Stat(mark); err != nil {
		t.Error("the worker did not go on after the resume")
	}
}

// a pause stops a task that is running
func TestPauseStopsARunningTask(t *testing.T) {
	workerSetup(t, "desk")
	sendTask(t, boss, "lap", "a long job")
	c, _ := client.FromEnv()
	t.Setenv("AIVOICEBUS_ME", wk) // set once, here: the worker below runs as the worker
	done := make(chan string, 1)
	go func() {
		_, out, _ := run(t, "", "worker", "--once", "--settle", "0s", "--exec", "sleep 60")
		done <- out
	}()
	time.Sleep(3 * time.Second)
	if _, err := c.Send(msg.Message{ID: msg.NewID(), From: boss, To: "lap", Prio: msg.PrioUrgent, Body: msg.PauseBody(time.Now().Add(time.Hour), "stop everything")}); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if !strings.Contains(out, "paused by") {
			t.Errorf("%s", out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the pause did not stop the run")
	}
	if l := logEntries(t); len(l) == 0 || !strings.Contains(l[len(l)-1].Why, "stopped by a pause") {
		t.Errorf("log: %+v", l)
	}
}
