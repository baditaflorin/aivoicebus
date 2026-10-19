package cli

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// `mesh --signed-check` says whether the nodes can be switched to `serve --require-signed` without
// refusing anyone: READY only when no node holds a message without a valid signature from the
// window (each node keeps every message it took in, so each answers for the mesh's traffic), and
// every node answered. Otherwise it names the senders, so their machines can be updated first.

type signedNode struct {
	URL           string `json:"url"`
	Node          string `json:"node,omitempty"`
	Scanned       int    `json:"scanned"`
	RequireSigned bool   `json:"require_signed"`
	Error         string `json:"error,omitempty"`
}

type signedView struct {
	Days      int                  `json:"days"`
	Ready     bool                 `json:"ready"`
	Nodes     []signedNode         `json:"nodes"`
	Offenders []msg.UnsignedSender `json:"offenders"`
	Verdict   string               `json:"verdict"`
}

func signedCheck(c *client.Client, days int, timeout time.Duration, asJSON bool, s Streams) error {
	if days < 1 || days > 30 {
		return outbox.Usagef("--days is from 1 to 30")
	}
	v := buildSignedView(c, days, timeout)
	if asJSON {
		if err := writeJSON(s.Out, v); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(s.Out, "messages without a valid signature in the last %dd (what serve --require-signed refuses):\n", days)
		for _, n := range v.Nodes {
			switch {
			case n.Error != "":
				fmt.Fprintf(s.Out, "  %s: %s\n", view.OneLine(n.URL, 60), view.OneLine(n.Error, 200))
			case n.RequireSigned:
				fmt.Fprintf(s.Out, "  %s: %d message(s) checked; already refuses unsigned ones\n", view.OneLine(n.Node, 40), n.Scanned)
			default:
				fmt.Fprintf(s.Out, "  %s: %d message(s) checked\n", view.OneLine(n.Node, 40), n.Scanned)
			}
		}
		if len(v.Offenders) > 0 {
			tw := tabwriter.NewWriter(s.Out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "  SENDER\tUNSIGNED\tBAD SIGNATURE\tLAST\tLAST ID")
			for _, o := range v.Offenders {
				fmt.Fprintf(tw, "  %s\t%d\t%d\t%s\t%s\n", view.Name(o.From, 60), o.Unsigned, o.Bad, o.LastAt.Local().Format("2006-01-02 15:04 MST"), view.OneLine(o.LastID, 40))
			}
			tw.Flush()
		}
		fmt.Fprintln(s.Out, v.Verdict)
	}
	if !v.Ready {
		return exitError{exitFailure}
	}
	return nil
}

// buildSignedView asks each node by itself (one that is down must not hide what another holds) and
// merges the senders: the most any node counted, and the newest message.
func buildSignedView(c *client.Client, days int, timeout time.Duration) signedView {
	v := signedView{Days: days, Nodes: make([]signedNode, len(c.URLs)), Offenders: []msg.UnsignedSender{}}
	reps := make([]msg.UnsignedReport, len(c.URLs))
	var wg sync.WaitGroup
	for i, u := range c.URLs {
		v.Nodes[i].URL = u
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			one := &client.Client{URLs: []string{u}, Token: c.Token, Timeout: timeout}
			r, err := one.Unsigned(days)
			switch {
			case client.IsNotFound(err):
				err = errors.New("runs a release that does not report unsigned messages (unsigned-report); update it to check")
			case err == nil:
				reps[i] = r
				v.Nodes[i].Node, v.Nodes[i].Scanned, v.Nodes[i].RequireSigned = r.Node, r.Scanned, r.RequireSigned
			}
			if err != nil {
				v.Nodes[i].Error = err.Error()
			}
		}(i, u)
	}
	wg.Wait()
	by := map[string]*msg.UnsignedSender{}
	for _, r := range reps {
		for _, o := range r.Senders {
			e := by[o.From]
			if e == nil {
				e = &msg.UnsignedSender{From: o.From}
				by[o.From] = e
			}
			e.Unsigned, e.Bad = max(e.Unsigned, o.Unsigned), max(e.Bad, o.Bad)
			if o.LastAt.After(e.LastAt) {
				e.LastAt, e.LastID = o.LastAt, o.LastID
			}
		}
	}
	for _, e := range by {
		v.Offenders = append(v.Offenders, *e)
	}
	sort.Slice(v.Offenders, func(i, j int) bool { return v.Offenders[i].LastAt.After(v.Offenders[j].LastAt) })
	var down int
	for _, n := range v.Nodes {
		if n.Error != "" {
			down++
		}
	}
	switch {
	case len(v.Offenders) > 0:
		v.Verdict = fmt.Sprintf("NOT READY: %d sender(s) sent messages without a valid signature in the last %dd; --require-signed would refuse them. Update those machines (aivoicebus update: every client of %s or later signs), then check again.", len(v.Offenders), days, msg.SignSince)
	case down > 0:
		v.Verdict = fmt.Sprintf("NOT READY: %d of %d node(s) did not answer; a sender only they hold could be unsigned.", down, len(v.Nodes))
	default:
		v.Ready = true
		v.Verdict = fmt.Sprintf("READY: no message without a valid signature in the last %dd on any of %d node(s). The switch: docs/operations.md, \"Signed messages only\".", days, len(v.Nodes))
	}
	return v
}
