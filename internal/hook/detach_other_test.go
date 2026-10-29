//go:build !unix

package hook

import "os/exec"

func sysProcSetsid(*exec.Cmd) bool { return false }
