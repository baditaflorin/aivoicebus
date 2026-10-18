package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
	"github.com/baditaflorin/aivoicebus/internal/worker"
)

// tokensCmd says what aivoicebus cost on this machine: the context it put into each session (hook
// output, MCP tool results, CLI reads by agents; an estimate, about 4 characters a token) and the
// worker's runs (the tokens the agents reported).

const charsPerToken = 4

type sessionCost struct {
	Me        string `json:"session"`
	HookChars int    `json:"hook_chars"`
	MCPChars  int    `json:"mcp_chars"`
	CLIChars  int    `json:"cli_chars"`
	EstTokens int    `json:"est_tokens"`
}

type workerCost struct {
	Runs      int     `json:"runs"`
	Reported  int     `json:"runs_with_counts"`
	TokensIn  int     `json:"tokens_in"`
	Cached    int     `json:"cached_tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
}

type tokensView struct {
	Since    time.Time     `json:"since"`
	Sessions []sessionCost `json:"sessions"`
	Context  int           `json:"context_est_tokens"`
	Worker   workerCost    `json:"worker"`
	Note     string        `json:"note"`
}

func tokensCmd(args []string, s Streams) error {
	fs := newFlagSet("tokens")
	since := fs.Duration("since", 24*time.Hour, "how far back")
	asJSON := fs.Bool("json", false, "print it as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return outbox.Usagef("usage: tokens [--since 24h] [--json]")
	}
	from := time.Now().Add(-*since)
	v := tokensView{Since: from.UTC(), Note: "context is estimated (about 4 characters a token); worker runs are what the agents reported"}
	by := map[string]*sessionCost{}
	for _, u := range config.ReadUsage(from) {
		c := by[u.Me]
		if c == nil {
			c = &sessionCost{Me: u.Me}
			by[u.Me] = c
		}
		switch {
		case len(u.What) > 5 && u.What[:5] == "hook:":
			c.HookChars += u.Chars
		case len(u.What) > 4 && u.What[:4] == "mcp:":
			c.MCPChars += u.Chars
		default:
			c.CLIChars += u.Chars
		}
	}
	for _, c := range by {
		c.EstTokens = (c.HookChars + c.MCPChars + c.CLIChars) / charsPerToken
		v.Context += c.EstTokens
		v.Sessions = append(v.Sessions, *c)
	}
	sort.Slice(v.Sessions, func(i, j int) bool { return v.Sessions[i].EstTokens > v.Sessions[j].EstTokens })
	if f, err := os.Open(worker.LogPath()); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var e worker.Entry
			if json.Unmarshal(sc.Bytes(), &e) != nil || e.At.Before(from) || (e.Outcome != "ran" && e.Outcome != "cancelled" && e.Outcome != "withheld") {
				continue
			}
			v.Worker.Runs++
			if e.Usage != nil {
				v.Worker.Reported++
				v.Worker.TokensIn += e.Usage.Input
				v.Worker.Cached += e.Usage.Cached
				v.Worker.TokensOut += e.Usage.Output + e.Usage.Reasoning
				v.Worker.CostUSD += e.Usage.CostUSD
			}
		}
		f.Close()
	}
	if *asJSON {
		if v.Sessions == nil {
			v.Sessions = []sessionCost{}
		}
		return writeJSON(s.Out, v)
	}
	fmt.Fprintf(s.Out, "aivoicebus on this machine since %s\n\n", view.When(from, time.Now()))
	fmt.Fprintf(s.Out, "context it put into sessions (estimate, ~%d characters a token):\n", charsPerToken)
	if len(v.Sessions) == 0 {
		fmt.Fprintln(s.Out, "  none recorded")
	} else {
		fmt.Fprintf(s.Out, "  %-44s %9s %9s %9s %9s\n", "SESSION", "HOOKS", "MCP", "CLI", "~TOKENS")
		for _, c := range v.Sessions {
			fmt.Fprintf(s.Out, "  %-44s %9d %9d %9d %9d\n", msg.SafeName(c.Me), c.HookChars, c.MCPChars, c.CLIChars, c.EstTokens)
		}
		fmt.Fprintf(s.Out, "  %-44s %39d\n", "total", v.Context)
	}
	w := v.Worker
	fmt.Fprintf(s.Out, "\nworker runs: %d", w.Runs)
	if w.Reported > 0 {
		fmt.Fprintf(s.Out, " (%d reported counts): %d tokens in (%d cached), %d out", w.Reported, w.TokensIn, w.Cached, w.TokensOut)
		if w.CostUSD > 0 {
			fmt.Fprintf(s.Out, ", $%.4f", w.CostUSD)
		}
	}
	fmt.Fprintln(s.Out)
	return nil
}
