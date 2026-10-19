package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// `token report` answers, from the nodes' own record, whether the shared token can be rotated
// without cutting anyone off: every machine heard from lately, and the kind of token the nodes last
// saw it use (GET /v1/token/seen on every node, the newest answer wins). A machine still on the
// shared token stops working when deploy/rotate-token.sh retires it, unless it is a node (its token
// file is rotated with the rest) or the machine that deploys (the script switches it itself).
// A machine no node has seen since it started is not proven either way, so it blocks too.

const tokenReportUsage = "usage: aivoicebus token report [--days 7] [--deploy HOST] [--nodes NAME,...] [--check] [--json]"

type reportNode struct {
	URL   string `json:"url"`
	Node  string `json:"node,omitempty"`
	Error string `json:"error,omitempty"`
}

type reportMachine struct {
	Host         string    `json:"host"`
	Role         string    `json:"role"` // client, node or deploy
	LastHeard    time.Time `json:"last_heard,omitzero"`
	Kind         string    `json:"kind,omitempty"` // machine or shared, as the nodes last saw it; "" = no node knows
	TokenName    string    `json:"token_name,omitempty"`
	SeenAt       time.Time `json:"seen_at,omitzero"`
	SeenBy       string    `json:"seen_by,omitempty"`
	MachineToken bool      `json:"machine_token_file"` // a token for it in machine-tokens/ here
	Blocks       string    `json:"blocks_rotation,omitempty"`
}

type tokenReportView struct {
	Days             int             `json:"days"`
	Nodes            []reportNode    `json:"nodes"`
	Machines         []reportMachine `json:"machines"`
	MachineTokensDir string          `json:"machine_tokens_dir"`
	MachineTokens    []string        `json:"machine_tokens"`
	RotateSafe       bool            `json:"rotate_safe"`
	Verdict          string          `json:"verdict"`
}

func tokenReport(args []string, s Streams) error {
	fs := newFlagSet("token report")
	days := fs.Int("days", 7, "machines heard from in this many days")
	deploy := fs.String("deploy", "", "the machine that deploys and keeps the shared token (default: this one)")
	nodes := fs.String("nodes", "", "more names that are nodes, comma-separated (the nodes' own names are known already)")
	check := fs.Bool("check", false, "exit 1 unless rotating the shared token now cuts nobody off")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 || *days < 1 {
		return outbox.Usagef(tokenReportUsage)
	}
	if *deploy == "" {
		*deploy = msg.HostOf(identity.Me())
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	v, err := buildTokenReport(c, time.Now(), *days, strings.ToLower(*deploy), splitList(strings.ToLower(*nodes)), machineTokensDir())
	if err != nil {
		return err
	}
	if *asJSON {
		if err := writeJSON(s.Out, v); err != nil {
			return err
		}
	} else {
		printTokenReport(s.Out, v, time.Now())
	}
	if *check && !v.RotateSafe {
		return exitError{exitFailure}
	}
	return nil
}

// buildTokenReport asks every node c names, each by itself (one that does not answer must not hide
// what another knows), and decides whether a rotation is safe.
func buildTokenReport(c *client.Client, now time.Time, days int, deploy string, extraNodes []string, dir string) (tokenReportView, error) {
	v := tokenReportView{Days: days, MachineTokensDir: dir}
	ones := make([]*client.Client, len(c.URLs))
	whos := make([]map[string]time.Time, len(c.URLs))
	v.Nodes = make([]reportNode, len(c.URLs))
	var wg sync.WaitGroup
	for i, u := range c.URLs {
		ones[i] = &client.Client{URLs: []string{u}, Token: c.Token, Timeout: 5 * time.Second}
		v.Nodes[i].URL = u
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, err := ones[i].Health()
			if err == nil {
				v.Nodes[i].Node = h.Node
				whos[i], err = ones[i].Who()
			}
			if err != nil {
				v.Nodes[i].Error = err.Error()
			}
		}(i)
	}
	wg.Wait()

	isNode := map[string]bool{}
	for _, n := range extraNodes {
		isNode[n] = true
	}
	answered := 0
	for _, n := range v.Nodes {
		if n.Error == "" {
			answered++
			isNode[strings.ToLower(n.Node)] = true
		}
	}
	if answered == 0 {
		return v, errors.New("no node answered: " + v.Nodes[0].Error)
	}

	from := now.Add(-time.Duration(days) * 24 * time.Hour)
	by := map[string]*reportMachine{}
	machine := func(host string) *reportMachine {
		m := by[host]
		if m == nil {
			m = &reportMachine{Host: host, Role: "client"}
			switch {
			case host == deploy:
				m.Role = "deploy"
			case isNode[host]:
				m.Role = "node"
			}
			by[host] = m
		}
		return m
	}
	for _, w := range whos {
		for me, at := range w {
			host := strings.ToLower(msg.HostOf(me))
			if at.Before(from) || !reTokenHost.MatchString(host) {
				continue
			}
			if m := machine(host); at.After(m.LastHeard) {
				m.LastHeard = at
			}
		}
	}
	v.MachineTokens = listMachineTokens(dir)
	for _, h := range v.MachineTokens {
		if m := by[h]; m != nil {
			m.MachineToken = true
		}
	}
	hosts := make([]string, 0, len(by))
	for h := range by {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	// Which token each node last saw each machine use; the newest answer wins.
	seen := make([][]msg.TokenSeen, len(ones))
	tooOld := make([]bool, len(ones))
	for i := range ones {
		if v.Nodes[i].Error != "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for _, h := range hosts {
				all, err := ones[i].TokenSeen(h)
				if client.IsNotFound(err) {
					tooOld[i] = true
					return
				}
				seen[i] = append(seen[i], all...)
			}
		}(i)
	}
	wg.Wait()
	for i, old := range tooOld {
		if old {
			v.Nodes[i].Error = "too old to say which token a machine uses (no token-seen); update it"
		}
	}
	for _, list := range seen {
		for _, e := range list {
			m := by[strings.ToLower(e.Host)]
			if m == nil || e.Kind == "" || !e.At.After(m.SeenAt) {
				continue
			}
			m.Kind, m.TokenName, m.SeenAt, m.SeenBy = e.Kind, e.Name, e.At, e.Node
		}
	}

	var blockers []string
	for _, h := range hosts {
		m := by[h]
		if m.Role == "client" {
			switch m.Kind {
			case "machine":
			case "shared":
				m.Blocks = "it uses the shared token"
				if m.MachineToken {
					m.Blocks += " (its machine token is made: aivoicebus token hand " + h + ")"
				} else {
					m.Blocks += " (give it its own: deploy/machine-token.sh add " + h + " --apply, then aivoicebus token hand " + h + ")"
				}
			default:
				m.Blocks = "no node has seen which token it uses since the node started; not proven"
			}
			if m.Blocks != "" {
				blockers = append(blockers, h)
			}
		}
		v.Machines = append(v.Machines, *m)
	}
	var down []string
	for _, n := range v.Nodes {
		if n.Error != "" {
			down = append(down, view.OneLine(n.URL, 60))
		}
	}
	switch {
	case len(blockers) > 0:
		v.Verdict = fmt.Sprintf("NO: rotating the shared token now would cut off %s", strings.Join(blockers, ", "))
	case len(down) > 0:
		v.Verdict = "NO: not every node answered (" + strings.Join(down, ", ") + "); a machine only it heard from could still use the shared token"
	default:
		v.RotateSafe = true
		v.Verdict = fmt.Sprintf("YES: every machine heard from in %dd other than the nodes and %s uses its own machine token", days, deploy)
	}
	return v, nil
}

// listMachineTokens names the machine tokens deploy/machine-token.sh made (one file per machine;
// a hidden file is one being revoked).
func listMachineTokens(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if n := e.Name(); e.Type().IsRegular() && !strings.HasPrefix(n, ".") && reTokenHost.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}

func printTokenReport(w io.Writer, v tokenReportView, now time.Time) {
	for _, n := range v.Nodes {
		if n.Error != "" {
			fmt.Fprintf(w, "node %s: %s\n", view.OneLine(n.URL, 60), view.OneLine(n.Error, 200))
		}
	}
	fmt.Fprintf(w, "machines heard from in the last %dd, and the token the nodes last saw each use:\n", v.Days)
	fmt.Fprintf(w, "  %-28s %-7s %-10s %-8s %-13s %s\n", "MACHINE", "ROLE", "HEARD", "TOKEN", "MACHINE TOKEN", "")
	for _, m := range v.Machines {
		kind := m.Kind
		if kind == "" {
			kind = "unknown"
		}
		file := "-"
		if m.MachineToken {
			file = "made"
		}
		note := ""
		if m.Blocks != "" {
			note = "<- " + m.Blocks
		}
		fmt.Fprintf(w, "  %-28s %-7s %-10s %-8s %-13s %s\n", view.OneLine(m.Host, 28), m.Role, agoText(now, m.LastHeard), kind, file, note)
	}
	if len(v.Machines) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	switch {
	case len(v.MachineTokens) == 0:
		fmt.Fprintf(w, "machine tokens made here (%s): none\n", v.MachineTokensDir)
	default:
		heard := map[string]bool{}
		for _, m := range v.Machines {
			heard[m.Host] = true
		}
		var names []string
		for _, h := range v.MachineTokens {
			if !heard[h] {
				h += fmt.Sprintf(" (not heard from in %dd)", v.Days)
			}
			names = append(names, h)
		}
		fmt.Fprintf(w, "machine tokens made here (%s): %s\n", v.MachineTokensDir, strings.Join(names, ", "))
	}
	fmt.Fprintf(w, "rotate the shared token now? %s\n", v.Verdict)
}

// agoText says how long ago t was, roughly.
func agoText(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}
