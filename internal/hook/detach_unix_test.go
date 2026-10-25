//go:build unix

package hook

import "os/exec"

func sysProcSetsid(cmd *exec.Cmd) bool { return cmd.SysProcAttr.Setsid }
