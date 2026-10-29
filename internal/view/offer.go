package view

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// OfferLine is what a session is told about a task the nodes offer to it alone (m.Offer, msg/offer.go):
// one line under the task, saying until when it is this session's to take, and the two ways to answer.
// "" when m carries no offer.
func OfferLine(m msg.Message) string {
	o := m.Offer
	if o == nil {
		return ""
	}
	id := OneLine(m.ID, msg.MaxRef)
	turn := "first"
	if o.Before > 0 {
		turn = fmt.Sprintf("%d had it before", o.Before)
	}
	after := "nobody else is about to take it after"
	if o.Left > 0 {
		after = fmt.Sprintf("%d more could take it after", o.Left)
	}
	return fmt.Sprintf("[aivoicebus] Task %s went to %s, not to one session: the nodes offer it to this session alone until %s (%s; %s). "+
		"If this session can take it now, claim it: aivoicebus claim %s, and tell the user what it asks before doing it. "+
		`If not: aivoicebus decline %s "why", and the next session is offered it at once.`,
		id, Name(m.To, msg.MaxIdentity), clockUTC(o.Until), turn, after, id, id)
}

// OfferLines writes the OfferLine of each message in ms the nodes offer to the reader alone.
func OfferLines(w io.Writer, ms []msg.Message) {
	for _, m := range ms {
		if line := OfferLine(m); line != "" {
			fmt.Fprintln(w, line)
		}
	}
}

// clockUTC is t as a UTC clock time to the minute: a session on another machine reads it too.
func clockUTC(t time.Time) string { return t.UTC().Format("15:04") + "Z" }

// maxDigest is how many sessions OfferDigest names before it counts the rest.
const maxDigest = 6

// OfferDigest is what became of a task the nodes offered one session at a time, for its sender: who was
// offered it, in order, and how each offer ended, then whether the person was told. "" for a message
// that was not offered round (no Offers in its receipts).
func OfferDigest(r msg.Receipts, now time.Time) string {
	if len(r.Offers) == 0 && r.Escalated == nil {
		return ""
	}
	var parts []string
	for i, st := range r.Offers {
		if i == maxDigest {
			parts = append(parts, fmt.Sprintf("and %d more", len(r.Offers)-maxDigest))
			break
		}
		what := st.Outcome
		switch st.Outcome {
		case msg.OfferOpen:
			what = "offered it now, until " + When(st.Until, now)
		case msg.OfferPassed:
			what = "passed"
		case msg.OfferClaimed:
			what = "took it"
		case msg.OfferDeclined:
			what = "declined"
			if st.Note != "" {
				what += `: "` + strings.ReplaceAll(OneLine(Sanitise(st.Note), 80), `"`, "'") + `"`
			}
		}
		parts = append(parts, Name(st.By, msg.MaxIdentity)+" ("+what+")")
	}
	line := "offered one session at a time"
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	} else {
		line += ": to nobody yet"
	}
	if r.Escalated != nil {
		line += fmt.Sprintf("; the person was told at %s, by %s", When(r.Escalated.At, now), Name(r.Escalated.By, msg.MaxIdentity))
	}
	return line
}
