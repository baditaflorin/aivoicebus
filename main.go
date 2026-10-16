// aivoicebus: a tiny message board so agents on different machines (and
// different tools: Claude, Codex) can talk. See README.md.
package main

import (
	"os"

	"github.com/baditaflorin/aivoicebus/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}))
}
