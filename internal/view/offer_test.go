package view

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestTheOfferLineSaysUntilWhenAndHowToTakeOrPassIt(t *testing.T) {
	until := time.Date(2026, 10, 25, 14, 40, 0, 0, time.UTC)
	m := msg.Message{ID: "0123456789", To: "lap", Kind: msg.KindTask, Offer: &msg.Offer{Until: until, Before: 2, Left: 3}}
	got := OfferLine(m)
	for _, want := range []string{"Task 0123456789 went to lap, not to one session", "until 14:40Z", "2 had it before", "3 more could take it after",
		"aivoicebus claim 0123456789", `aivoicebus decline 0123456789 "why"`, "tell the user what it asks before doing it"} {
		if !strings.Contains(got, want) {
			t.Errorf("offer line lacks %q: %s", want, got)
		}
	}
	m.Offer.Before, m.Offer.Left = 0, 0
	if got := OfferLine(m); !strings.Contains(got, "(first; nobody else is about to take it after)") {
		t.Errorf("first and last: %s", got)
	}
	m.Offer = nil
	if OfferLine(m) != "" {
		t.Error("a message with no offer has an offer line")
	}
	var b strings.Builder
	OfferLines(&b, []msg.Message{m, {ID: "1123456789", To: "x\x1b[2Jy", Offer: &msg.Offer{Until: until}}})
	if strings.Count(b.String(), "\n") != 1 || strings.Contains(b.String(), "\x1b") {
		t.Errorf("OfferLines: %q", b.String())
	}
}

func TestTheDigestSaysWhoWasOfferedATaskAndWhatEachDid(t *testing.T) {
	now := time.Now()
	r := msg.Receipts{Offers: []msg.OfferStep{
		{By: "lap/claude.a1", At: now.Add(-30 * time.Minute), Outcome: msg.OfferPassed},
		{By: "lap/claude.b2", Outcome: msg.OfferDeclined, Note: `not "my" repo` + "\n\x1b[31mred"},
		{By: "lap/claude.c3", Outcome: msg.OfferClaimed},
	}, Escalated: &msg.Receipt{By: "lap/claude.b2", At: now}}
	got := OfferDigest(r, now)
	want := `offered one session at a time: lap/claude.a1 (passed), lap/claude.b2 (declined: "not 'my' repo red"), lap/claude.c3 (took it); the person was told at `
	if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "by lap/claude.b2") {
		t.Errorf("digest:\n%s\nwant prefix\n%s", got, want)
	}
	if OfferDigest(msg.Receipts{}, now) != "" {
		t.Error("a message that was not offered round has a digest")
	}
	var many msg.Receipts
	for i := 0; i < maxDigest+3; i++ {
		many.Offers = append(many.Offers, msg.OfferStep{By: "lap/claude.x", Outcome: msg.OfferPassed})
	}
	if got := OfferDigest(many, now); strings.Count(got, "lap/claude.x") != maxDigest || !strings.HasSuffix(got, "and 3 more") {
		t.Errorf("a long digest: %s", got)
	}
}
