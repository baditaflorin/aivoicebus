package cli

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// demoLeftNothing fails t if anything of a demo is left: its directory, or a port that answers.
func demoLeftNothing(t *testing.T, dir string, addrs []string) {
	t.Helper()
	if dir == "" || len(addrs) != 3 {
		t.Fatalf("the demo did not say what it started: dir %q, addrs %v", dir, addrs)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s is still there (%v)", dir, err)
	}
	for _, a := range addrs {
		if c, err := net.DialTimeout("tcp", a, time.Second); err == nil {
			c.Close()
			t.Errorf("%s still accepts connections", a)
		}
	}
}

// The demo plays the whole conversation through real nodes, and afterwards nothing of it is left
// and nothing of the machine's own aivoicebus setup was read or written.
func TestDemoPlaysTheScriptAndLeavesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_URL", "http://192.0.2.1:7777") // a real mesh's settings: never used
	t.Setenv("AIVOICEBUS_TOKEN", "do-not-use-this-token-0123456789")
	var dir string
	var addrs []string
	var out, errb strings.Builder
	err := runDemo(context.Background(), demoOptions{Ready: func(d string, a []string) { dir, addrs = d, a }}, Streams{Out: &out, Err: &errb})
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out.String(), errb.String())
	}
	o := out.String()
	for _, want := range []string{
		"nothing of this machine's config, token or mesh is used",
		"viewer (read-only): http://127.0.0.1:",
		`alice runs ` + "`who`" + `: buildbox/codex.c3d4 "Build box helper" (listening)`,
		"gives the machine buildbox a task",
		"later: demo-b got the task from demo-a by replication",
		"bob claims it",
		"progress",
		"alice fetched it and its sha256 matches",
		"bob answers: \"2 failures",
		`alice: "thanks!"`,
		`bob: "you're welcome!"`,
		"the brakes: the node refused alice's third \"thanks!\"",
		"looks like a loop",
		"alice closes the conversation (end)",
		"cleaned up: the 2 nodes and the viewer are stopped",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("missing %q in:\n%s", want, o)
		}
	}
	if errb.Len() != 0 {
		t.Errorf("stderr: %s", errb.String())
	}
	demoLeftNothing(t, dir, addrs)
	if os.Getenv("HOME") != home || os.Getenv("AIVOICEBUS_URL") != "http://192.0.2.1:7777" || os.Getenv("AIVOICEBUS_PIPE_URL") != "off" {
		t.Error("the demo did not put the environment back")
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("the demo wrote into the real home: %v", entries)
	}
}

// Ctrl-C in the middle of the script stops it and cleans up just the same.
func TestDemoInterruptedMidwayCleansUp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dir string
	var addrs []string
	var out strings.Builder
	err := runDemo(ctx, demoOptions{Pace: 300 * time.Millisecond, Ready: func(d string, a []string) {
		dir, addrs = d, a
		time.AfterFunc(700*time.Millisecond, cancel)
	}}, Streams{Out: &out, Err: &out})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "interrupted") || strings.Contains(out.String(), "closes the conversation") {
		t.Errorf("it was not cut short:\n%s", out.String())
	}
	demoLeftNothing(t, dir, addrs)
}

// `demo --keep` runs as its own process until Ctrl-C; then the process exits 0, having started no
// other process, and its directory and ports are gone.
func TestDemoKeepRunsUntilCtrlCAndLeavesNoProcess(t *testing.T) {
	cmd := asCLI(t, t.TempDir(), "tester/claude.0001", "", "demo", "--keep", "--pace", "0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	var all strings.Builder
	var mu sync.Mutex
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		once := sync.Once{}
		for sc.Scan() {
			mu.Lock()
			all.WriteString(sc.Text() + "\n")
			mu.Unlock()
			if strings.HasPrefix(sc.Text(), "Ctrl-C stops it") {
				once.Do(func() { close(ready) })
			}
		}
		exited <- cmd.Wait()
	}()
	select {
	case <-ready:
	case err := <-exited:
		t.Fatalf("it ended before Ctrl-C: %v\n%s\n%s", err, all.String(), stderr.String())
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("no --keep prompt in 60s:\n%s", all.String())
	}
	mu.Lock()
	text := all.String()
	mu.Unlock()
	dir := regexp.MustCompile(`data in (\S+)`).FindStringSubmatch(text)
	if dir == nil {
		t.Fatalf("no data dir in:\n%s", text)
	}
	if _, err := os.Stat(dir[1]); err != nil {
		t.Fatalf("while it runs, its directory is there: %v", err)
	}
	var addrs []string
	for _, m := range regexp.MustCompile(`http://(127\.0\.0\.1:\d+)`).FindAllStringSubmatch(text, -1) {
		if !strings.Contains(strings.Join(addrs, " "), m[1]) {
			addrs = append(addrs, m[1])
		}
	}
	if kids := childrenOf(cmd.Process.Pid); kids > 0 {
		t.Errorf("the demo started %d other process(es)", kids)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit after Ctrl-C: %v\n%s", err, stderr.String())
		}
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("still running 30s after Ctrl-C")
	}
	if !strings.Contains(all.String(), "cleaned up:") {
		t.Errorf("no cleanup line:\n%s", all.String())
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err == nil {
		t.Error("the process is still there")
	}
	demoLeftNothing(t, dir[1], addrs)
}

// childrenOf counts the processes whose parent is pid (Linux /proc; 0 where there is none).
func childrenOf(pid int) int {
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	n := 0
	for _, f := range stats {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...: comm may hold spaces, so read after the last ')'
		s := string(b)
		fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(fields) > 1 {
			if ppid, _ := strconv.Atoi(fields[1]); ppid == pid {
				n++
			}
		}
	}
	return n
}
