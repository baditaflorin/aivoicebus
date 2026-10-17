//go:build unix

package worker

import (
	"os/exec"
	"syscall"
)

// ownGroup puts the command in a process group of its own, so a time limit kills everything it
// started, not just the first process.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// killGroup ends whatever the run left behind in its group once the command itself has exited.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
