//go:build !unix

package cli

import (
	"os"
	"os/exec"
)

func ownLeaseProcessGroup(cmd *exec.Cmd)                   {}
func signalLeaseProcessGroup(cmd *exec.Cmd, sig os.Signal) { _ = cmd.Process.Signal(sig) }
func killLeaseProcessGroup(cmd *exec.Cmd)                  { _ = cmd.Process.Kill() }
