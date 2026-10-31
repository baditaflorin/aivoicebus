package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/worker"
)

func writeRunSpec(t *testing.T, name, body string) {
	t.Helper()
	dir := worker.RunDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const echoerSpec = `{"argv":["/bin/sh","-c","printf 'arg=%s' \"$1\"","sh","{0}"],"args":["[a-z]+"],"sandbox":false,"timeout":"20s"}`

func TestAWorkerWithRunTakesANamedCommandAndNothingElse(t *testing.T) {
	mark := workerSetup(t, "desk")
	writeRunSpec(t, "echoer", echoerSpec)
	body, err := worker.EncodeRun("echoer", []string{"hi"})
	if err != nil {
		t.Fatal(err)
	}
	sendTask(t, boss, "lap", body)
	code, out, e := runWorker(t, "--run")
	if code != 0 || !strings.Contains(out, "answered") {
		t.Fatalf("worker: %d\n%s\n%s", code, out, e)
	}
	if got := inboxOf(t, boss); !strings.Contains(got, "arg=hi") || !strings.Contains(got, "exit 0") {
		t.Fatalf("the sender was not told the output:\n%s", got)
	}
	// A plain task is not run by a run-only worker.
	sendTask(t, boss, "lap", "touch "+mark)
	_, out, _ = runWorker(t, "--run")
	if !strings.Contains(out, "named commands only") {
		t.Errorf("a plain task was not refused:\n%s", out)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("a plain task ran on a run-only worker")
	}
}

func TestARunRequestOutsideItsFileIsRefusedAndNothingRuns(t *testing.T) {
	workerSetup(t, "desk")
	writeRunSpec(t, "echoer", echoerSpec)
	for _, c := range []struct {
		cmd  string
		args []string
		want string
	}{
		{"echoer", []string{"Hi; touch x"}, "does not match"},
		{"echoer", nil, "takes 1 argument"},
		{"formatdisk", []string{"x"}, "no named command"},
	} {
		body, _ := worker.EncodeRun(c.cmd, c.args)
		sendTask(t, boss, "lap", body)
		runWorker(t, "--run")
		if got := inboxOf(t, boss); !strings.Contains(got, "not run") || !strings.Contains(got, c.want) {
			t.Errorf("%s %v: want %q in\n%s", c.cmd, c.args, c.want, got)
		}
	}
}

func TestARunTaskNeverReachesAnAgentWorkerWithoutRun(t *testing.T) {
	mark := workerSetup(t, "desk")
	body, _ := worker.EncodeRun("echoer", []string{"hi"})
	sendTask(t, boss, "lap", body)
	_, out, _ := runWorker(t, "--exec", "sh -c 'touch "+mark+"'")
	if !strings.Contains(out, "not started with --run") {
		t.Errorf("not skipped:\n%s", out)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("the agent command ran for a [run] task")
	}
}

func TestRunNeedsMachinesAndACommand(t *testing.T) {
	for _, args := range [][]string{{"run"}, {"run", "lap"}, {"run", "--", "x"}, {"run", "lap", "--"}, {"run", "lap", "--", "Bad Name"}} {
		if code, _, _ := run(t, "", args...); code == 0 {
			t.Errorf("%v succeeded", args)
		}
	}
}
