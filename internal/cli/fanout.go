package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// fanOut is task --each: the same task to several machines, and with --wait-all one wait for all
// the answers, in parallel, under one deadline. Each result says who took it and what came back,
// or that nothing did in time.

type fanResult struct {
	Machine string `json:"machine"`
	resultView
}

func fanOut(machines []string, tool, text, waitAll string, flags []string, s Streams) error {
	asJSON := slices.Contains(flags, "--json") || slices.Contains(flags, "-json")
	var d time.Duration
	if waitAll != "" {
		var err error
		if d, err = time.ParseDuration(waitAll); err != nil || d < time.Second {
			return outbox.Usagef("--wait-all is a duration, e.g. 15m")
		}
	}
	var targets []string
	for _, m := range machines {
		if m = strings.TrimSpace(m); m != "" && !slices.Contains(targets, m) {
			targets = append(targets, m)
		}
	}
	if len(targets) < 2 || strings.TrimSpace(text) == "" {
		return outbox.Usagef(`usage: task --each M1,M2[,...] "what to do" [--tool codex] [--wait-all 15m] [--json]   (two machines or more)`)
	}
	c, err := sessionClient()
	if err != nil {
		return err
	}
	me := identity.Me()
	results := make([]fanResult, len(targets))
	sent := make([]msg.Message, len(targets))
	for i, m := range targets {
		to, err := taskTarget(m, tool, "", "")
		if err != nil {
			return outbox.Usagef("%v", err)
		}
		plan, err := outbox.Prepare(c, outbox.Request{To: to, Body: text, Kind: msg.KindTask, Queue: true, TargetSaid: true}, io.Discard)
		if err != nil {
			return fmt.Errorf("%s: %w", m, err)
		}
		if sent[i], err = plan.Send(c); err != nil {
			return fmt.Errorf("%s: %w", m, err)
		}
		results[i] = fanResult{Machine: m, resultView: resultOf(sent[i], "", nil)}
		fmt.Fprintf(s.Err, "sent %s to %s\n", sent[i].ID, view.Name(to, msg.MaxIdentity))
	}
	if d == 0 {
		if asJSON {
			return writeJSON(s.Out, results)
		}
		for _, r := range results {
			fmt.Fprintf(s.Out, "%-24s %s\n", r.Machine, r.ID)
		}
		return nil
	}
	fmt.Fprintf(s.Err, "waiting up to %s for %d answers...\n", d, len(targets))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			taken := ""
			got, err := tasks.Wait(ctx, c, me, tasks.Filter{Thread: sent[i].Thread, Progress: func(m msg.Message) { taken = m.From }}, d, 3*time.Second)
			if err == nil {
				for _, m := range got {
					_ = c.AckVia(m.ID, me, msg.ViaCLI, 0)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			results[i] = fanResult{Machine: targets[i], resultView: resultOf(sent[i], taken, got)}
			if err == nil {
				fmt.Fprintf(s.Err, "  %s answered\n", targets[i])
			}
		}(i)
	}
	wg.Wait()
	n := 0
	for _, r := range results {
		if r.Answered {
			n++
		}
	}
	if asJSON {
		if err := writeJSON(s.Out, map[string]any{"answered": n, "of": len(results), "results": results}); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(s.Out, view.UntrustedBanner)
		for _, r := range results {
			fmt.Fprintf(s.Out, "== %s (%s)", r.Machine, r.ID)
			switch {
			case r.Answered:
				fmt.Fprintf(s.Out, ", answered by %s\n", view.Name(r.Answers[len(r.Answers)-1].From, msg.MaxIdentity))
				for _, a := range r.Answers {
					for _, line := range strings.Split(view.Sanitise(a.Body), "\n") {
						fmt.Fprintln(s.Out, "  "+line)
					}
				}
			case r.TakenBy != "":
				fmt.Fprintf(s.Out, ": taken by %s, no answer in %s\n", view.Name(r.TakenBy, msg.MaxIdentity), d)
			default:
				fmt.Fprintf(s.Out, ": nobody took it in %s\n", d)
			}
		}
		fmt.Fprintf(s.Out, "%d of %d answered\n", n, len(results))
	}
	if n < len(results) {
		return exitError{exitTimeout}
	}
	return nil
}
