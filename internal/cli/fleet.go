package cli

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/version"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// fleetCmd shows every machine heard from lately in one table: its role, when it was last heard,
// the client release it says it runs, and the token it uses, as the nodes saw them (the same
// gathering as `token report`). `fleet update` asks the person on each machine that is behind, or
// whose release no node knows yet, to run `aivoicebus update` there: a task for the person, which
// no agent takes, since replacing the binary is theirs to approve.
func fleetCmd(args []string, s Streams) error {
	if len(args) > 0 && args[0] == "update" {
		return fleetUpdate(args[1:], s)
	}
	fs := newFlagSet("fleet")
	days := fs.Int("days", 7, "machines heard from in this many days (1 to 30)")
	asJSON := fs.Bool("json", false, "print the view as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 || *days < 1 || *days > 30 {
		return outbox.Usagef("usage: fleet [--days 7] [--json]   |   fleet update [--apply] [--only-behind] [--days 7]")
	}
	v, target, err := fleetView(*days)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(s.Out, struct {
			Target   string          `json:"nodes_release"`
			Nodes    []reportNode    `json:"nodes"`
			Machines []reportMachine `json:"machines"`
		}{target, v.Nodes, v.Machines})
	}
	fmt.Fprintf(s.Out, "machines heard from in the last %dd; the nodes run %s:\n", *days, orDash(target))
	tw := tabwriter.NewWriter(s.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  MACHINE\tROLE\tHEARD\tRELEASE\tTOKEN\t")
	behind := 0
	for _, m := range v.Machines {
		rel, note := fleetRelease(m, target)
		if note != "" && m.Role == "client" {
			behind++
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", m.Host, m.Role, agoText(time.Now(), m.LastHeard), rel, orDash(m.Kind), note)
	}
	tw.Flush()
	for _, n := range v.Nodes {
		if n.Error != "" {
			fmt.Fprintf(s.Out, "  node %s: %s\n", view.OneLine(n.URL, 80), view.OneLine(n.Error, 160))
		}
	}
	if behind > 0 {
		fmt.Fprintf(s.Out, "%d machine(s) may be behind: aivoicebus fleet update (a plan; --apply asks the person on each to update)\n", behind)
	}
	return nil
}

// fleetView gathers the machines and the release the nodes run (the newest any answering node runs).
func fleetView(days int) (tokenReportView, string, error) {
	c, err := sessionClient()
	if err != nil {
		return tokenReportView{}, "", err
	}
	deploy := "" // the machine you deploy from is the one holding machine tokens (as in doctor)
	if deployTokens() > 0 {
		deploy = msg.HostOf(identity.Me())
	}
	v, err := buildTokenReport(c, time.Now(), days, deploy, nil, machineTokensDir())
	if err != nil {
		return v, "", err
	}
	target := ""
	for _, n := range v.Nodes {
		if n.Error == "" && n.Release != "" && (target == "" || release.Newer(n.Release, target)) {
			target = n.Release
		}
	}
	return v, target, nil
}

// fleetRelease is the release column and, for a machine that may be behind, why.
func fleetRelease(m reportMachine, target string) (string, string) {
	switch {
	case m.Release == "":
		return "unknown", "<- not heard from since its client said; before 0.11.4 clients did not say"
	case target != "" && release.Newer(target, m.Release):
		return m.Release, "<- behind " + target
	}
	return m.Release, ""
}

// fleetUpdate plans, and with --apply sends, one task for the person on each client machine that is
// behind the nodes (or whose release no node knows): "run aivoicebus update there".
func fleetUpdate(args []string, s Streams) error {
	fs := newFlagSet("fleet update")
	apply := fs.Bool("apply", false, "send the tasks (without it: only the plan)")
	onlyBehind := fs.Bool("only-behind", false, "leave out machines whose release no node knows")
	days := fs.Int("days", 7, "machines heard from in this many days (1 to 30)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 || *days < 1 || *days > 30 {
		return outbox.Usagef("usage: fleet update [--apply] [--only-behind] [--days 7]")
	}
	v, target, err := fleetView(*days)
	if err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("no node said which release it runs: run aivoicebus mesh to see why, then try again")
	}
	var hosts []string
	for _, m := range v.Machines {
		if m.Role != "client" {
			continue
		}
		rel, note := fleetRelease(m, target)
		if note == "" || (*onlyBehind && rel == "unknown") {
			continue
		}
		hosts = append(hosts, m.Host)
		fmt.Fprintf(s.Out, "  %s: runs %s, the nodes run %s\n", m.Host, rel, target)
	}
	sort.Strings(hosts)
	if here := version.Release; release.Newer(target, here) {
		fmt.Fprintf(s.Out, "  this machine runs %s: aivoicebus update (here, yourself)\n", here)
	}
	if len(hosts) == 0 {
		fmt.Fprintln(s.Out, "every machine heard from runs the nodes' release: nothing to do")
		return nil
	}
	if !*apply {
		fmt.Fprintf(s.Out, "plan only: aivoicebus fleet update --apply sends each a task for the person there (no agent takes it): \"aivoicebus update\"\n")
		return nil
	}
	sent := 0
	for _, h := range hosts {
		text := fmt.Sprintf("Update aivoicebus on %s: the nodes run %s. In a terminal there: aivoicebus update (it checks the signature before installing), then aivoicebus version.", h, target)
		if err := taskCmd([]string{h, text, "--person", "--ttl", "7d"}, s); err != nil {
			fmt.Fprintf(s.Err, "  %s: not sent: %v\n", h, err)
			continue
		}
		sent++
	}
	fmt.Fprintf(s.Out, "sent %d of %d: each person sees it in aivoicebus status, and standby says it once\n", sent, len(hosts))
	if sent < len(hosts) {
		return exitError{exitFailure}
	}
	return nil
}

func orDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}
