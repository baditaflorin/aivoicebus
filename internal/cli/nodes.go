package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
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
	type nodeView struct {
		URL    string `json:"url"`
		Source string `json:"source"` // configured | learned
	}
	nodes := []nodeView{}
	for _, u := range config.URLs() {
		from, src := "learned from the mesh", "learned"
		if configured[u] {
			from, src = "configured", "configured"
		}
		nodes = append(nodes, nodeView{u, src})
		fmt.Fprintf(out, "%-36s %s\n", u, from)
	}
	if *asJSON {
		v := struct {
			Nodes      []nodeView `json:"nodes"`
			Serial     int64      `json:"serial"` // 0: no node list learned yet
			FetchedAt  *time.Time `json:"fetched_at,omitempty"`
			ValidUntil *time.Time `json:"valid_until,omitempty"`
			Expired    bool       `json:"expired"`
		}{Nodes: nodes, Serial: d.Serial, Expired: d.Serial != 0 && d.Expired(time.Now())}
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

// parseNodeConf reads deploy/nodes.conf lines name:ssh-alias:tailnet-ip into node entries.
func parseNodeConf(path string, port string) ([]release.NodeEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []release.NodeEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("%s: %q is not name:alias:ip", path, line)
		}
		out = append(out, release.NodeEntry{Name: parts[0], URL: "http://" + parts[2] + ":" + port})
	}
	return out, sc.Err()
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
		return errors.New("release nodes needs --dir")
	}
	entries, err := parseNodeConf(*conf, *port)
	if err != nil {
		return err
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
	n, err := release.PackNodes(release.PackNodesOptions{Dir: *dir, Serial: *serial, Nodes: entries, Key: priv, ValidFor: validFor})
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "signed the node list (serial %d, %d node(s), valid until %s) in %s\n", n.Serial, len(n.Nodes), validText(n), *dir)
	return nil
}

func validText(n *release.Nodes) string {
	if n.ValidUntil == "" {
		return "no expiry"
	}
	return n.Until().Local().Format("2006-01-02")
}
