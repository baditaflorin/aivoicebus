package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// nodeReport is what `aivoicebus mesh` learned about one node.
type nodeReport struct {
	URL     string      `json:"url"`
	Name    string      `json:"name"`
	Error   string      `json:"error,omitempty"`
	Health  *msg.Health `json:"health,omitempty"`
	SkewMs  int64       `json:"skew_ms"` // this node's clock against the middle of the nodes' clocks
	Records int64       `json:"records"` // records held, all origins
}

// meshThresholds decide what counts as a problem.
type meshThresholds struct {
	MaxSkew time.Duration // a node's clock this far from the others'
	MaxLag  int64         // a peer this many records ahead of a node
	MaxGap  int64         // a node this many records behind the most complete one
	// MaxRecords is a size alarm (0 = off): a node holds its whole log in memory, at roughly
	// 650 bytes a record, under a 256 MB limit. See docs/capacity.md.
	MaxRecords int64
}

// meshView is the whole report, for --json.
type meshView struct {
	Nodes    []nodeReport `json:"nodes"`
	Problems []string     `json:"problems"`
}

// fetchMesh asks every node about itself at once.
func fetchMesh(urls []string, token string, timeout time.Duration) []nodeReport {
	reports := make([]nodeReport, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			c := &client.Client{URLs: []string{u}, Token: token, Timeout: timeout}
			r := nodeReport{URL: u, Name: u}
			t0 := time.Now()
			h, err := c.Health()
			t1 := time.Now()
			switch {
			case err != nil:
				r.Error = err.Error()
			default:
				r.Health = &h
				if h.Node != "" {
					r.Name = h.Node
				}
				for _, n := range h.Log {
					r.Records += int64(n)
				}
				r.SkewMs = h.Now.Sub(t0.Add(t1.Sub(t0) / 2)).Milliseconds() // against our clock for now
			}
			reports[i] = r
		}(i, u)
	}
	wg.Wait()
	return reports
}

// assess turns the reports into the problems an operator should look at, and
// makes each node's skew relative to the middle of the nodes' own clocks, so one
// machine with a wrong clock is not blamed on all the others.
func assess(reports []nodeReport, th meshThresholds) []string {
	var problems []string
	var skews []int64
	var maxRecords int64
	versions := map[string][]string{}
	for _, r := range reports {
		if r.Health == nil {
			problems = append(problems, fmt.Sprintf("%s is unreachable: %s", r.Name, r.Error))
			continue
		}
		skews = append(skews, r.SkewMs)
		if r.Records > maxRecords {
			maxRecords = r.Records
		}
		v := r.Health.Version
		if v == "" {
			v = "unknown"
		}
		versions[v] = append(versions[v], r.Name)
	}
	var mid int64
	if len(skews) > 0 {
		sorted := append([]int64(nil), skews...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		mid = sorted[len(sorted)/2]
	}
	for i := range reports {
		r := &reports[i]
		if r.Health == nil {
			continue
		}
		r.SkewMs -= mid
		if d := time.Duration(r.SkewMs) * time.Millisecond; d > th.MaxSkew || d < -th.MaxSkew {
			problems = append(problems, fmt.Sprintf("%s's clock is %s off the others (message expiry uses node clocks)", r.Name, d.Round(100*time.Millisecond)))
		}
		if th.MaxRecords > 0 && r.Records > th.MaxRecords {
			problems = append(problems, fmt.Sprintf("%s holds %d records, more than %d: it keeps them all in memory (about 650 bytes each, limit 256 MB); see docs/capacity.md", r.Name, r.Records, th.MaxRecords))
		}
		if gap := maxRecords - r.Records; gap > th.MaxGap {
			problems = append(problems, fmt.Sprintf("%s holds %d fewer records than the most complete node: replication is behind", r.Name, gap))
		}
		for _, p := range r.Health.Peers {
			switch {
			case !p.OK:
				problems = append(problems, fmt.Sprintf("%s cannot reach its peer %s: %s", r.Name, p.URL, p.Error))
			case p.Lag > th.MaxLag:
				problems = append(problems, fmt.Sprintf("%s is %d records behind its peer %s", r.Name, p.Lag, p.URL))
			}
		}
	}
	if len(versions) > 1 {
		var parts []string
		for v, names := range versions {
			sort.Strings(names)
			parts = append(parts, fmt.Sprintf("%s on %s", v, strings.Join(names, ", ")))
		}
		sort.Strings(parts)
		problems = append(problems, "the nodes run different releases: "+strings.Join(parts, "; "))
	}
	sort.Strings(problems)
	return problems
}

// meshCmd is `aivoicebus mesh`: how healthy is the mesh, node by node.
func meshCmd(args []string, s Streams) error {
	fs := newFlagSet("mesh")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	maxSkew := fs.Duration("max-skew", 2*time.Second, "report a node whose clock is this far from the others")
	maxLag := fs.Int64("max-lag", 50, "report a node this many records behind a peer")
	maxGap := fs.Int64("max-gap", 50, "report a node holding this many fewer records than the most complete one")
	maxRecords := fs.Int64("max-records", 150000, "report a node holding more records than this (0 turns the check off): the whole log is held in memory")
	timeout := fs.Duration("timeout", 4*time.Second, "per node")
	signed := fs.Bool("signed-check", false, "say whether the nodes can be switched to serve --require-signed: READY only when no message without a valid signature arrived in --days")
	days := fs.Int("days", 7, "with --signed-check: the window, in days (1 to 30)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	if *signed {
		return signedCheck(c, *days, *timeout, *asJSON, s)
	}
	reports := fetchMesh(c.URLs, c.Token, *timeout)
	problems := assess(reports, meshThresholds{MaxSkew: *maxSkew, MaxLag: *maxLag, MaxGap: *maxGap, MaxRecords: *maxRecords})
	if note := nodeListNote(c, time.Now()); note != "" {
		problems = append(problems, note)
	}
	if *asJSON {
		enc := json.NewEncoder(s.Out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(meshView{Nodes: reports, Problems: append([]string{}, problems...)})
	} else {
		tw := tabwriter.NewWriter(s.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NODE\tRELEASE\tCLOCK\tRECORDS\tMESSAGES\tPEERS\tFEATURES\tURL")
		for _, r := range reports {
			if r.Health == nil {
				fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t-\t-\t%s\n", r.Name, r.URL)
				continue
			}
			ok := 0
			for _, p := range r.Health.Peers {
				if p.OK {
					ok++
				}
			}
			v := r.Health.Version
			if v == "" {
				v = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%+.1fs\t%d\t%d\t%d/%d up\t%s\t%s\n", r.Name, v, float64(r.SkewMs)/1000, r.Records, r.Health.Messages, ok, len(r.Health.Peers), featureList(r.Health), r.URL)
		}
		tw.Flush()
		if len(problems) == 0 {
			fmt.Fprintf(s.Out, "\nmesh healthy: %d node(s) answer, same release, clocks and logs agree\n", len(reports))
		} else {
			fmt.Fprintln(s.Out, "\nproblems:")
			for _, p := range problems {
				fmt.Fprintln(s.Out, "  -", p)
			}
		}
	}
	if len(problems) > 0 {
		return exitError{1}
	}
	return nil
}

// nodeListWarning is how long before a published node list runs out the mesh starts saying so.
const nodeListWarning = 14 * 24 * time.Hour

// nodeListNote says when the node list the mesh publishes (for machines to learn nodes from) has
// run out or is about to, "" if there is none or it is fine. Machines stop using an expired list,
// so a list nobody renewed quietly stops teaching them new nodes. This is only an alarm: the
// list is not verified here (machines verify it with the release key).
func nodeListNote(c *client.Client, now time.Time) string {
	body, err := c.FetchRelease(release.NodesName, release.MaxNodesBytes)
	if err != nil {
		return ""
	}
	n, err := release.ParseNodes(body)
	if err != nil || n.ValidUntil == "" {
		return ""
	}
	switch {
	case n.Expired(now):
		return fmt.Sprintf("the published node list expired on %s: machines no longer learn nodes from it (re-sign: aivoicebus release nodes, then deploy/publish-nodes.sh from the root of an aivoicebus checkout)", n.Until().Local().Format("2006-01-02"))
	case n.Until().Sub(now) < nodeListWarning:
		return fmt.Sprintf("the published node list expires on %s (re-sign: aivoicebus release nodes, then deploy/publish-nodes.sh from the root of an aivoicebus checkout)", n.Until().Local().Format("2006-01-02"))
	}
	return ""
}

// featureList is a node's advertised features for a table cell: each cut to what a peer could
// make of it, "-" for a node that advertises none.
func featureList(h *msg.Health) string {
	const shown = 12 // a hostile node cannot widen the table; a cut list says so
	var out []string
	more := 0
	for _, f := range h.Features {
		if f = view.OneLine(f, 24); f == "" {
			continue
		}
		if len(out) == shown {
			more++
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return "-"
	}
	if more > 0 {
		out = append(out, fmt.Sprintf("+%d more", more))
	}
	return strings.Join(out, ",")
}
