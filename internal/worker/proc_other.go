//go:build !unix

package worker

import "os/exec"

// ownGroup: there are no process groups to use here; the time limit kills the command itself.
func ownGroup(*exec.Cmd) {}

func killGroup(*exec.Cmd) {}
