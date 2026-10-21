package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

const (
	amSender = "imac/claude.s1"  // sent the original
	amOther  = "imac/claude.s2"  // another session on the sender's machine
	amReadA  = "air/codex.r1"    // reads the original before it is amended
	amReadB  = "air/codex.r2"    // has not read it when it is amended
	amFar    = "other/claude.x9" // a session on another machine
)

// amendStore is a store whose clock moves one minute with every call of the function it returns.
func amendStore(t *testing.T) (*Store, func()) {
	t.Helper()
	s, err := Open(t.TempDir(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	s.SetSendLimit(1 << 20)
	now := time.Date(2026, 10, 21, 4, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	return s, func() { now = now.Add(time.Minute) }
}

// amendTo sends text as from to replace o, as `aivoicebus amend` does: a message to the same target,
// in the same thread, at the same priority, then the claim.
func amendTo(t *testing.T, s *Store, from string, o msg.Message, text string) msg.Message {
	t.Helper()
	a, err := s.Send(msg.Message{From: from, To: o.To, Re: o.ID, Prio: o.Prio, Body: text})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Amend(a.ID, from); err != nil {
		t.Fatalf("amend: %v", err)
	}
	return a
}

func put2(t *testing.T, s *Store, from, to, prio, body string) msg.Message {
	t.Helper()
	m, err := s.Send(msg.Message{From: from, To: to, Prio: prio, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ids(ms []msg.Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestAmendmentGivesAReaderWhoHasNotReadTheOriginalOnlyTheNewText(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", msg.PrioUrgent, "deploy to prod at noon")
	tick()
	a := amendTo(t, s, amSender, o, "cancelled: do NOT deploy")

	got := s.Inbox(amReadB, false)
	if !slices.Equal(ids(got), []string{a.ID}) {
		t.Fatalf("an unread reader is given the amendment only, got %v (original %s, amendment %s)", ids(got), o.ID, a.ID)
	}
	if got[0].Amends == nil || got[0].Amends.Of != o.ID || got[0].Amends.Read != "" {
		t.Errorf("it replaces the original and this reader read nothing: %+v", got[0].Amends)
	}
	if got[0].Body != "cancelled: do NOT deploy" || got[0].Prio != msg.PrioUrgent {
		t.Errorf("the words and the priority are the sender's, untouched: %+v", got[0])
	}
	if got[0].Retracted != nil {
		t.Error("an amendment is not a retraction")
	}
}

func TestAmendmentTellsAReaderWhoReadTheOriginalWhichVersionItRead(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "deploy to prod at noon")
	if in := s.Inbox(amReadA, false); !slices.Equal(ids(in), []string{o.ID}) {
		t.Fatalf("it reads the original: %v", ids(in))
	}
	if err := s.Ack(o.ID, amReadA); err != nil {
		t.Fatal(err)
	}
	tick()
	a := amendTo(t, s, amSender, o, "deploy at 13:00, not noon")

	got := s.Inbox(amReadA, false)
	if !slices.Equal(ids(got), []string{a.ID}) || got[0].Amends == nil || got[0].Amends.Read != o.ID || got[0].Amends.Of != o.ID {
		t.Fatalf("a reader of the original is given the amendment, told what it had read: %+v", got)
	}
	// and the one that did not read it is not told it replaces something it read
	if g := s.Inbox(amReadB, false); len(g) != 1 || g[0].Amends == nil || g[0].Amends.Read != "" {
		t.Errorf("%+v", g)
	}
}

func TestAnFyiThatWasOnlyCountedIsNotARead(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", msg.PrioFYI, "FYI: building")
	if err := s.AckWith(msg.Ack{ID: o.ID, By: amReadA, Via: msg.ViaHookCount}); err != nil { // the session start only counted it
		t.Fatal(err)
	}
	tick()
	amendTo(t, s, amSender, o, "FYI: built")
	got := s.Inbox(amReadA, false)
	if len(got) != 1 || got[0].Amends == nil || got[0].Amends.Read != "" {
		t.Errorf("it never saw the text of the original, so it replaces nothing it read: %+v", got)
	}
}

func TestOriginalAndAmendmentBothListedInAllWithForwardLink(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "prod is healthy")
	tick()
	a := amendTo(t, s, amSender, o, "prod is NOT healthy")
	all := s.Inbox(amReadB, true)
	if !slices.Equal(ids(all), []string{o.ID, a.ID}) {
		t.Fatalf("--all lists both: %v", ids(all))
	}
	if all[0].AmendedBy == nil || all[0].AmendedBy.By != a.ID || !all[0].AmendedBy.At.Equal(a.At) || all[0].Amends != nil {
		t.Errorf("the original links forward and is no amendment: %+v %+v", all[0].AmendedBy, all[0].Amends)
	}
	if all[0].Body != "prod is healthy" {
		t.Error("the original's words are not rewritten or hidden")
	}
	if all[1].Amends == nil || all[1].Amends.Of != o.ID || all[1].AmendedBy != nil {
		t.Errorf("%+v %+v", all[1].Amends, all[1].AmendedBy)
	}
}

func TestOnlyTheMachineThatSentTheOriginalMayAmendIt(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "deploy at noon")
	tick()
	// another session of the sender's machine may (as for a retraction), another machine may not
	if _, err := s.Send(msg.Message{From: amFar, To: o.To, Re: o.ID, Body: "no, 13:00"}); err != nil {
		t.Fatal(err)
	}
	var far msg.Message
	for _, m := range s.Thread(o.ID) {
		if m.From == amFar {
			far = m
		}
	}
	var na *ErrNotAmender
	if err := s.Amend(far.ID, amFar); !errors.Is(err, ErrNotAmendment) {
		t.Errorf("a message from another machine is no amendment of it: %v", err)
	}
	mine, err := s.Send(msg.Message{From: amSender, To: o.To, Re: o.ID, Body: "13:00"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Amend(mine.ID, amFar); !errors.As(err, &na) || na.Sender != amSender {
		t.Errorf("another machine cannot claim it: %v", err)
	}
	if err := s.Amend(mine.ID, amOther); err != nil {
		t.Errorf("a session of the same machine can: %v", err)
	}
	if got := s.Inbox(amReadB, false); !slices.Equal(ids(got), []string{far.ID, mine.ID}) {
		t.Errorf("the other machine's reply is only a reply: it lives beside the amendment, and the original is replaced: %v", ids(got))
	}
}

func TestAClaimByAnotherMachineChangesNothing(t *testing.T) {
	// a record that arrives from a peer is taken as it is: the claim must not hide a message
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "deploy at noon")
	tick()
	a, err := s.Send(msg.Message{From: amSender, To: o.To, Re: o.ID, Body: "deploy at 13:00"})
	if err != nil {
		t.Fatal(err)
	}
	forged := msg.Record{Origin: "peer", Seq: 1, Ack: &msg.Ack{ID: msg.AmendPrefix + a.ID, By: amFar, At: s.now()}}
	if n, err := s.Merge([]msg.Record{forged}); err != nil || n != 1 {
		t.Fatalf("it is stored like any ack: %d %v", n, err)
	}
	if got := s.Inbox(amReadB, false); !slices.Equal(ids(got), []string{o.ID, a.ID}) {
		t.Errorf("nothing was replaced: %v", ids(got))
	}
	if all := s.Inbox(amReadB, true); all[0].AmendedBy != nil || all[1].Amends != nil {
		t.Error("no link is shown either")
	}
}

func TestAnAmendmentGoesToTheSameTargetInTheSameThreadAndIsAPlainNote(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "deploy at noon")
	tick()
	elsewhere, _ := s.Send(msg.Message{From: amSender, To: "air/codex.r2", Re: o.ID, Body: "deploy at 13:00"})
	if err := s.Amend(elsewhere.ID, amSender); !errors.Is(err, ErrNotAmendment) {
		t.Errorf("another target: %v", err)
	}
	question, _ := s.Send(msg.Message{From: amSender, To: o.To, Re: o.ID, Kind: msg.KindQuestion, Body: "13:00?"})
	if err := s.Amend(question.ID, amSender); !errors.Is(err, ErrNotAmendment) {
		t.Errorf("a question is not a plain note: %v", err)
	}
	// the claims were refused at write time
	if got := s.Inbox(amReadB, false); !slices.Contains(ids(got), o.ID) {
		t.Errorf("the original is still delivered: %v", ids(got))
	}
}

func TestAmendmentsAgreeWhateverOrderTheRecordsArriveIn(t *testing.T) {
	// the original and the amendment come from one node, the claim from another
	a, tick := amendStore(t)
	o := put2(t, a, amSender, "air/codex", "", "deploy at noon")
	tick()
	am, err := a.Send(msg.Message{From: amSender, To: o.To, Re: o.ID, Body: "deploy at 13:00"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Open(t.TempDir(), "n2")
	b.SetClock(a.now)
	if _, err := b.Merge(a.Since(nil, 100)); err != nil {
		t.Fatal(err)
	}
	if err := b.Amend(am.ID, amSender); err != nil {
		t.Fatal(err)
	}
	var recs []msg.Record
	recs = append(recs, a.Since(nil, 100)...)
	for _, r := range b.Since(a.VV(), 100) {
		recs = append(recs, r)
	}
	want := ""
	for name, seq := range orders(recs) {
		s, _ := Open(t.TempDir(), "n3")
		s.SetClock(a.now)
		for _, r := range seq {
			if _, err := s.Merge([]msg.Record{r}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		in := s.Inbox(amReadB, false)
		got := fmt.Sprint(ids(in))
		if len(in) == 1 && in[0].Amends != nil {
			got += " amends " + in[0].Amends.Of
		}
		if want == "" {
			want = got
		}
		if got != want || !strings.Contains(got, am.ID) || strings.Contains(got, "["+o.ID) {
			t.Errorf("%s: %s, other orders gave %s", name, got, want)
		}
	}
}

func TestAChainOfAmendmentsDeliversTheLatestAndNamesTheVersionRead(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "v1: deploy at noon")
	if err := s.Ack(o.ID, amReadA); err != nil { // read v1
		t.Fatal(err)
	}
	tick()
	a1 := amendTo(t, s, amSender, o, "v2: deploy at 13:00")
	if err := s.Ack(a1.ID, "air/codex.r3"); err != nil { // read v2 only (never saw v1: it was gone)
		t.Fatal(err)
	}
	tick()
	a2 := amendTo(t, s, amSender, a1, "v3: deploy at 14:00")

	for _, c := range []struct{ who, read string }{{amReadB, ""}, {amReadA, o.ID}, {"air/codex.r3", a1.ID}} {
		got := s.Inbox(c.who, false)
		if len(got) != 1 || got[0].ID != a2.ID || got[0].Amends == nil || got[0].Amends.Of != a1.ID || got[0].Amends.Read != c.read {
			t.Errorf("%s: %+v (want v3 only, read %q)", c.who, got, c.read)
		}
	}
	th := s.Thread(o.ID)
	if len(th) != 3 || th[0].AmendedBy == nil || th[0].AmendedBy.By != a1.ID || th[1].AmendedBy == nil || th[1].AmendedBy.By != a2.ID || th[2].AmendedBy != nil {
		t.Fatalf("the thread shows the whole chain: %+v", th)
	}
	if th[1].Amends == nil || th[1].Amends.Of != o.ID || th[2].Amends.Of != a1.ID || th[2].Amends.Read != "" {
		t.Errorf("a thread read has no reader, so nothing was read: %+v %+v", th[1].Amends, th[2].Amends)
	}
}

func TestOfTwoAmendmentsOfTheSameMessageTheLaterWinsAndTheOtherIsSuperseded(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "deploy at noon")
	tick()
	first := amendTo(t, s, amSender, o, "deploy at 13:00")
	tick()
	second := amendTo(t, s, amOther, o, "deploy at 14:00") // a second session of the machine, who had not seen the first
	got := s.Inbox(amReadB, false)
	if !slices.Equal(ids(got), []string{second.ID}) {
		t.Errorf("the latest wins, and the one it raced is not left beside it: %v (first %s second %s)", ids(got), first.ID, second.ID)
	}
}

func TestRetractingTheAmendmentGivesTheOriginalBack(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "deploy at noon")
	tick()
	a := amendTo(t, s, amSender, o, "deploy at 13:00 (typo)")
	if got := s.Inbox(amReadB, false); !slices.Equal(ids(got), []string{a.ID}) {
		t.Fatalf("%v", ids(got))
	}
	if _, err := s.Retract(a.ID, amSender, false); err != nil {
		t.Fatal(err)
	}
	got := s.Inbox(amReadB, false)
	if !slices.Equal(ids(got), []string{o.ID}) || got[0].AmendedBy != nil {
		t.Errorf("the amendment is withdrawn, so the original is delivered again and links nowhere: %+v", got)
	}
}

func TestAmendmentsThatPointAtEachOtherDoNotHangOrHideEverything(t *testing.T) {
	s, tick := amendStore(t)
	x := msg.Message{ID: "aaaaaaaaaa", From: amSender, To: "air/codex", Re: "bbbbbbbbbb", Thread: "aaaaaaaaaa", Body: "x"}
	y := msg.Message{ID: "bbbbbbbbbb", From: amSender, To: "air/codex", Re: "aaaaaaaaaa", Thread: "aaaaaaaaaa", Body: "y"}
	if _, err := s.Send(x); err != nil {
		t.Fatal(err)
	}
	tick()
	if _, err := s.Send(y); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{x.ID, y.ID} {
		if err := s.Amend(id, amSender); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Inbox(amReadB, false)
		s.Inbox(amReadB, true)
		s.Thread(x.ID)
		for _, m := range s.Inbox(amReadB, true) {
			s.mu.Lock()
			s.readVersionLocked(m, amReadB)
			s.mu.Unlock()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a loop of amendments made a read hang")
	}
}

func TestAmendmentsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	s.SetSendLimit(1 << 20)
	o := put2(t, s, amSender, "air/codex", "", "deploy at noon")
	a := amendTo(t, s, amSender, o, "deploy at 13:00")
	re, err := Open(dir, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got := re.Inbox(amReadB, false); !slices.Equal(ids(got), []string{a.ID}) {
		t.Errorf("the claim is in the log: %v", ids(got))
	}
}

func TestAmendWritesOneClaimAndIsNoReadReceipt(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "deploy at noon")
	tick()
	a, _ := s.Send(msg.Message{From: amSender, To: o.To, Re: o.ID, Body: "deploy at 13:00"})
	before := s.Stats()
	for range 3 {
		if err := s.Amend(a.ID, amSender); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(s.log[s.node]); n != 3 { // the original, the amendment, one claim
		t.Errorf("a claim made again writes nothing: %d records", n)
	}
	if st := s.Stats(); st.Acks != before.Acks {
		t.Errorf("a claim is no read receipt: %d acks, %d before", st.Acks, before.Acks)
	}
	if rc, ok := s.Receipts(a.ID); ok && len(rc.Reads) != 0 {
		t.Errorf("%+v", rc)
	}
}

func TestAmendRefusals(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "deploy at noon")
	tick()
	for name, c := range map[string]struct {
		id, by string
		want   error
	}{
		"not an id":    {"nope", amSender, ErrBadAmend},
		"uppercase id": {"ABCDEF0123", amSender, ErrBadAmend},
		"no identity":  {o.ID, "", ErrBadAmend},
		"unknown":      {"0123456789", amSender, ErrUnknown},
		"no original":  {o.ID, amSender, ErrNotAmendment}, // the original replies to nothing
	} {
		if err := s.Amend(c.id, c.by); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	orphan, _ := s.Send(msg.Message{From: amSender, To: "air/codex", Re: "0123456789", Body: "replies to what this node lacks"})
	if err := s.Amend(orphan.ID, amSender); !errors.Is(err, ErrUnknown) {
		t.Errorf("the original may still be on its way: %v", err)
	}
}

func TestResponseOnlyAmendFieldsAreNeverStored(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "deploy at noon")
	tick()
	m, err := s.Send(msg.Message{From: amSender, To: "air/codex", Body: "hello", Amends: &msg.Amends{Of: o.ID}, AmendedBy: &msg.Amended{By: o.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if m.Amends != nil || m.AmendedBy != nil {
		t.Errorf("what a node says is not something a sender sets: %+v", m)
	}
	rec := msg.Record{Origin: "peer", Seq: 1, Msg: &msg.Message{ID: "cccccccccc", From: amFar, To: "air/codex", Thread: "cccccccccc", Body: "x", At: s.now(), Amends: &msg.Amends{Of: o.ID}, AmendedBy: &msg.Amended{By: o.ID}}}
	if _, err := s.Merge([]msg.Record{rec}); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.log["peer"] {
		if r.Msg.Amends != nil || r.Msg.AmendedBy != nil {
			t.Errorf("stored: %+v", r.Msg)
		}
	}
}

func TestACorrectionIsNotAnEchoOfWhatItCorrects(t *testing.T) {
	// five corrections in a row, each almost the same words as the one before: not a loop
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "status of the deploy to prod: waiting for approval, nothing changed, owner is me")
	cur := o
	for i := 1; i <= 5; i++ {
		tick()
		a, err := s.Send(msg.Message{From: amSender, To: o.To, Re: cur.ID, Body: fmt.Sprintf("status of the deploy to prod: waiting for approval number %d, nothing changed, owner is me", i)})
		if err != nil {
			t.Fatalf("correction %d: %v", i, err)
		}
		if err := s.Amend(a.ID, amSender); err != nil {
			t.Fatal(err)
		}
		cur = a
	}
	if got := s.Inbox(amReadB, false); !slices.Equal(ids(got), []string{cur.ID}) {
		t.Errorf("%v", ids(got))
	}
}

func TestPlainRepliesToOneselfStillMakeALoopAndAreSeenOneMessageLater(t *testing.T) {
	s, tick := amendStore(t)
	body := func(i int) string {
		return fmt.Sprintf("status of the deploy to prod: waiting for approval number %d, nothing changed, owner is me", i)
	}
	cur := put2(t, s, amSender, "air/codex", "", body(0))
	var refused error
	for i := 1; i <= 8 && refused == nil; i++ { // each one replies to the sender's own last, and nobody claims it
		tick()
		next, err := s.Send(msg.Message{From: amSender, To: "air/codex", Re: cur.ID, Body: body(i)})
		if err != nil {
			refused = err
			break
		}
		cur = next
	}
	if !errors.Is(refused, ErrTurnLimit) || !strings.Contains(refused.Error(), "loop") {
		t.Errorf("the loop check still stops an agent talking to itself: %v", refused)
	}
}

func TestACorrectionIsNeitherAReplyNorAReadInTheReceiptsOfTheOriginal(t *testing.T) {
	s, tick := amendStore(t)
	o := put2(t, s, amSender, "air/codex", "", "deploy at noon")
	if err := s.Ack(o.ID, amReadA); err != nil {
		t.Fatal(err)
	}
	tick()
	amendTo(t, s, amOther, o, "deploy at 13:00") // another session of the sender's machine corrects it
	tick()
	if _, err := s.Send(msg.Message{From: amReadB, To: amSender, Re: o.ID, Body: "ok, noted"}); err != nil { // a real reply
		t.Fatal(err)
	}
	rc, ok := s.Receipts(o.ID)
	if !ok || rc.Replies != 1 {
		t.Errorf("one reply, not two: %+v", rc)
	}
	for _, r := range rc.Reads {
		if r.By == amOther {
			t.Errorf("the session that wrote the correction did not read the original: %+v", rc.Reads)
		}
	}
	if len(rc.Reads) != 2 { // amReadA acked it, amReadB answered it
		t.Errorf("%+v", rc.Reads)
	}
}
