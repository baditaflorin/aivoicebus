//go:build !unix

package sound

import "os/exec"

func detach(*exec.Cmd) {}
