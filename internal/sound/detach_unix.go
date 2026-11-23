//go:build unix

package sound

import (
	"os/exec"
	"syscall"
)

// detach puts the player in a session of its own, so the tool ending a hook's process group does not
// cut the sound short.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
