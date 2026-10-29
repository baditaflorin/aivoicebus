//go:build !unix

package hook

import "os/exec"

func detach(*exec.Cmd) {}
