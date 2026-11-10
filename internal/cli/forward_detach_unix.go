//go:build unix

package cli

import (
	"os/exec"
	"syscall"
)

// detachForwarder puts cmd in a session of its own, so the shell that started it ending does not end it.
func detachForwarder(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
