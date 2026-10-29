//go:build unix

package hook

import (
	"os/exec"
	"syscall"
)

// detach puts cmd in a session of its own, so the tool ending the hook's process group does not end it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
