package cli

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// machineFacts is what one machine's sessions said it has: the newest facts any of them declared
// (msg/facts.go), and when the machine was last heard from at all.
type machineFacts struct {
	Seen  time.Time // its most recently heard session
	From  string    // the session the facts are from; "" when none of them said any
	At    time.Time // when that session declared them
	Facts msg.Facts
}

// factsByMachine groups sessions by machine, keeping per machine the newest facts declared there.
func factsByMachine(rows []msg.RosterEntry) map[string]machineFacts {
	out := map[string]machineFacts{}
	for _, e := range rows {
		h := msg.HostOf(e.Me)
		cur := out[h]
		if e.Seen.After(cur.Seen) {
			cur.Seen = e.Seen
		}
		at := e.LabelsAt
		if at.IsZero() {
			at = e.Seen
		}
		if f := msg.FactsOf(e.Labels); f.Known && (cur.From == "" || at.After(cur.At)) {
			cur.From, cur.At, cur.Facts = e.Me, at, f
		}
		out[h] = cur
	}
	return out
}

// factsText is the FACTS column of `fleet`: the facts in words, none, or - when the machine said nothing.
func factsText(m machineFacts) string {
	switch {
	case m.From == "":
		return "-"
	case len(m.Facts.Words()) == 0:
		return msg.FactNone
	}
	return strings.Join(m.Facts.Words(), " ")
}

// factAnswer is one machine's answer to a --has.
type factAnswer struct {
	Machine string    `json:"machine"`
	Answer  string    `json:"answer"`          // yes, no, or unknown (its sessions said nothing)
	Value   string    `json:"value,omitempty"` // the version or alias that answered (on a no: the version it has)
	Seen    time.Time `json:"seen"`            // the machine's most recently heard session
	From    string    `json:"from,omitempty"`  // the session whose labels answered
}

// factAnswers is every machine in rows with its answer to q: yes first, then no, then unknown,
// each by machine name.
func factAnswers(rows []msg.RosterEntry, q msg.FactQuery) []factAnswer {
	byMachine := factsByMachine(view.SanitiseRoster(rows))
	rank := map[string]int{"yes": 0, "no": 1, "unknown": 2}
	var out []factAnswer
	for _, h := range slices.Sorted(maps.Keys(byMachine)) {
		m := byMachine[h]
		a := factAnswer{Machine: h, Answer: "unknown", Seen: m.Seen, From: m.From}
		if m.From != "" {
			yes, v := q.Match(m.Facts) // on a no, v is the version it has instead, if any
			a.Answer, a.Value = "no", v
			if yes {
				a.Answer = "yes"
			}
		}
		out = append(out, a)
	}
	slices.SortStableFunc(out, func(a, b factAnswer) int { return rank[a.Answer] - rank[b.Answer] })
	return out
}

// printFactAnswers writes the --has table and a line that says what it means.
func printFactAnswers(w io.Writer, q msg.FactQuery, answers []factAnswer, now time.Time, recent string) {
	if len(answers) == 0 {
		fmt.Fprintf(w, "(no session heard from %s, so no machine to ask about %s)\n", recent, q)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  MACHINE\tHEARD\t"+strings.ToUpper(q.String())+"\t")
	var yes, unknown []string
	for _, a := range answers {
		cell := a.Answer
		if a.Value != "" {
			cell += " (" + view.Name(a.Value, 32) + ")"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t\n", view.Name(a.Machine, msg.MaxIdentity), agoText(now, a.Seen), cell)
		switch a.Answer {
		case "yes":
			yes = append(yes, view.Name(a.Machine, msg.MaxIdentity))
		case "unknown":
			unknown = append(unknown, view.Name(a.Machine, msg.MaxIdentity))
		}
	}
	tw.Flush()
	switch len(yes) {
	case 0:
		fmt.Fprintf(w, "no machine heard from %s says it has %s\n", recent, q)
	default:
		fmt.Fprintf(w, "%s: %s\n", q, strings.Join(yes, ", "))
	}
	if len(unknown) > 0 {
		fmt.Fprintf(w, "unknown: %s: no session there says what it has (an older client, AIVOICEBUS_FACTS=off, or its labels are full): not a no; ask there\n", strings.Join(unknown, ", "))
	}
	fmt.Fprintf(w, "facts are what each machine says of itself, worked out at most every %d minutes; `aivoicebus whoami --labels` shows this machine's (has=, ssh=)\n", int(identity.FactsTTL/time.Minute))
}
