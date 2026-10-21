package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/discovery"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// nodesCmd is `aivoicebus nodes`: the nodes this machine uses, where each came from,
// and (with --refresh) a fresh look at the mesh's signed node list.
func nodesCmd(args []string, s Streams) error {
	fs := newFlagSet("nodes")
	refresh := fs.Bool("refresh", false, "fetch the signed node list from the mesh now")
	asJSON := fs.Bool("json", false, "print the nodes and the node list's state as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	out := s.Out
	if *asJSON {
		out = io.Discard // the refresh's one-line report is for people; the JSON below says the same
	}
	if *refresh {
		c, err := client.FromEnv()
		if err != nil {
			return err
		}
		c.Timeout = 5 * time.Second
		res, err := discovery.Refresh(c, trustedKeys(), time.Now())
		if err != nil {
			return err
		}
		switch {
		case res.Changed:
			fmt.Fprintf(out, "node list updated (serial %d): %d node(s)", res.Serial, len(res.Nodes))
			if len(res.Added) > 0 {
				fmt.Fprintf(out, ", added %s", strings.Join(res.Added, " "))
			}
			if len(res.Removed) > 0 {
				fmt.Fprintf(out, ", removed %s", strings.Join(res.Removed, " "))
			}
			fmt.Fprintln(out)
		default:
			fmt.Fprintf(out, "node list is current (serial %d)\n", res.Serial)
		}
	}
	configured := map[string]bool{}
	for _, u := range strings.Split(os.Getenv("AIVOICEBUS_URL"), ",") {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			configured[u] = true
		}
	}
	d := config.LoadDiscovered()
	ns := config.Resolve()
	type nodeView struct {
		URL    string `json:"url"`
		Source string `json:"source"` // configured | learned
	}
	nodes := []nodeView{}
	for _, u := range ns.URLs {
		from, src := "learned from the mesh", "learned"
		if configured[u] {
			from, src = "configured", "configured"
		}
		if t := retiredText(u, ns); t != "" { // only when Kept: otherwise it is not among the URLs
			from += ", " + t + ", but still used: AIVOICEBUS_URL names no node of the list"
		}
		nodes = append(nodes, nodeView{u, src})
		fmt.Fprintf(out, "%-36s %s\n", u, from)
	}
	if !ns.Kept {
		for _, r := range ns.Retired {
			fmt.Fprintf(out, "%-36s configured, %s: not used\n", r.URL, retiredText(r.URL, ns))
		}
	}
	if *asJSON {
		v := struct {
			Nodes      []nodeView    `json:"nodes"`
			Retired    []retiredView `json:"retired"` // configured nodes the list retired
			Serial     int64         `json:"serial"`  // 0: no node list learned yet
			FetchedAt  *time.Time    `json:"fetched_at,omitempty"`
			ValidUntil *time.Time    `json:"valid_until,omitempty"`
			Expired    bool          `json:"expired"`
		}{Nodes: nodes, Retired: retiredViews(ns), Serial: d.Serial, Expired: d.Serial != 0 && d.Expired(time.Now())}
		if d.Serial != 0 {
			v.FetchedAt = &d.FetchedAt
			if !d.ValidUntil.IsZero() {
				v.ValidUntil = &d.ValidUntil
			}
		}
		return writeJSON(s.Out, v)
	}
	if d.Serial == 0 {
		fmt.Fprintln(s.Out, "(this machine has not learned the node list yet: `aivoicebus nodes --refresh`)")
	} else {
		until := "no expiry"
		if !d.ValidUntil.IsZero() {
			until = "valid until " + d.ValidUntil.Local().Format("2006-01-02")
			if d.Expired(time.Now()) {
				until = "EXPIRED " + d.ValidUntil.Local().Format("2006-01-02") + ", not used"
			}
		}
		fmt.Fprintf(s.Out, "(node list serial %d, fetched %s, %s)\n", d.Serial, view.When(d.FetchedAt, time.Now()), until)
	}
	return nil
}

// retiredView is a configured node the signed node list retired, for --json.
type retiredView struct {
	Name  string     `json:"name"`
	URL   string     `json:"url"`
	Since *time.Time `json:"since,omitempty"`
	Used  bool       `json:"used"` // still tried: AIVOICEBUS_URL names no node of the list
}

func retiredViews(ns config.NodeSet) []retiredView {
	out := []retiredView{}
	for _, r := range ns.Retired {
		v := retiredView{Name: r.Name, URL: r.URL, Used: ns.Kept}
		if !r.Since.IsZero() {
			since := r.Since
			v.Since = &since
		}
		out = append(out, v)
	}
	return out
}

// retiredText says which configured node the signed node list retired, and since when.
func retiredText(u string, ns config.NodeSet) string {
	for _, r := range ns.Retired {
		if r.URL == u {
			s := "retired from the mesh (" + r.Name
			if !r.Since.IsZero() {
				s += ", " + r.Since.Local().Format("2006-01-02")
			}
			return s + ") by the signed node list"
		}
	}
	return ""
}

// parseNodeConf reads deploy/nodes.conf: each name:ssh-alias:tailnet-ip line is a node, and each
// "# removed TIME name:alias:ip" line (deploy/remove-node.sh leaves one in place of a node it took
// out of the mesh) a node the list retires, newest first, unless a line lists it again.
func parseNodeConf(path string, port string) ([]release.NodeEntry, []release.RetiredEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	var out []release.NodeEntry
	removed := map[string]release.RetiredEntry{} // by URL: the latest removal
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if r, ok := removedLine(line, port); ok {
			if prev, seen := removed[r.URL]; !seen || r.Since > prev.Since {
				removed[r.URL] = r
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 3 {
			return nil, nil, fmt.Errorf("%s: %q is not name:alias:ip: fix that line, then run it again", path, line)
		}
		out = append(out, release.NodeEntry{Name: parts[0], URL: "http://" + parts[2] + ":" + port})
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	for _, e := range out {
		delete(removed, e.URL) // back in the mesh (or its address reused): a node, not retired
	}
	retired := make([]release.RetiredEntry, 0, len(removed))
	for _, r := range removed {
		retired = append(retired, r)
	}
	sort.Slice(retired, func(i, j int) bool {
		if retired[i].Since != retired[j].Since {
			return retired[i].Since > retired[j].Since
		}
		return retired[i].URL < retired[j].URL
	})
	return out, retired, nil
}

// removedLine reads a "# removed TIME name:alias:ip [gone] [stopped TIME]" line. Any other
// comment, a hand-written "# removed ..." note among them, is not one.
func removedLine(line, port string) (release.RetiredEntry, bool) {
	f := strings.Fields(line)
	if len(f) < 4 || f[0] != "#" || f[1] != "removed" {
		return release.RetiredEntry{}, false
	}
	t, err := time.Parse(time.RFC3339, f[2])
	parts := strings.Split(f[3], ":")
	if err != nil || len(parts) != 3 {
		return release.RetiredEntry{}, false
	}
	// one form, so the times compare as strings
	return release.RetiredEntry{Name: parts[0], URL: "http://" + parts[2] + ":" + port, Since: t.UTC().Format(time.RFC3339)}, true
}

// releaseNodes signs the mesh's node list: nodes.json and nodes.sig.
func releaseNodes(args []string, s Streams) error {
	fs := newFlagSet("release nodes")
	conf := fs.String("conf", "deploy/nodes.conf", "the node list (name:alias:ip per line)")
	dir := fs.String("dir", "", "directory to write nodes.json and nodes.sig to")
	key := fs.String("key", defaultKeyPath(), "private signing key")
	port := fs.String("port", "7777", "node port")
	serial := fs.Int64("serial", 0, "list serial (default: the current time in seconds, which always increases)")
	valid := fs.String("valid", "90d", "how long machines trust the list (e.g. 30d, 90d, 400d; 'never' for no expiry)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("release nodes needs --dir: run it again as `aivoicebus release nodes --dir DIR`")
	}
	entries, retired, err := parseNodeConf(*conf, *port)
	if err != nil {
		return err
	}
	left := 0
	if len(retired) > release.MaxRetired {
		left, retired = len(retired)-release.MaxRetired, retired[:release.MaxRetired]
	}
	priv, err := release.LoadPrivateKey(*key)
	if err != nil {
		return err
	}
	validFor := time.Duration(-1)
	if *valid != "never" {
		if validFor, err = msg.ParseSpan(*valid); err != nil {
			return fmt.Errorf("--valid: %v", err)
		}
	}
	n, err := release.PackNodes(release.PackNodesOptions{Dir: *dir, Serial: *serial, Nodes: entries, Retired: retired, Key: priv, ValidFor: validFor})
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "signed the node list (serial %d, %d node(s), valid until %s) in %s\n", n.Serial, len(n.Nodes), validText(n), *dir)
	if len(n.Retired) > 0 {
		var names []string
		for _, r := range n.Retired {
			names = append(names, r.Name)
		}
		fmt.Fprintf(s.Out, "it retires %d node(s) (the \"# removed\" lines): %s; machines still configured for one stop using it\n", len(n.Retired), strings.Join(names, ", "))
	}
	if left > 0 {
		fmt.Fprintf(s.Out, "%d older removed node(s) left out: a list retires at most %d\n", left, release.MaxRetired)
	}
	return nil
}

func validText(n *release.Nodes) string {
	if n.ValidUntil == "" {
		return "no expiry"
	}
	return n.Until().Local().Format("2006-01-02")
}
