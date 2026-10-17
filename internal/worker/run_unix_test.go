//go:build unix

package worker

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A run's time limit ends everything it started, not just the first process: an agent that left a
// build or a server running in the background must not outlive its run.
func TestTheTimeLimitKillsTheWholeGroup(t *testing.T) {
	r := Exec(context.Background(), []string{"sh", "-c", "sleep 60 & echo $!; wait"}, "", os.Environ(), Limits{Timeout: 500 * time.Millisecond, MaxOutput: 100})
	pid, err := strconv.Atoi(strings.TrimSpace(r.Stdout))
	if !r.TimedOut || err != nil {
		t.Fatalf("run: %+v", r)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the background process %d outlived the run", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A run that exits by itself still takes what it left in the background with it.
func TestAFinishedRunLeavesNothingBehind(t *testing.T) {
	r := Exec(context.Background(), []string{"sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $!"}, "", os.Environ(), lim)
	pid, err := strconv.Atoi(strings.TrimSpace(r.Stdout))
	if r.ExitCode != 0 || err != nil {
		t.Fatalf("run: %+v", r)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the background process %d outlived the run", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// alive reports whether pid is a running process; a zombie (dead, not yet reaped by whoever
// inherited it) counts as gone.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	_, after, _ := strings.Cut(string(b), ") ")
	return !strings.HasPrefix(after, "Z")
}
