package cli

import (
	"bytes"
	"fmt"
	"strings"
	"sync"

	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/worker"
)

// runCmd sends a named command to the workers of one or more machines and prints each answer:
//
//	aivoicebus run laptop-b,laptop-c [--wait 1m] -- brightness 1
//
// It sends a signed task whose body is a [run] request, which only a worker started with --run takes.
// That worker runs the file run.d/NAME.json of its own machine: no model reads the request, no shell
// runs it, and a name or argument its owner did not allow is refused. Each machine is asked at the
// same time, with its own wait, so a machine that is off costs the wait and nothing else.
func runCmd(args []string, s Streams) error {
	usage := outbox.Usagef("usage: run MACHINE[,MACHINE...] [--wait 1m] -- NAME [ARG...]   (the machines' workers must be started with --run and have run.d/NAME.json)")
	split := -1
	for i, a := range args {
		if a == "--" {
			split = i
			break
		}
	}
	if split < 0 || split+1 >= len(args) {
		return usage
	}
	head, tail := args[:split], args[split+1:]
	wait := "1m"
	var machines []string
	for i := 0; i < len(head); i++ {
		a := head[i]
		switch {
		case a == "--wait" && i+1 < len(head):
			i++
			wait = head[i]
		case strings.HasPrefix(a, "--wait="):
			wait = strings.TrimPrefix(a, "--wait=")
		case strings.HasPrefix(a, "-"):
			return usage
		default:
			for _, m := range strings.Split(a, ",") {
				if m = strings.TrimSpace(m); m != "" {
					machines = append(machines, m)
				}
			}
		}
	}
	if len(machines) == 0 {
		return usage
	}
	body, err := worker.EncodeRun(tail[0], tail[1:])
	if err != nil {
		return outbox.Usagef("%v", err)
	}
	type result struct {
		out, err bytes.Buffer
		fail     error
	}
	res := make([]*result, len(machines))
	var wg sync.WaitGroup
	for i, m := range machines {
		res[i] = &result{}
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			r := res[i]
			r.fail = taskCmd([]string{m, body, "--wait-reply", wait}, Streams{In: strings.NewReader(""), Out: &r.out, Err: &r.err})
		}(i, m)
	}
	wg.Wait()
	failed := 0
	for i, m := range machines {
		r := res[i]
		fmt.Fprintf(s.Out, "== %s\n", m)
		fmt.Fprint(s.Out, r.out.String())
		if r.fail != nil {
			failed++
			fmt.Fprintf(s.Out, "failed: %v\n", r.fail)
			if t := strings.TrimSpace(r.err.String()); t != "" {
				fmt.Fprintln(s.Out, t)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d machines did not answer or failed", failed, len(machines))
	}
	return nil
}
