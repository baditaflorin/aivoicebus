package cli

import (
	"fmt"
	"maps"
	"slices"
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
// gathering as `token report`). `fleet update` lists the machines that are behind, or whose release
// no node knows yet; it sends nothing (see fleetUpdate).
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
		return outbox.Usagef("usage: fleet [--days 7] [--json]   |   fleet update [--only-behind] [--days 7]")
	}
	v, target, err := fleetView(*days)
	if err != nil {
		return err
	}
	roster := fleetRoster(time.Duration(*days) * 24 * time.Hour)
	caps := fleetCaps(roster)
	facts := factsByMachine(roster)
	if *asJSON {
		known := map[string]msg.Facts{}
		for h, f := range facts {
			if f.From != "" {
				known[h] = f.Facts
			}
		}
		return writeJSON(s.Out, struct {
			Target   string               `json:"nodes_release"`
			Nodes    []reportNode         `json:"nodes"`
			Machines []reportMachine      `json:"machines"`
			Caps     map[string][]string  `json:"caps"`  // per machine, what its sessions can do (task --needs)
			Facts    map[string]msg.Facts `json:"facts"` // per machine, what it says it has (who --has)
		}{target, v.Nodes, v.Machines, caps, known})
	}
	fmt.Fprintf(s.Out, "machines heard from in the last %dd; the nodes run %s:\n", *days, orDash(target))
	tw := tabwriter.NewWriter(s.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  MACHINE\tROLE\tHEARD\tRELEASE\tTOKEN\tFACTS\t")
	behind := 0
	for _, m := range v.Machines {
		rel, note := fleetRelease(m, target)
		if note != "" && m.Role == "client" {
			behind++
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.Host, m.Role, agoText(time.Now(), m.LastHeard), rel, orDash(m.Kind), view.Name(factsText(facts[m.Host]), 160), note)
	}
	tw.Flush()
	for _, n := range v.Nodes {
		if n.Error != "" {
			fmt.Fprintf(s.Out, "  node %s: %s\n", view.OneLine(n.URL, 80), view.OneLine(n.Error, 160))
		}
	}
	if len(caps) > 0 {
		fmt.Fprintf(s.Out, "what their sessions can do (from their labels; task --needs picks by it):\n")
		for _, h := range slices.Sorted(maps.Keys(caps)) {
			fmt.Fprintf(s.Out, "  %s: %s\n", view.Name(h, msg.MaxIdentity), view.Name(strings.Join(caps[h], " "), 400))
		}
	}
	if behind > 0 {
		fmt.Fprintf(s.Out, "%d machine(s) may be behind: each one's own aivoicebus tells its person, nothing to send (aivoicebus fleet update lists them)\n", behind)
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

// fleetRoster is the sessions seen within span, made safe to show. Nothing when the roster cannot
// be read: the table does not depend on it.
func fleetRoster(span time.Duration) []msg.RosterEntry {
	c, err := sessionClient()
	if err != nil {
		return nil
	}
	roster, err := c.Roster()
	if err != nil {
		return nil
	}
	var out []msg.RosterEntry
	for _, e := range view.SanitiseRoster(roster) {
		if time.Since(e.Seen) <= span {
			out = append(out, e)
		}
	}
	return out
}

// fleetCaps is, per machine, every capability (msg.CapsOf) of the sessions in roster: the declared
// words first, then the repositories, branches, tools, models and roles, each once.
func fleetCaps(roster []msg.RosterEntry) map[string][]string {
	out := map[string][]string{}
	for _, e := range roster {
		h := msg.HostOf(e.Me)
		for _, c := range msg.CapsOf(e.Me, e.Labels) {
			if !slices.Contains(out[h], c) {
				out[h] = append(out[h], c)
			}
		}
	}
	for h := range out {
		slices.SortStableFunc(out[h], func(a, b string) int { // words first, then kinds in NeedKeys order
			return capRank(a) - capRank(b)
		})
	}
	return out
}

func capRank(c string) int {
	k, _, ok := strings.Cut(c, ":")
	if !ok {
		return -1
	}
	return slices.Index(msg.NeedKeys, k)
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

// fleetUpdate lists the client machines that are behind the nodes, or whose release no node knows.
// It sends nothing. The nodes announce the release they hold on every answer, so each machine's own
// aivoicebus already says it to its person, at session start (once a day) and in `status`, stops by
// itself once that machine runs it, and installs it by itself where the person turned on
// `aivoicebus update --auto`. Until 0.13.x `--apply` sent each such machine's person a task, a new
// one with every release, and nobody read them; the flag is still taken, so an old script or note
// that runs it learns why nothing goes out.
func fleetUpdate(args []string, s Streams) error {
	fs := newFlagSet("fleet update")
	apply := fs.Bool("apply", false, "sends nothing any more: each machine's own aivoicebus tells its person")
	onlyBehind := fs.Bool("only-behind", false, "leave out machines whose release no node knows")
	days := fs.Int("days", 7, "machines heard from in this many days (1 to 30)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 || *days < 1 || *days > 30 {
		return outbox.Usagef("usage: fleet update [--only-behind] [--days 7]")
	}
	v, target, err := fleetView(*days)
	if err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("no node said which release it runs: run aivoicebus mesh to see why, then try again")
	}
	behind := 0
	for _, m := range v.Machines {
		if m.Role != "client" {
			continue
		}
		rel, note := fleetRelease(m, target)
		if note == "" || (*onlyBehind && rel == "unknown") {
			continue
		}
		behind++
		fmt.Fprintf(s.Out, "  %s: runs %s, the nodes run %s\n", m.Host, rel, target)
	}
	if here := version.Release; release.Newer(target, here) {
		fmt.Fprintf(s.Out, "  this machine runs %s: aivoicebus update (here, yourself), or once: aivoicebus update --auto on\n", here)
	}
	if *apply {
		fmt.Fprintln(s.Out, "--apply sends nothing any more: a task per machine and release went unread, and each machine already hears it from the nodes")
	}
	if behind == 0 {
		fmt.Fprintln(s.Out, "every machine heard from runs the nodes' release: nothing to do")
		return nil
	}
	fmt.Fprintf(s.Out, "nothing to send: each one's own aivoicebus tells its person at session start (once a day) and in aivoicebus status, and installs by itself where the person turned on aivoicebus update --auto\n")
	return nil
}

func orDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}
