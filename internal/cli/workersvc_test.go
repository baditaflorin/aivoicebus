package cli

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/worker"
)

// The service files carry the command word for word: a quote, a dollar or an ampersand in --exec
// must reach the worker as written, not be read by launchd's XML or systemd's own expansion.
func TestTheServiceFilesKeepTheCommandAsWritten(t *testing.T) {
	sp := svcSpec{
		Argv: []string{"/h/.local/bin/aivoicebus", "worker", "--exec", `claude -p --append-system-prompt 'say "hi" & cost $5 at 100%'`},
		Dir:  "/h/aivoicebus-worker", Path: "/opt/x bin:/usr/bin", URLs: "http://10.0.0.5:7777,http://10.0.0.6:7777", Log: "/h/Library/Logs/aivoicebus-worker.log",
	}
	pl := launchdPlist(sp)
	for _, want := range []string{
		"<string>--exec</string>",
		"<string>claude -p --append-system-prompt &#39;say &#34;hi&#34; &amp; cost $5 at 100%&#39;</string>",
		"<key>AIVOICEBUS_URL</key>\n    <string>http://10.0.0.5:7777,http://10.0.0.6:7777</string>",
		"<string>/h/aivoicebus-worker</string>",
	} {
		if !strings.Contains(pl, want) {
			t.Errorf("plist lacks %q:\n%s", want, pl)
		}
	}
	u := systemdUnit(sp)
	for _, want := range []string{
		`ExecStart=/h/.local/bin/aivoicebus worker --exec "claude -p --append-system-prompt 'say \"hi\" & cost $$5 at 100%%'"`,
		`Environment="PATH=/opt/x bin:/usr/bin"`,
		"WorkingDirectory=/h/aivoicebus-worker",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
}

// install refuses what would make a service that runs nothing or cannot start.
func TestWorkerInstallRefusesWhatCannotWork(t *testing.T) {
	mesh(t)
	if code, _, e := run(t, "", "worker", "install"); code != 2 || !strings.Contains(e, "usage: worker install") {
		t.Errorf("no agent: %d %s", code, e)
	}
	if code, _, e := run(t, "", "worker", "install", "--agent", "codex"); code == 0 || !strings.Contains(e, "run `aivoicebus setup` first") {
		t.Errorf("no installed binary: %d %s", code, e)
	}
	home, _ := os.UserHomeDir()
	os.MkdirAll(home+"/.local/bin", 0o755)
	os.WriteFile(home+"/.local/bin/aivoicebus", []byte("#!/bin/sh\n"), 0o755)
	if code, _, e := run(t, "", "worker", "install", "--exec", "/bin/cat"); code == 0 || !strings.Contains(e, "no sender is allowed") {
		t.Errorf("empty allowlist: %d %s", code, e)
	}
	code, out, e := run(t, "", "worker", "install", "--exec", "/bin/cat", "--timeout", "2m", "--dry-run")
	if code != 0 || !strings.Contains(out, "would write") || !strings.Contains(out, "--timeout") || !strings.Contains(out, os.Getenv("AIVOICEBUS_URL")) {
		t.Errorf("dry run: %d %s %s", code, out, e)
	}
}

// With a node that holds reads open, the running worker answers a task within a second or two of
// it being sent, not at its next poll.
func TestTheWorkerTakesATaskAsSoonAsItArrives(t *testing.T) {
	workerSetup(t, "desk")
	t.Setenv("AIVOICEBUS_ME", wk)
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	allow, err := worker.LoadAllow()
	if err != nil {
		t.Fatal(err)
	}
	w := &worker.Worker{C: c, Me: wk, Allow: allow, Argv: []string{"/bin/cat"}, Env: os.Environ(),
		Limits: worker.Limits{Timeout: 10 * time.Second, MaxOutput: 1000}, MaxAge: time.Hour, Out: io.Discard}
	if w.Log, err = worker.FileLog(worker.LogPath()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Loop(ctx, time.Minute) }() // a poll a minute: only a held read answers in time
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("loop: %v", err)
		}
	}()
	time.Sleep(1500 * time.Millisecond) // the worker is in its held read now
	sent := time.Now()
	id := sendTask(t, boss, "lap", "ping-held-read") // (this flips AIVOICEBUS_ME: the run is read from the worker's log)
	for time.Since(sent) < 8*time.Second {
		for _, e := range logEntries(t) {
			if e.Task == id && e.Outcome == "ran" {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the worker did not answer within 8s: it is polling, not holding a read open")
}
