package hook

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// The person may be looking at no agent's session at all, so on macOS a hook that names tasks
// waiting for the person also raises a notification: once per task on this machine, whichever
// session saw it first. Its text is fixed (how many, and where to look); nothing a sender wrote
// reaches it, and the words go to osascript as an argument, never as script.

// notifiedAs is the name the notifications are recorded under (config.Told), one for the machine.
const notifiedAs = "macos-notification"

// notifyPerson raises the notification for n new tasks (macOS only); a variable so tests can count instead.
var notifyPerson = func(n int) {
	if runtime.GOOS != "darwin" {
		return
	}
	text := fmt.Sprintf("%d new task(s) wait for you, the person on this machine. In your own terminal: aivoicebus status", n)
	cmd := exec.Command("osascript", "-e", "on run argv", "-e", `display notification (item 1 of argv) with title "aivoicebus"`, "-e", "end run", text)
	detach(cmd)
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// notifyNew raises one notification for the tasks of ps not notified on this machine yet, and
// records them.
func notifyNew(ps []msg.Message) {
	fresh := 0
	for _, m := range ps {
		if !config.Told(notifiedAs, m.ID) {
			fresh++
			config.NoteTold(notifiedAs, m.ID)
		}
	}
	if fresh > 0 {
		notifyPerson(fresh)
	}
}
