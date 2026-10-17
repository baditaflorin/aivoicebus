package worker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitCommand(t *testing.T) {
	for in, want := range map[string][]string{
		`claude -p`:       {"claude", "-p"},
		`  claude   -p  `: {"claude", "-p"},
		`claude -p --append-system-prompt "untrusted input: be careful"`: {"claude", "-p", "--append-system-prompt", "untrusted input: be careful"},
		`sh -c 'cat; echo "$HOME"'`:                                      {"sh", "-c", `cat; echo "$HOME"`},
		`a "x\"y\\z" b\ c ''`:                                            {"a", `x"y\z`, "b c", ""},
		`echo $(id) ; rm -rf ~ | tee >x`:                                 {"echo", "$(id)", ";", "rm", "-rf", "~", "|", "tee", ">x"},
	} {
		got, err := SplitCommand(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("SplitCommand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", `a 'b`, `a "b`, `a \`} {
		if _, err := SplitCommand(in); err == nil {
			t.Errorf("SplitCommand(%q) was accepted", in)
		}
	}
	if got := QuoteCommand([]string{"claude", "-p", "it's here"}); got != `claude -p 'it'\''s here'` {
		t.Errorf("QuoteCommand: %s", got)
	}
}

var lim = Limits{Timeout: 10 * time.Second, MaxOutput: 1000}

func TestExecGivesTheTaskOnStdinAndNothingInterpretsIt(t *testing.T) {
	dir := t.TempDir()
	mark := filepath.Join(dir, "pwned")
	task := "do this $(touch " + mark + ") `touch " + mark + "`; touch " + mark + "\nsecond line"
	r := Exec(context.Background(), []string{"cat"}, task, os.Environ(), lim)
	if r.ExitCode != 0 || r.Stdout != task {
		t.Fatalf("cat: %+v", r)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("the task text was run by a shell")
	}
	r = Exec(context.Background(), []string{"sh", "-c", `printf '%s|' "$#" "$AIVOICEBUS_TASK_ID"; exit 3`}, task, append(os.Environ(), "AIVOICEBUS_TASK_ID=0123456789"), lim)
	if r.ExitCode != 3 || r.Stdout != "0|0123456789|" {
		t.Errorf("args and env: %+v", r)
	}
	if !strings.Contains(r.Describe(lim.Timeout), "exit 3") {
		t.Errorf("describe: %s", r.Describe(lim.Timeout))
	}
	r = Exec(context.Background(), []string{filepath.Join(dir, "no-such-command")}, task, nil, lim)
	if r.StartErr == nil || !strings.Contains(r.Describe(lim.Timeout), "could not start") {
		t.Errorf("missing command: %+v", r)
	}
}

func TestExecCapsTheOutput(t *testing.T) {
	r := Exec(context.Background(), []string{"sh", "-c", "head -c 200000 /dev/zero | tr '\\0' x; echo oops >&2"}, "", os.Environ(), Limits{Timeout: 10 * time.Second, MaxOutput: 100})
	if r.ExitCode != 0 || len(r.Stdout) != 100 || !r.Truncated || r.Stderr != "oops\n" {
		t.Errorf("capped run: exit %d, %d bytes, truncated %v, stderr %q", r.ExitCode, len(r.Stdout), r.Truncated, r.Stderr)
	}
	c := &capped{max: 4}
	c.Write([]byte("aé€")) // a, then é (2 bytes), then the first byte of €
	if c.String() != "aé" || !c.over {
		t.Errorf("cut inside a character: %q %v", c.String(), c.over)
	}
}

func TestExecStopsAtTheTimeLimitAndWhenStopped(t *testing.T) {
	start := time.Now()
	r := Exec(context.Background(), []string{"sh", "-c", "echo started; sleep 30"}, "", os.Environ(), Limits{Timeout: 300 * time.Millisecond, MaxOutput: 1000})
	if !r.TimedOut || r.ExitCode != -1 || r.Stdout != "started\n" || time.Since(start) > 8*time.Second {
		t.Errorf("timeout: %+v after %s", r, time.Since(start))
	}
	if !strings.Contains(r.Describe(300*time.Millisecond), "time limit") {
		t.Errorf("describe: %s", r.Describe(300*time.Millisecond))
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	r = Exec(ctx, []string{"sleep", "30"}, "", os.Environ(), lim)
	if !r.Stopped || r.TimedOut || !strings.Contains(r.Describe(lim.Timeout), "shut down") {
		t.Errorf("stopped: %+v", r)
	}
}
