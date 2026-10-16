//go:build unix

package hook

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// stalledNote makes hook-status.json a FIFO nobody writes to: every read of it blocks for good,
// as a file on a hung network mount would. The returned func unblocks the stragglers.
func stalledNote(t *testing.T) {
	t.Helper()
	p := config.HookStatusPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("no fifos here: %v", err)
	}
	t.Cleanup(func() { // let the readers that are stuck on it finish, so they do not outlive the test
		if f, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
}

// A note on a stalled disk must cost a session at most noteBudget twice: the hook still shows
// the message it fetched, and still acknowledges it, long before its own deadline.
func TestAStalledNoteNeitherHidesMessagesNorDelaysTheAcks(t *testing.T) {
	s, _ := mesh(t)
	s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "still delivered"})
	stalledNote(t)
	at(t, t0)

	start := time.Now()
	got := runHook(t, "")
	if took := time.Since(start); took > Deadline/2 {
		t.Errorf("the hook took %v on a stalled note (deadline %v)", took, Deadline)
	}
	if !strings.Contains(got, "still delivered") {
		t.Errorf("the message must arrive whatever the disk does: %q", got)
	}
	if ms := s.InboxFor("air/codex", false, nil); len(ms) != 0 {
		t.Errorf("the message was shown, so it is acknowledged: %d unread", len(ms))
	}
}

// The deadline path took the note's lock while the run held it across a read of the file, so a
// stalled disk kept the hook past its deadline. It must not.
func TestAStalledNoteDoesNotKeepAHookPastItsDeadline(t *testing.T) {
	stalledNote(t)
	t.Setenv("AIVOICEBUS_URL", hang(t))
	old := Deadline
	Deadline = 300 * time.Millisecond
	defer func() { Deadline = old }()
	at(t, t0)

	start := time.Now()
	if got := runHook(t, ""); got != "" {
		t.Errorf("%q", got)
	}
	if took := time.Since(start); took > Deadline+time.Second {
		t.Errorf("the hook took %v with a %v deadline", took, Deadline)
	}
}
