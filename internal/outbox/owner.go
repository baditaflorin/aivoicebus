package outbox

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// OwnerRecords lists the live owner records on the nodes (leases whose key starts owner:, see
// msg/owner.go), each with the verdict on its signature: the client reads the lease's message to get
// it, once per record that keep accepts (nil keeps all). The lease list says who declared a record
// and what it says; only the message says whether the machine's key really signed it.
func OwnerRecords(c *client.Client, keep func(subject string) bool) ([]msg.OwnerRecord, error) {
	live, err := c.Leases()
	switch {
	case client.IsNotFound(err):
		return nil, fmt.Errorf("this node does not hold leases, which is where owner records live: ask the user to update the nodes (`aivoicebus nodes` lists them)")
	case err != nil:
		return nil, fmt.Errorf("the nodes could not list the owner records (%v): try again", err)
	}
	var out []msg.OwnerRecord
	for _, l := range live {
		rec, ok := msg.OwnerOf(l)
		if !ok || (keep != nil && !keep(rec.Subject)) {
			continue
		}
		if rec.Problem == "" {
			rec.Verified = verdictOf(c, rec.ID)
		}
		out = append(out, rec)
	}
	slices.SortStableFunc(out, func(a, b msg.OwnerRecord) int {
		if c := strings.Compare(a.Subject, b.Subject); c != 0 {
			return c
		}
		return a.At.Compare(b.At)
	})
	return out, nil
}

// verdictOf is the signature verdict on message id ("" when it could not be read).
func verdictOf(c *client.Client, id string) string {
	ms, err := c.Thread(id)
	if err != nil {
		return ""
	}
	for _, m := range ms {
		if m.ID == id {
			return m.Verified
		}
	}
	return ""
}

// OwnerPins is the sender's trusted declarers.
func OwnerPins() ([]string, error) {
	text, err := config.ReadOwnerTrust()
	if err != nil { // an unusable file is no pin: say so rather than fall back to none
		return nil, fmt.Errorf("the owner-trust file is unusable (%v): fix or remove it, or ask the user", err)
	}
	return msg.ParseOwnerPins(text), nil
}

// ResolveOwner finds who answers for subject: the live records about it, checked and folded by
// msg.ResolveOwner. The resolution is filled in as far as it got when there is an error (the records
// that were not used, for `owner get`).
func ResolveOwner(c *client.Client, subject string) (msg.OwnerResolution, error) {
	res := msg.OwnerResolution{Subject: subject}
	if err := msg.ValidOwnerLookup(subject); err != nil {
		return res, Usagef("owner:%s: %v: `aivoicebus owner list` shows what is recorded", msg.SafeName(subject), err)
	}
	recs, err := OwnerRecords(c, func(s string) bool { return msg.ValidOwnerSubject(s) == nil && msg.MatchKey(s, subject) })
	if err != nil {
		return res, fmt.Errorf("owner:%s cannot be resolved: %w", msg.SafeName(subject), err)
	}
	pins, err := OwnerPins()
	if err != nil {
		return res, fmt.Errorf("owner:%s cannot be resolved: %w", msg.SafeName(subject), err)
	}
	return msg.ResolveOwner(subject, recs, pins, time.Now())
}
