package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

const leaseKey = "deploy:prod:domainscope"

// leaseProc is `aivoicebus lease run` as session me, in a process of its own: a run that holds a
// lease while the test acts as another session, and one a signal can be sent to.
type leaseProc struct {
	cmd      *exec.Cmd
	out, err bytes.Buffer
}

func startLeaseRun(t *testing.T, me string, args ...string) *leaseProc {
	t.Helper()
	p := &leaseProc{cmd: asCLI(t, t.TempDir(), me, os.Getenv("AIVOICEBUS_URL"), append([]string{"lease", "run"}, args...)...)}
	p.cmd.Env = append(p.cmd.Env, "AIVOICEBUS_TOKEN=tok")
	p.cmd.Stdout, p.cmd.Stderr = &p.out, &p.err
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	return p
}

// wait is the exit code once the process is gone.
func (p *leaseProc) wait(t *testing.T) int {
	t.Helper()
	err := p.cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	}
	t.Fatalf("wait: %v", err)
	return -1
}

// waitFile blocks until the command a run started has made the file.
func waitFile(t *testing.T, path string) {
	t.Helper()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("%s did not appear", path)
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// While a run's command goes on, the key is the run's: another session is refused, with who holds it,
// why and until when, and its command does not run. When the command ends the key is free, and the
// run's exit code is the command's.
func TestLeaseRunHoldsTheKeyWhileTheCommandRunsThenReleasesIt(t *testing.T) {
	mesh(t)
	dir := t.TempDir()
	started, stop, ranB := filepath.Join(dir, "started"), filepath.Join(dir, "stop"), filepath.Join(dir, "ran-b")
	p := startLeaseRun(t, sa, leaseKey, "--ttl", "10m", "--note", "shipping 1.2.3", "--settle", "0s", "--",
		"sh", "-c", `echo "leases=$AIVOICEBUS_LEASES"; touch `+started+`; while [ ! -e `+stop+` ]; do sleep 0.02; done; exit 3`)
	waitFile(t, started)

	t.Setenv("AIVOICEBUS_ME", sb)
	if _, out, _ := run(t, "", "lease", "list"); !strings.Contains(out, leaseKey+" is held by "+sa) || !strings.Contains(out, "shipping 1.2.3") {
		t.Errorf("the run's lease is not listed: %q", out)
	}
	code, out, e := run(t, "", "lease", "run", leaseKey, "--settle", "0s", "--", "sh", "-c", "touch "+ranB)
	if code != exitLeaseHeld || out != "" || !strings.Contains(e, "held by "+sa) || !strings.Contains(e, "shipping 1.2.3") ||
		!strings.Contains(e, "until") || !strings.Contains(e, "--override-hold") || !strings.Contains(e, "did not run") {
		t.Errorf("a second run on a held key: %d %q %q", code, out, e)
	}
	if exists(ranB) {
		t.Error("the refused command ran")
	}
	if code, _, _ := run(t, "", "lease", "take", leaseKey, "--settle", "0s"); code == 0 {
		t.Error("take got a key a run holds")
	}

	if err := os.WriteFile(stop, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := p.wait(t); got != 3 {
		t.Errorf("exit code %d, want the command's 3\n%s", got, p.err.String())
	}
	if !strings.Contains(p.out.String(), "leases="+leaseKey) {
		t.Errorf("the command was not told its key: %q", p.out.String())
	}
	if _, out, _ := run(t, "", "lease", "list"); !strings.Contains(out, "(no leases held)") {
		t.Errorf("the lease outlived the command: %q", out)
	}
	if code, _, e := run(t, "", "lease", "run", leaseKey, "--settle", "0s", "--", "sh", "-c", "touch "+ranB); code != 0 || !exists(ranB) {
		t.Errorf("a run after the release: %d %q", code, e)
	}
}

// A signal sent to the run is passed on to the command, and the lease is released once the command
// is gone: the run that gets killed politely must not hold a deploy key until its ttl.
func TestLeaseRunReleasesOnSignals(t *testing.T) {
	for _, c := range []struct {
		sig  syscall.Signal
		code int
	}{{syscall.SIGTERM, 143}, {syscall.SIGHUP, 129}} {
		t.Run(c.sig.String(), func(t *testing.T) {
			mesh(t)
			started := filepath.Join(t.TempDir(), "started")
			p := startLeaseRun(t, sa, leaseKey, "--settle", "0s", "--", "sh", "-c", "touch "+started+"; exec sleep 60")
			waitFile(t, started)
			t.Setenv("AIVOICEBUS_ME", sb)
			if _, out, _ := run(t, "", "lease", "list"); !strings.Contains(out, leaseKey) {
				t.Fatalf("the lease is not held while the command runs: %q", out)
			}
			if err := p.cmd.Process.Signal(c.sig); err != nil {
				t.Fatal(err)
			}
			if got := p.wait(t); got != c.code {
				t.Errorf("exit code %d, want 128+signal = %d\n%s", got, c.code, p.err.String())
			}
			if _, out, _ := run(t, "", "lease", "list"); !strings.Contains(out, "(no leases held)") {
				t.Errorf("the lease survived %s: %q", c.sig, out)
			}
		})
	}
}

// A command that outlives the ttl keeps its lease: the run extends it, one live lease at a time.
func TestLeaseRunExtendsTheLeaseWhileTheCommandGoesOn(t *testing.T) {
	meshStore(t) // with a send limit high enough for an extension every 40ms
	t.Setenv("AIVOICEBUS_ME", sa)
	old := leaseRenewEvery
	leaseRenewEvery = func(time.Duration) time.Duration { return 40 * time.Millisecond }
	t.Cleanup(func() { leaseRenewEvery = old })

	done := make(chan int, 1)
	go func() {
		code, _, _ := run(t, "", "lease", "run", leaseKey, "--ttl", "1m", "--settle", "0s", "--", "sh", "-c", "sleep 1.5")
		done <- code
	}()
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	var first, last time.Time
	maxLive := 0
	for running := true; running; {
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("exit code %d", code)
			}
			running = false
		case <-time.After(25 * time.Millisecond):
			live, err := c.Leases()
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, l := range live {
				if l.Key == leaseKey {
					n++
				}
			}
			maxLive = max(maxLive, n)
			for _, h := range msg.Holders(live) {
				if h.Key == leaseKey {
					if first.IsZero() {
						first = h.Until
					}
					last = h.Until
				}
			}
		}
	}
	if first.IsZero() || last.Sub(first) < 500*time.Millisecond {
		t.Errorf("the lease was not extended while the command ran: until moved %s", last.Sub(first))
	}
	if maxLive > 2 {
		t.Errorf("%d live leases at once: extensions pile up instead of replacing each other", maxLive)
	}
	if live, _ := c.Leases(); len(live) != 0 {
		t.Errorf("leases left after the run: %+v", live)
	}
}

// --override-hold needs a reason, runs the command without the lease, leaves the holder's lease alone,
// and is recorded: in the audit log, and as a notice to the holder.
func TestLeaseRunOverrideHoldIsAuditedAndTellsTheHolder(t *testing.T) {
	s := meshStore(t)
	t.Setenv("AIVOICEBUS_ME", sb)
	if code, _, e := run(t, "", "lease", "take", leaseKey, "--note", "VACUUM of domains", "--settle", "0s"); code != 0 {
		t.Fatalf("take: %d %q", code, e)
	}
	t.Setenv("AIVOICEBUS_ME", sa)
	ran := filepath.Join(t.TempDir(), "ran")
	for _, why := range []string{"x", " too short ", ""} {
		if code, _, e := run(t, "", "lease", "run", leaseKey, "--override-hold", why, "--settle", "0s", "--", "sh", "-c", "touch "+ran); code != exitUsage || !strings.Contains(e, "reason") || exists(ran) {
			t.Errorf("override with the reason %q: %d %q ran=%v", why, code, e, exists(ran))
		}
	}
	reason := "Florin agreed: the hold is stale, hotfix"
	code, out, e := run(t, "", "lease", "run", leaseKey, "--override-hold", reason, "--settle", "0s", "--", "sh", "-c", "echo secret-arg-$AIVOICEBUS_LEASES; touch "+ran)
	if code != 0 || !exists(ran) || !strings.Contains(out, "secret-arg-"+leaseKey) || !strings.Contains(e, "audit") {
		t.Fatalf("override: %d out=%q err=%q ran=%v", code, out, e, exists(ran))
	}
	es, bad := config.ReadAudit()
	if len(bad) != 0 || len(es) != 1 || es[0].Action != "lease override" || es[0].Who != sa ||
		!strings.Contains(es[0].Detail, leaseKey) || !strings.Contains(es[0].Detail, sb) || !strings.Contains(es[0].Detail, reason) ||
		!strings.Contains(es[0].Detail, "VACUUM of domains") || !strings.Contains(es[0].Detail, "sh") {
		t.Errorf("audit: %+v %v", es, bad)
	}
	if strings.Contains(es[0].Detail, "touch") || strings.Contains(es[0].Detail, "secret-arg") {
		t.Errorf("the command's arguments are in the audit log: %q", es[0].Detail)
	}
	if _, out, _ := run(t, "", "lease", "list"); !strings.Contains(out, leaseKey+" is held by "+sb) {
		t.Errorf("the override took the holder's lease: %q", out)
	}
	var notice *msg.Message
	for _, m := range s.Inbox(sb, true) {
		if strings.HasPrefix(m.Body, "lease override:") {
			notice = &m
		}
	}
	if notice == nil || !strings.Contains(notice.Body, leaseKey) || !strings.Contains(notice.Body, reason) || notice.From != sa {
		t.Fatalf("the holder was not told: %+v", notice)
	}
	if why := msg.HoldReason(*notice); why != "" {
		t.Errorf("the notice would be held back from the agent it is for: %s", why)
	}
	// With nothing to override the key is taken as usual, and nothing is recorded.
	t.Setenv("AIVOICEBUS_ME", sb)
	run(t, "", "lease", "release", leaseKey)
	t.Setenv("AIVOICEBUS_ME", sa)
	if code, _, e := run(t, "", "lease", "run", leaseKey, "--override-hold", reason, "--settle", "0s", "--", "true"); code != 0 {
		t.Errorf("override of a free key: %d %q", code, e)
	}
	if es, _ := config.ReadAudit(); len(es) != 1 {
		t.Errorf("an override that overrode nothing was recorded: %+v", es)
	}
}

// With no node to ask there is no way to tell the key is free: the command does not run (code 4,
// like any call that reaches no node), unless a person said to run it anyway, which is recorded.
func TestLeaseRunWithNoNodeRefusesUnlessOverridden(t *testing.T) {
	mesh(t)
	dead := httptest.NewServer(nil)
	dead.Close()
	t.Setenv("AIVOICEBUS_ME", sa)
	for _, c := range []struct {
		name, url string
		code      int
	}{{"a node that does not answer", dead.URL, exitUnreachable}, {"no node configured", "", exitUsage}} {
		t.Setenv("AIVOICEBUS_URL", c.url)
		ran := filepath.Join(t.TempDir(), "ran")
		code, _, e := run(t, "", "lease", "run", leaseKey, "--", "sh", "-c", "touch "+ran)
		if code != c.code || exists(ran) || !strings.Contains(e, "did not run") {
			t.Errorf("%s: %d %q ran=%v", c.name, code, e, exists(ran))
		}
		code, _, e = run(t, "", "lease", "run", leaseKey, "--override-hold", "the hub is down and the rollback cannot wait", "--", "sh", "-c", "touch "+ran)
		if code != 0 || !exists(ran) {
			t.Errorf("%s, overridden: %d %q ran=%v", c.name, code, e, exists(ran))
		}
	}
	if es, _ := config.ReadAudit(); len(es) != 2 || !strings.Contains(es[0].Detail, "could not be asked") || !strings.Contains(es[1].Detail, "could not be asked") {
		t.Errorf("audit: %+v", es)
	}
}

// A lease this session already holds is not taken again: two runs of one session at once are the
// double deploy the lease is for. The message says how to free it when the earlier run is gone.
func TestLeaseRunRefusesAKeyThisSessionHolds(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", sa)
	run(t, "", "lease", "take", leaseKey, "--settle", "0s")
	ran := filepath.Join(t.TempDir(), "ran")
	code, _, e := run(t, "", "lease", "run", leaseKey, "--settle", "0s", "--", "sh", "-c", "touch "+ran)
	if code != exitLeaseHeld || exists(ran) || !strings.Contains(e, "held by you") || !strings.Contains(e, "lease release "+leaseKey) {
		t.Errorf("%d %q ran=%v", code, e, exists(ran))
	}
	// and a refused run does not release what it never took
	if _, out, _ := run(t, "", "lease", "list"); !strings.Contains(out, leaseKey+" is held by you") {
		t.Errorf("the earlier lease is gone: %q", out)
	}
}

// The command's own failures keep their codes, a command that cannot start gives the shell's codes
// and leaves the key free, and a misuse is a usage error that runs nothing.
func TestLeaseRunExitCodesAndUsage(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", sa)
	dir := t.TempDir()
	noexec := filepath.Join(dir, "noexec")
	if err := os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		args []string
		code int
		out  string
	}{
		{"its own code", []string{"lease", "run", leaseKey, "--settle", "0s", "--", "sh", "-c", "exit 42"}, 42, ""},
		{"output and stdin pass through", []string{"lease", "run", leaseKey, "--settle", "0s", "--", "sh", "-c", "cat; echo to-stderr >&2"}, 0, "from stdin"},
		{"a flag of the command is the command's", []string{"lease", "run", leaseKey, "--settle", "0s", "--", "sh", "-c", `[ "$1" = --ttl ]`, "sh", "--ttl", "x"}, 0, ""},
		{"not found", []string{"lease", "run", leaseKey, "--settle", "0s", "--", filepath.Join(dir, "missing")}, 127, ""},
		{"not executable", []string{"lease", "run", leaseKey, "--settle", "0s", "--", noexec}, 126, ""},
		{"no --", []string{"lease", "run", leaseKey, "sh", "-c", "true"}, exitUsage, ""},
		{"nothing after --", []string{"lease", "run", leaseKey, "--"}, exitUsage, ""},
		{"no key", []string{"lease", "run", "--", "true"}, exitUsage, ""},
		{"a key with a space", []string{"lease", "run", "a b", "--", "true"}, exitUsage, ""},
		{"two keys", []string{"lease", "run", leaseKey, "other", "--", "true"}, exitUsage, ""},
		{"a ttl under a minute", []string{"lease", "run", leaseKey, "--ttl", "5s", "--", "true"}, exitUsage, ""},
	} {
		code, out, e := run(t, "from stdin", c.args...)
		if code != c.code || !strings.Contains(out, c.out) {
			t.Errorf("%s: code %d out %q err %q, want code %d", c.name, code, out, e, c.code)
		}
		if _, lease, _ := run(t, "", "lease", "list"); !strings.Contains(lease, "(no leases held)") {
			t.Errorf("%s: a lease was left behind: %q", c.name, lease)
		}
	}
}

// `lease run -h` lists the flags and exits 0, before the `--` and without running anything: it is what a
// script probes for, because an aivoicebus older than `lease run` answers it with a usage error (2).
func TestLeaseRunHelpIsAProbeThatRunsNothing(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", sa)
	ran := filepath.Join(t.TempDir(), "ran")
	for _, args := range [][]string{
		{"lease", "run", "-h"},
		{"lease", "run", "--help"},
		{"lease", "run", leaseKey, "-h", "--", "sh", "-c", "touch " + ran},
		{"lease", "run", "-h", "--", "sh", "-c", "touch " + ran},
	} {
		code, out, e := run(t, "", args...)
		if code != 0 || !strings.Contains(e, "override-hold") || out != "" || exists(ran) {
			t.Errorf("%v: %d out=%q err=%q ran=%v", args, code, out, e, exists(ran))
		}
	}
	// a -h that belongs to the command is the command's
	if code, _, e := run(t, "", "lease", "run", leaseKey, "--settle", "0s", "--", "sh", "-c", `[ "$1" = -h ]`, "sh", "-h"); code != 0 {
		t.Errorf("-h after the --: %d %q", code, e)
	}
}

// `lease list` takes globs: `*` for any run (a `/` or `:` too) and `?` for one character; several
// globs add up, and flags may come after them.
func TestLeaseListGlobs(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", sa)
	for _, k := range []string{"load:prod/scan-a", "load:prod/backfill", "load:staging/scan-b", leaseKey, "hub:server.go"} {
		if code, _, e := run(t, "", "lease", "take", k, "--settle", "0s"); code != 0 {
			t.Fatalf("take %s: %d %q", k, code, e)
		}
	}
	lines := func(args ...string) []string {
		t.Helper()
		code, out, e := run(t, "", append([]string{"lease", "list"}, args...)...)
		if code != 0 {
			t.Fatalf("list %v: %d %q", args, code, e)
		}
		var keys []string
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			keys = append(keys, strings.Fields(l)[0])
		}
		return keys
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "deploy:prod:domainscope hub:server.go load:prod/backfill load:prod/scan-a load:staging/scan-b"},
		{[]string{"load:prod/*"}, "load:prod/backfill load:prod/scan-a"},
		{[]string{"load:*"}, "load:prod/backfill load:prod/scan-a load:staging/scan-b"},
		{[]string{"load:prod/*", "deploy:*"}, "deploy:prod:domainscope load:prod/backfill load:prod/scan-a"},
		{[]string{"*:prod:*"}, "deploy:prod:domainscope"},
		{[]string{"hub:server.g?"}, "hub:server.go"},
		{[]string{"hub:server.go"}, "hub:server.go"},
	} {
		if got := strings.Join(lines(c.args...), " "); got != c.want {
			t.Errorf("lease list %v:\n got %s\nwant %s", c.args, got, c.want)
		}
	}
	if _, out, _ := run(t, "", "lease", "list", "nothing:*"); strings.TrimSpace(out) != "(no leases held that match nothing:*)" {
		t.Errorf("no match: %q", out)
	}
	// --json, before or after the glob, lists the matching holders only
	for _, args := range [][]string{{"--json", "load:prod/*"}, {"load:prod/*", "--json"}} {
		var hs []msg.Lease
		if _, out, _ := run(t, "", append([]string{"lease", "list"}, args...)...); json.Unmarshal([]byte(out), &hs) != nil || len(hs) != 2 {
			t.Errorf("lease list %v: %q", args, out)
		}
	}
	// a key that is not held but matches: the glob is not a key
	if _, out, _ := run(t, "", "lease", "list", "load:prod/scan-?"); !strings.Contains(out, "load:prod/scan-a") || strings.Contains(out, "backfill") {
		t.Errorf("%q", out)
	}
}
