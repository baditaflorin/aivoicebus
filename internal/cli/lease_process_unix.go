//go:build unix

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

func ownLeaseProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalLeaseProcessGroup(cmd *exec.Cmd, sig os.Signal) {
	if n, ok := sig.(syscall.Signal); ok && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, n)
	}
}

func killLeaseProcessGroup(cmd *exec.Cmd) { signalLeaseProcessGroup(cmd, syscall.SIGKILL) }
