package view

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// digestRunes bounds each quoted line of a digest.
const digestRunes = 140

// Digest is a thread in a few lines, for an agent catching up (a woken standby session, say)
// without reading every message: who took part, the task's state, each side's last word, and
// whether it is still open. Quoted text is peer text, cut to one line.
func Digest(w io.Writer, ms []msg.Message) {
	if len(ms) == 0 {
		return
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].At.Before(ms[j].At) })
	first, lastAt := ms[0], ms[len(ms)-1].At
	text := func(m msg.Message) string {
		if m.Retracted != nil {
			return "(taken back)"
		}
		return `"` + OneLine(sealedText(m), digestRunes) + `"`
	}
	type side struct {
		n    int
		last msg.Message
	}
	sides := map[string]*side{}
	var order []string
	var takes []msg.TakeRef
	var end *msg.Message
	for i, m := range ms {
		sd, ok := sides[m.From]
		if !ok {
			sd = &side{}
			sides[m.From] = sd
			order = append(order, m.From)
		}
		sd.n++
		if m.Kind != msg.KindClaim {
			sd.last = m
		}
		if msg.Takes(m) && m.Retracted == nil && m.Re == first.ID {
			takes = append(takes, msg.TakeRef{ID: m.ID, From: m.From, At: m.At})
		}
		if m.Kind == msg.KindEnd && m.Retracted == nil {
			end = &ms[i]
		}
	}
	fmt.Fprintln(w, UntrustedBanner)
	fmt.Fprintf(w, "thread %s: %d message(s) from %d session(s), %s to %s (%s)\n", first.Thread, len(ms), len(order),
		When(first.At, time.Now()), When(lastAt, time.Now()), lastAt.Sub(first.At).Round(time.Second))
	fmt.Fprintf(w, "  size: %s\n", SizeLine(msg.SizeOf(ms)))
	what := "started"
	if first.Kind == msg.KindTask {
		what = "task"
	}
	fmt.Fprintf(w, "  %s by %s -> %s: %s\n", what, Name(first.From, msg.MaxIdentity), Name(first.To, msg.MaxIdentity), text(first))
	if first.Retracted != nil {
		fmt.Fprintf(w, "  withdrawn by its sender at %s: nobody needs to act on it\n", WhenSec(first.Retracted.At, time.Now()))
	} else if first.Kind == msg.KindTask {
		if t, ok := msg.TaskOwner(first.From, takes); ok {
			fmt.Fprintf(w, "  taken by %s at %s\n", Name(t.From, msg.MaxIdentity), WhenSec(t.At, time.Now()))
		} else {
			fmt.Fprintln(w, "  nobody has taken it")
		}
	}
	v := msg.ThreadState(ms)
	if v.State != "" {
		fmt.Fprintf(w, "  %s\n", StateLine(v))
		if n := len(v.Ignored); n > 0 {
			fmt.Fprintf(w, "  %d state message(s) the rules ignored (aivoicebus state %s lists them)\n", n, first.ID)
		}
	}
	for _, who := range order {
		sd := sides[who]
		if sd.last.ID == "" || sd.last.ID == first.ID {
			continue
		}
		fmt.Fprintf(w, "  last from %s (%d, %s): %s\n", Name(who, msg.MaxIdentity), sd.n, WhenSec(sd.last.At, time.Now()), text(sd.last))
	}
	switch last := ms[len(ms)-1]; {
	case first.Retracted != nil && len(ms) == 1:
	case end != nil:
		fmt.Fprintf(w, "  ended by %s at %s\n", Name(end.From, msg.MaxIdentity), WhenSec(end.At, time.Now()))
	case v.Final():
		fmt.Fprintf(w, "  closed: %s by %s at %s\n", v.State, Name(v.By, msg.MaxIdentity), WhenSec(v.At, time.Now()))
	default:
		fmt.Fprintf(w, "  open: the last word is %s's\n", Name(last.From, msg.MaxIdentity))
	}
}

// SizeLine is a thread's size (msg.SizeOf) on one line: "12 messages, 4310 characters (about 1077
// tokens to read); agents reported 14212 tokens in, 5 out (1 run)".
func SizeLine(z msg.ThreadSize) string {
	line := fmt.Sprintf("%d message(s), %d characters (about %d tokens to read)", z.Messages, z.Chars, z.EstTokens)
	if z.BudgetChars > 0 {
		line = fmt.Sprintf("%d message(s), %d of its %d characters (--budget-chars; about %d tokens to read)", z.Messages, z.Chars, z.BudgetChars, z.EstTokens)
	}
	if z.Runs > 0 {
		line += fmt.Sprintf("; agents reported %d tokens in, %d out (%d run(s))", z.TokensIn, z.TokensOut, z.Runs)
		if z.CostUSD > 0 {
			line += fmt.Sprintf(", $%.4f", z.CostUSD)
		}
	}
	return line
}

// StaleSummary is the body of the [end] that `thread ID --close-stale` writes: why it closes, and
// the thread in a few lines (its size, its state, each side's last word, quoted and cut), so a
// reader who missed it knows where it stood. ms is one thread, its last message quiet since idle.
func StaleSummary(ms []msg.Message, idle time.Duration, by string) string {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].At.Before(ms[j].At) })
	last := ms[len(ms)-1]
	var b strings.Builder
	fmt.Fprintf(&b, "[closed as stale] no message in this conversation for %s (the last from %s); closed by %s.\n",
		idle.Round(time.Minute), Name(last.From, msg.MaxIdentity), by)
	fmt.Fprintf(&b, "size: %s\n", SizeLine(msg.SizeOf(ms)))
	if v := msg.ThreadState(ms); v.State != "" {
		fmt.Fprintf(&b, "%s\n", StateLine(v))
	}
	seen := map[string]bool{}
	for i := len(ms) - 1; i >= 0 && len(seen) < 4; i-- {
		m := ms[i]
		if seen[m.From] || m.Kind == msg.KindClaim || m.Retracted != nil {
			continue
		}
		seen[m.From] = true
		fmt.Fprintf(&b, "last from %s (%s): %q\n", Name(m.From, msg.MaxIdentity), m.At.UTC().Format("2006-01-02 15:04Z"), OneLine(sealedText(m), 100))
	}
	b.WriteString("If it is still needed, start it again with `aivoicebus send`.")
	return b.String()
}
