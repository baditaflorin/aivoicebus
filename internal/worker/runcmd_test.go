package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func writeSpec(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

const brightSpec = `{"argv":["/usr/bin/python3","-I","/opt/bright.py","{0}"],"args":["^(0(\\.[0-9]+)?|1(\\.0+)?)$"],"sandbox":false,"timeout":"30s"}`

func TestRunRequestRoundTripsAndRefusesJunk(t *testing.T) {
	body, err := EncodeRun("brightness", []string{"1"})
	if err != nil || !IsRun(body) {
		t.Fatalf("encode: %q %v", body, err)
	}
	r, err := ParseRun(body)
	if err != nil || r.Cmd != "brightness" || len(r.Args) != 1 || r.Args[0] != "1" {
		t.Fatalf("parse: %+v %v", r, err)
	}
	for _, bad := range []string{
		`[run] {"cmd":"Bad Name"}`,
		`[run] {"cmd":"x","extra":1}`,
		`[run] {"cmd":"x"} trailing`,
		`[run] not json`,
	} {
		if _, err := ParseRun(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := EncodeRun("../etc", nil); err == nil {
		t.Error("encoded a path as a name")
	}
}

func TestResolveChecksNameCountAndEveryArgument(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "brightness", brightSpec, 0o600)
	rs, err := LoadRunSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	argv, sp, err := rs.Resolve(RunRequest{Cmd: "brightness", Args: []string{"0.5"}})
	if err != nil || strings.Join(argv, " ") != "/usr/bin/python3 -I /opt/bright.py 0.5" {
		t.Fatalf("resolve: %v %v", argv, err)
	}
	if sp.UseSandbox() || sp.Limit() != 30*time.Second {
		t.Errorf("spec: sandbox=%v timeout=%v", sp.UseSandbox(), sp.Limit())
	}
	for _, c := range []RunRequest{
		{Cmd: "nope", Args: []string{"1"}},
		{Cmd: "brightness"},
		{Cmd: "brightness", Args: []string{"1", "2"}},
		{Cmd: "brightness", Args: []string{"2"}},
		{Cmd: "brightness", Args: []string{"1; rm -rf ~"}},
		{Cmd: "brightness", Args: []string{"1\n"}},
		{Cmd: "brightness", Args: []string{"x1"}},
	} {
		if _, _, err := rs.Resolve(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestLoadRunSetRefusesUnsafeFiles(t *testing.T) {
	cases := map[string]struct {
		name, body string
		mode       os.FileMode
		want       string
	}{
		"relative argv":  {"a", `{"argv":["python3"]}`, 0o600, "absolute"},
		"unknown field":  {"a", `{"argv":["/bin/true"],"shell":true}`, 0o600, "unknown field"},
		"bad name":       {"A_b", `{"argv":["/bin/true"]}`, 0o600, "command name"},
		"group writable": {"a", `{"argv":["/bin/true"]}`, 0o660, "writable"},
		"placeholder":    {"a", `{"argv":["/bin/echo","{0}"]}`, 0o600, "{0}"},
		"bad pattern":    {"a", `{"argv":["/bin/true"],"args":["("]}`, 0o600, "pattern"},
		"bad timeout":    {"a", `{"argv":["/bin/true"],"timeout":"soon"}`, 0o600, "timeout"},
	}
	for label, c := range cases {
		dir := t.TempDir()
		writeSpec(t, dir, c.name, c.body, c.mode)
		if err := os.Chmod(filepath.Join(dir, c.name+".json"), c.mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRunSet(dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", label, err)
		}
	}
	dir := t.TempDir()
	if _, err := LoadRunSet(dir); err == nil {
		t.Error("an empty run.d loaded")
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "x.json")); err == nil {
		if _, err := LoadRunSet(dir); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("symlink: %v", err)
		}
	}
}

func TestTimeoutIsCapped(t *testing.T) {
	sp, err := parseSpec("a", []byte(`{"argv":["/bin/true"],"timeout":"24h"}`))
	if err != nil || sp.Limit() != MaxRunTimeout {
		t.Fatalf("%v %v", sp, err)
	}
}

// A [run] task never reaches the agent: with no --run it is skipped, and a run-only worker skips
// everything that is not a [run] task.
func TestDecideKeepsRunTasksAwayFromTheAgent(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	a, _ := ParseAllow("desk\n")
	mk := func(body string) msg.Message {
		return msg.Message{ID: "0123456789", Kind: msg.KindTask, From: "desk/claude.z9", To: "lap", At: now, Verified: msg.VerifiedSigned, Body: body}
	}
	run, _ := EncodeRun("brightness", []string{"1"})
	agent := &Worker{Me: "lap/claude.worker", Allow: a, Argv: []string{"claude", "-p"}, Now: func() time.Time { return now }}
	if why, _ := agent.Decide(mk(run)); !strings.Contains(why, "--run") {
		t.Errorf("an agent worker without --run took a [run] task: %q", why)
	}
	if why, _ := agent.Decide(mk("check the disk")); why != "" {
		t.Errorf("plain task refused: %q", why)
	}
	dir := t.TempDir()
	writeSpec(t, dir, "brightness", brightSpec, 0o600)
	rs, _ := LoadRunSet(dir)
	only := &Worker{Me: "lap/worker", Allow: a, Run: rs, Now: func() time.Time { return now }}
	if why, _ := only.Decide(mk(run)); why != "" {
		t.Errorf("run-only worker refused a [run] task: %q", why)
	}
	if why, _ := only.Decide(mk("check the disk")); !strings.Contains(why, "named commands only") {
		t.Errorf("run-only worker took a plain task: %q", why)
	}
	both := &Worker{Me: "lap/claude.worker", Allow: a, Argv: []string{"claude", "-p"}, Run: rs, Now: func() time.Time { return now }}
	if why, _ := both.Decide(mk(run)); why != "" {
		t.Errorf("worker with agent and --run refused a [run] task: %q", why)
	}
	if why, _ := both.Decide(mk("check the disk")); why != "" {
		t.Errorf("worker with agent and --run refused a plain task: %q", why)
	}
}
