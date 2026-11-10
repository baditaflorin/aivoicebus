//go:build !unix

package cli

import "os/exec"

func detachForwarder(cmd *exec.Cmd) {}
