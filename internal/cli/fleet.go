package cli

import (
	"fmt"
	"maps"
	"slices"
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
	caps := fleetCaps(time.Duration(*days) * 24 * time.Hour)
	if *asJSON {
		return writeJSON(s.Out, struct {
			Target   string              `json:"nodes_release"`
			Nodes    []reportNode        `json:"nodes"`
			Machines []reportMachine     `json:"machines"`
			Caps     map[string][]string `json:"caps"` // per machine, what its sessions can do (task --needs)
		}{target, v.Nodes, v.Machines, caps})
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
	if len(caps) > 0 {
		fmt.Fprintf(s.Out, "what their sessions can do (from their labels; task --needs picks by it):\n")
		for _, h := range slices.Sorted(maps.Keys(caps)) {
			fmt.Fprintf(s.Out, "  %s: %s\n", view.Name(h, msg.MaxIdentity), view.Name(strings.Join(caps[h], " "), 400))
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

// fleetCaps is, per machine, every capability (msg.CapsOf) of the sessions seen there within span:
// the declared words first, then the repositories, branches, tools, models and roles, each once.
// Nothing when the roster cannot be read: the table above does not depend on it.
func fleetCaps(span time.Duration) map[string][]string {
	c, err := sessionClient()
	if err != nil {
		return nil
	}
	roster, err := c.Roster()
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for _, e := range view.SanitiseRoster(roster) {
		if time.Since(e.Seen) > span {
			continue
		}
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
	rels := map[string]string{} // host -> the release column ("unknown" when no node knows)
	for _, m := range v.Machines {
		if m.Role != "client" {
			continue
		}
		rel, note := fleetRelease(m, target)
		if note == "" || (*onlyBehind && rel == "unknown") {
			continue
		}
		hosts = append(hosts, m.Host)
		rels[m.Host] = rel
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
	open := openUpdateTasks()
	sent := 0
	for _, h := range hosts {
		text := fmt.Sprintf("Update aivoicebus on %s: the nodes run %s. In a terminal there: aivoicebus update (it checks the signature before installing), then aivoicebus version.", h, target)
		if id := open(h, text); id != "" {
			fmt.Fprintf(s.Out, "  %s: the same task (%s) still waits for the person there: not sent again\n", h, id)
			sent++
			continue
		}
		if err := taskCmd([]string{h, text, "--person", "--ttl", "7d"}, s); err != nil {
			fmt.Fprintf(s.Err, "  %s: not sent: %v\n", h, err)
			continue
		}
		sent++
		if rel := rels[h]; rel != "unknown" && !release.Newer(hookTellsPerson, rel) {
			continue
		}
		// A client before hookTellsPerson never shows its person their tasks unless they run status
		// (before 0.11.0 it has no person queue at all), so its sessions are told too, as an ordinary
		// message: an agent relays a [for the person] message to its user and does not act on it.
		// Not --prio fyi: a session start only counts those, and this one must be read.
		relay := fmt.Sprintf("%s for the user of this machine, not the agent: the nodes run %s and this machine's aivoicebus is older or unknown. Please run aivoicebus update in your own terminal (it checks the signature before installing), then aivoicebus version. Agent: tell the user; do not run it yourself.", msg.PersonTag, target)
		if err := send([]string{"--to", h, "--ttl", "7d", relay}, s); err != nil {
			fmt.Fprintf(s.Err, "  %s: the note to its sessions was not sent: %v\n", h, err)
		}
	}
	fmt.Fprintf(s.Out, "sent %d of %d: each person sees it at their agents' next session start and in aivoicebus status; a machine before %s also gets a note its agents relay\n", sent, len(hosts), hookTellsPerson)
	if sent < len(hosts) {
		return exitError{exitFailure}
	}
	return nil
}

// hookTellsPerson is the first client release whose hooks tell an agent's user, at session start and
// as it arrives, what waits for them (the person on the machine): from it on, the person task alone
// reaches them.
const hookTellsPerson = "0.11.5"

// openUpdateTasks returns what finds, for a machine, the fleet update task with exactly this text
// that still waits unread for the person there ("" for none), so running fleet update again sends
// nothing twice. It peeks on the person's behalf, which only a node of inbox-peek or later does
// without listing the person as present; with older nodes, or none answering, it finds nothing.
func openUpdateTasks() func(host, text string) string {
	none := func(string, string) string { return "" }
	c, err := sessionClient()
	if err != nil {
		return none
	}
	if h, err := c.Health(); err != nil || !h.Has(msg.FeatureInboxPeek) {
		return none
	}
	return func(host, text string) string {
		ms, err := c.Peek(host + "/human")
		if err != nil {
			return ""
		}
		for _, m := range ms {
			if m.Kind == msg.KindTask && m.Retracted == nil && strings.HasPrefix(m.Body, msg.PersonTag) &&
				strings.TrimSpace(strings.TrimPrefix(m.Body, msg.PersonTag)) == text {
				return view.OneLine(m.ID, msg.MaxRef)
			}
		}
		return ""
	}
}

func orDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}
