package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

// `status` with no id is the home screen, as text and as JSON, on this machine's own settings.
func TestStatusHomeScreen(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "desk/claude.1")
	if err := config.SaveQuiet(config.Quiet{From: 22 * 60, To: 7 * 60}); err != nil {
		t.Fatal(err)
	}
	code, out, e := run(t, "", "status")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	for _, want := range []string{"aivoicebus status (desk/claude.1, ", "  me       desk/claude.1", "  nodes    1 of 1 up", "  inbox    nothing unread",
		"  worker   not running (service not installed)", "  quiet    22:00-07:00 daily"} {
		if !strings.Contains(out, want) {
			t.Errorf("screen lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "\n"); n > 20 {
		t.Errorf("%d lines:\n%s", n, out)
	}

	code, out, _ = run(t, "", "status", "--json")
	var r struct {
		Me struct {
			ID string `json:"id"`
		} `json:"me"`
		Quiet struct {
			Window string `json:"window"`
		} `json:"quiet"`
		Unknown []string `json:"unknown"`
	}
	if err := json.Unmarshal([]byte(out), &r); code != 0 || err != nil || r.Me.ID != "desk/claude.1" || r.Quiet.Window != "22:00-07:00" || len(r.Unknown) != 0 {
		t.Errorf("json: exit %d, %v, %+v\n%s", code, err, r, out)
	}

	if code, _, e := run(t, "", "status", "--budget", "1ms"); code != exitUsage || !strings.Contains(e, "--budget") {
		t.Errorf("a budget too short: %d %s", code, e)
	}
	if code, _, e := run(t, "", "status", "a", "b"); code != exitUsage || !strings.Contains(e, "status MESSAGE_ID") {
		t.Errorf("two words: %d %s", code, e)
	}
}

// The worker's lock tells a worker started by hand from none, without making the lock file.
func TestStatusSeesAWorkerByItsLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	lock := filepath.Join(config.Dir(), ".worker.lock")
	if w := localWorker(); w.Running || w.Service != "not installed" {
		t.Errorf("no worker: %+v", w)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("looking made the lock: %v", err)
	}
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok := config.Lock(lock, 0)
	if !ok {
		t.Fatal("cannot take the lock")
	}
	if w := localWorker(); !w.Running || w.Detail != "started by hand" {
		t.Errorf("a worker holds the lock: %+v", w)
	}
	unlock()
	if w := localWorker(); w.Running {
		t.Errorf("the worker stopped: %+v", w)
	}
}

// The no-node case: the screen still shows what this machine knows, and says how to join.
func TestStatusWithoutNodes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", "")
	t.Setenv("AIVOICEBUS_ME", "desk/claude.1")
	code, out, e := run(t, "", "status")
	if code != 0 || !strings.Contains(out, "aivoicebus join --url URL") || !strings.Contains(out, "unknown: no node is configured") {
		t.Errorf("exit %d %s\n%s", code, e, out)
	}
}
