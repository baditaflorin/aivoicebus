package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

const offerBoss = "desk/claude.z9"

// offerTask sends a task to `to` from offerBoss.
func offerTask(t *testing.T, s *Store, to string) msg.Message {
	t.Helper()
	m, err := s.Send(msg.Message{From: offerBoss, To: to, Body: "roll the release out", Kind: msg.KindTask})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// offered is the copy of message id in me's inbox read, nil when it is not there.
func offered(s *Store, me, id string) *msg.Message {
	for _, m := range s.InboxFor(me, false, nil) {
		if m.ID == id {
			return &m
		}
	}
	return nil
}

func decline(t *testing.T, s *Store, by string, task msg.Message, why string) msg.Message {
	t.Helper()
	d, err := s.Send(msg.Message{From: by, To: task.From, Re: task.ID, Thread: task.Thread, Kind: msg.KindProgress, Prio: msg.PrioFYI, Body: msg.DeclineBody(why)})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAMachineTaskIsOfferedToOneSessionAtATime(t *testing.T) {
	s, clock := retractStore(t)
	const a, b, c = "lap/claude.a1", "lap/claude.b2", "lap/codex.c3"
	for _, me := range []string{a, b, c} {
		s.InboxFor(me, false, nil) // all three are about
	}
	*clock = clock.Add(time.Minute)
	task := offerTask(t, s, "lap")
	got := offered(s, a, task.ID)
	if got == nil || got.Offer == nil {
		t.Fatalf("%s, the first to read, is not offered the task: %+v", a, got)
	}
	if o := got.Offer; !o.Since.Equal(*clock) || !o.Until.Equal(clock.Add(msg.OfferWindow)) || o.Before != 0 || o.Left != 2 || o.Escalate {
		t.Errorf("offer = %+v, want since now, until in %s, 0 before, 2 left, no escalation", o, msg.OfferWindow)
	}
	for _, me := range []string{b, c} {
		if offered(s, me, task.ID) != nil {
			t.Errorf("%s sees the task while %s holds its offer", me, a)
		}
	}
	if offered(s, a, task.ID) == nil {
		t.Error("the session that holds the offer no longer sees the task")
	}
	*clock = clock.Add(msg.OfferWindow - time.Second)
	if offered(s, b, task.ID) != nil {
		t.Errorf("%s was offered it a second before %s's offer ran out", b, a)
	}
	*clock = clock.Add(time.Second)
	got = offered(s, b, task.ID)
	if got == nil || got.Offer == nil || got.Offer.Before != 1 || got.Offer.Left != 1 {
		t.Fatalf("after %s the next reader is not offered it as the second of three: %+v", msg.OfferWindow, got)
	}
	if offered(s, a, task.ID) != nil {
		t.Errorf("%s is offered it again after its turn", a)
	}
	if offered(s, c, task.ID) != nil {
		t.Errorf("%s sees it while %s holds the offer", c, b)
	}
}

func TestATitledSessionIsOfferedBeforeAnUntitledOne(t *testing.T) {
	s, clock := retractStore(t)
	const titled, hourly = "lap/claude.t1", "lap/claude.h2"
	s.InboxFor(titled, false, nil)
	s.NoteTitle(titled, "fix the release script")
	s.InboxFor(hourly, false, nil)
	task := offerTask(t, s, "lap")
	*clock = clock.Add(msg.OfferActiveWithin)
	if offered(s, hourly, task.ID) != nil {
		t.Fatalf("an untitled run was offered the task while a titled session was seen %s ago", msg.OfferActiveWithin)
	}
	*clock = clock.Add(time.Second)
	if got := offered(s, hourly, task.ID); got == nil || got.Offer == nil {
		t.Fatalf("with the titled session idle for more than %s, the untitled run is not offered it", msg.OfferActiveWithin)
	}
}

func TestAListeningSessionIsOfferedFirst(t *testing.T) {
	s, clock := retractStore(t)
	const titled, worker, waiter = "lap/claude.t1", "lap/worker.w1", "lap/claude.r3"
	s.InboxFor(titled, false, nil)
	s.NoteTitle(titled, "porting the parser")
	s.InboxFor(worker, false, nil)
	s.InboxFor(waiter, false, nil)
	endWorker := s.HoldRead(worker, clock.Add(20*time.Second), &msg.WaitScope{}) // a worker or standby: takes anything
	endWaiter := s.HoldRead(waiter, clock.Add(20*time.Second), &msg.WaitScope{Thread: "abcdef0123"})
	task := offerTask(t, s, "lap")
	if offered(s, titled, task.ID) != nil {
		t.Fatal("a titled session was offered the task while a worker listens")
	}
	if offered(s, waiter, task.ID) != nil {
		t.Fatal("a session waiting for one reply was offered the task as if it listened")
	}
	if got := offered(s, worker, task.ID); got == nil || got.Offer == nil {
		t.Fatal("the listening worker is not offered the task")
	}
	endWorker(false)
	endWaiter(false)
}

func TestADeclineEndsAnOfferAtOnceAndAWithdrawnOneCountsForNothing(t *testing.T) {
	s, clock := retractStore(t)
	const a, b = "lap/claude.a1", "lap/claude.b2"
	s.InboxFor(b, false, nil)
	task := offerTask(t, s, "lap")
	if offered(s, a, task.ID) == nil {
		t.Fatal("not offered")
	}
	*clock = clock.Add(time.Minute)
	d := decline(t, s, a, task, "not my repo")
	if offered(s, a, task.ID) != nil {
		t.Error("the session that declined still sees the task")
	}
	if !msg.IsDecline(d) {
		t.Fatal("the test's decline is no decline")
	}
	if _, err := s.Retract(d.ID, a, false); err != nil {
		t.Fatal(err)
	}
	if offered(s, b, task.ID) != nil {
		t.Fatalf("%s was offered it although %s withdrew its decline within its offer", b, a)
	}
	decline(t, s, a, task, "really not mine")
	if got := offered(s, b, task.ID); got == nil || got.Offer == nil {
		t.Fatalf("after %s declined, %s is not offered the task at once", a, b)
	}
}

// A decline from someone the task never reached, or one that is not a decline, moves nothing.
func TestOnlyTheDeclinersOwnDeclineCounts(t *testing.T) {
	s, _ := retractStore(t)
	const a, b = "lap/claude.a1", "lap/claude.b2"
	s.InboxFor(b, false, nil)
	task := offerTask(t, s, "lap")
	if offered(s, a, task.ID) == nil {
		t.Fatal("not offered")
	}
	decline(t, s, b, task, "on behalf of a1") // b declines for itself only
	s.Send(msg.Message{From: a, To: task.From, Re: task.ID, Kind: msg.KindProgress, Body: "[declined] a typo, not a decline"})
	s.Send(msg.Message{From: a, To: task.From, Re: task.ID, Kind: msg.KindProgress, Body: msg.StateBody(msg.StateStuck, "waiting on CI")})
	if offered(s, a, task.ID) == nil {
		t.Error("someone else's decline, or a progress message that is no decline, ended a1's offer")
	}
}

func TestAReadThroughAnotherNodeStartsAnOffer(t *testing.T) {
	s, clock := retractStore(t)
	const a, b = "lap/claude.a1", "lap/claude.b2"
	s.InboxFor(a, false, nil)
	task := offerTask(t, s, "lap")
	// a was shown the task through another node: its receipt arrives here by replication.
	if _, err := s.Merge([]msg.Record{{Origin: "n2", Seq: 1, Ack: &msg.Ack{ID: task.ID, By: a, At: *clock, Via: msg.ViaHook}}}); err != nil {
		t.Fatal(err)
	}
	if offered(s, b, task.ID) != nil {
		t.Fatalf("%s was offered it while %s, shown it through another node, holds the offer", b, a)
	}
	*clock = clock.Add(msg.OfferWindow)
	if offered(s, b, task.ID) == nil {
		t.Fatal("the offer did not move on after the window")
	}
}

func TestAnAllReadAndAPeekStartNoOffer(t *testing.T) {
	s, _ := retractStore(t)
	const a, b = "lap/claude.a1", "lap/claude.b2"
	task := offerTask(t, s, "lap")
	all := s.InboxFor(b, true, nil)
	if !containsID(all, task.ID) {
		t.Fatal("inbox --all does not list an offered task")
	}
	for _, m := range all {
		if m.Offer != nil {
			t.Errorf("an --all read is told of an offer: %+v", m.Offer)
		}
	}
	if containsID(s.Peek(b), task.ID) {
		t.Error("a peek was given the task")
	}
	if got := offered(s, a, task.ID); got == nil || got.Offer == nil || got.Offer.Before != 0 {
		t.Fatalf("an --all read or a peek counted as an offer: %+v", got)
	}
}

func TestTheSenderThePersonAndAProbeAreNeverOffered(t *testing.T) {
	s, _ := retractStore(t)
	const a = "lap/claude.a1"
	task, _ := s.Send(msg.Message{From: "lap/claude.s0", To: "lap", Body: "someone else please", Kind: msg.KindTask})
	if containsID(s.InboxFor("lap/human", false, nil), task.ID) || containsID(s.Peek("lap/human"), task.ID) {
		t.Error("the person was offered an agent's task")
	}
	if containsID(s.InboxFor("lap/"+msg.ProbeReaderAgent, false, nil), task.ID) {
		t.Error("a join probe was offered the task")
	}
	if got := offered(s, a, task.ID); got == nil || got.Offer == nil || got.Offer.Before != 0 {
		t.Fatalf("the person or a probe took a turn: %+v", got)
	}
	if steps := s.offerStepsLocked(task, s.now()); len(steps) != 1 || steps[0].By != a {
		t.Errorf("offers = %+v, want %s only", steps, a)
	}
}

func TestASelectorTaskIsOfferedAmongTheSessionsItMatches(t *testing.T) {
	s, _ := retractStore(t)
	x := msg.Labels{"repo": "x"}
	const a, b, other = "lap/claude.a1", "desk/codex.b2", "lap/claude.y3"
	s.InboxFor(other, false, msg.Labels{"repo": "y"})
	task := offerTask(t, s, "@repo=x")
	if containsID(s.InboxFor(other, false, msg.Labels{"repo": "y"}), task.ID) {
		t.Fatal("a session the selector does not match was given the task")
	}
	if !containsID(s.InboxFor(a, false, x), task.ID) {
		t.Fatal("the first matching reader is not offered the task")
	}
	if containsID(s.InboxFor(b, false, x), task.ID) {
		t.Error("a second matching session on another machine sees it while the first holds the offer")
	}
}

func TestWhatIsNotATaskToAnAudienceGoesToEveryoneAsBefore(t *testing.T) {
	s, _ := retractStore(t)
	const a, b = "lap/claude.a1", "lap/claude.b2"
	var ids []string
	for _, m := range []msg.Message{
		{To: "lap/claude.a1", Kind: msg.KindTask, Body: "to one session"},
		{To: "lap", Kind: msg.KindQuestion, Body: "anyone?"},
		{To: "lap", Body: "a note"},
		{To: "lap", Kind: msg.KindTask, Body: msg.PersonTag + " swap the disk"},
		{To: "lap/build1", Kind: msg.KindTask, Body: "to a long-lived name"},
	} {
		m.From = offerBoss
		sent, err := s.Send(m)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, sent.ID)
	}
	for _, id := range ids[1:4] {
		for _, me := range []string{a, b} {
			got := offered(s, me, id)
			if got == nil {
				t.Errorf("%s does not get %s as before", me, id)
			} else if got.Offer != nil {
				t.Errorf("%s was told of an offer on a message that is not offered: %+v", me, got.Offer)
			}
		}
	}
	if offered(s, a, ids[0]) == nil {
		t.Error("a task to one session does not reach it")
	}
	if offered(s, "lap/build1", ids[4]) == nil {
		t.Error("a task to a long-lived name does not reach it")
	}
}

func TestEscalationIsDueAfterHalfAnHourUntilThePersonWasTold(t *testing.T) {
	s, clock := retractStore(t)
	const a, b, c = "lap/claude.a1", "lap/claude.b2", "lap/claude.c3"
	task := offerTask(t, s, "lap")
	if got := offered(s, a, task.ID); got == nil || got.Offer.Escalate {
		t.Fatalf("escalation due at once: %+v", got)
	}
	*clock = clock.Add(msg.EscalateAfter - time.Second)
	if got := offered(s, b, task.ID); got == nil || got.Offer.Escalate {
		t.Fatalf("escalation due a second early: %+v", got)
	}
	*clock = clock.Add(msg.OfferWindow + time.Second)
	got := offered(s, c, task.ID)
	if got == nil || !got.Offer.Escalate {
		t.Fatalf("escalation not due after %s: %+v", msg.EscalateAfter, got)
	}
	e, err := s.Send(msg.Message{ID: msg.EscalationID(task.ID), From: c, To: msg.EscalationTo(task), Re: task.ID, Thread: task.Thread, Body: msg.PersonTag + " nobody took it"})
	if err != nil {
		t.Fatal(err)
	}
	if got := offered(s, c, task.ID); got == nil || got.Offer.Escalate {
		t.Fatalf("escalation still due once the person was told: %+v", got)
	}
	r, _ := s.Receipts(task.ID)
	if r.Escalated == nil || r.Escalated.By != c || !r.Escalated.At.Equal(e.At) {
		t.Errorf("receipts escalated = %+v, want by %s", r.Escalated, c)
	}
}

func TestTheMessageToThePersonIsDeliveredUntilTheTaskIsTaken(t *testing.T) {
	s, _ := retractStore(t)
	const a = "lap/claude.a1"
	task := offerTask(t, s, "lap")
	e, _ := s.Send(msg.Message{ID: msg.EscalationID(task.ID), From: a, To: "lap/human", Re: task.ID, Thread: task.Thread, Body: msg.PersonTag + " nobody took it"})
	plain, _ := s.Send(msg.Message{From: a, To: "lap/human", Re: task.ID, Thread: task.Thread, Body: "a plain note about it"})
	if !containsID(s.Peek("lap/human"), e.ID) {
		t.Fatal("the person is not shown the escalation")
	}
	s.Send(msg.Message{From: a, To: offerBoss, Re: task.ID, Kind: msg.KindClaim, Body: "mine"})
	if p := s.Peek("lap/human"); containsID(p, e.ID) || !containsID(p, plain.ID) {
		t.Errorf("after the claim the escalation is still shown, or a plain note is not: %v", ids(p))
	}
	task2 := offerTask(t, s, "lap")
	e2, _ := s.Send(msg.Message{ID: msg.EscalationID(task2.ID), From: a, To: "lap/human", Re: task2.ID, Thread: task2.Thread, Body: msg.PersonTag + " nobody took it"})
	if _, err := s.Retract(task2.ID, offerBoss, false); err != nil {
		t.Fatal(err)
	}
	if containsID(s.Peek("lap/human"), e2.ID) {
		t.Error("the escalation of a withdrawn task is still shown")
	}
}

func TestReceiptsSayWhoWasOfferedTheTaskAndWhatCameOfIt(t *testing.T) {
	s, clock := retractStore(t)
	const a, b, c, d = "lap/claude.a1", "lap/claude.b2", "lap/claude.c3", "lap/claude.d4"
	task := offerTask(t, s, "lap")
	start := *clock
	offered(s, a, task.ID) // passes
	*clock = clock.Add(msg.OfferWindow)
	offered(s, b, task.ID)
	*clock = clock.Add(time.Minute)
	decline(t, s, b, task, "not my repo")
	offered(s, c, task.ID)
	r, _ := s.Receipts(task.ID)
	want := []msg.OfferStep{
		{By: a, At: start, Until: start.Add(msg.OfferWindow), Outcome: msg.OfferPassed},
		{By: b, At: start.Add(msg.OfferWindow), Until: clock.UTC(), Outcome: msg.OfferDeclined, Note: "not my repo"},
		{By: c, At: clock.UTC(), Until: clock.Add(msg.OfferWindow), Outcome: msg.OfferOpen},
	}
	if !sameSteps(r.Offers, want) {
		t.Fatalf("offers =\n%+v\nwant\n%+v", r.Offers, want)
	}
	*clock = clock.Add(2 * time.Minute)
	s.Send(msg.Message{From: d, To: offerBoss, Re: task.ID, Kind: msg.KindClaim, Body: "on it"}) // never offered: a claim by id
	r, _ = s.Receipts(task.ID)
	want[2].Outcome, want[2].Until = msg.OfferPassed, clock.UTC()
	want = append(want, msg.OfferStep{By: d, At: clock.UTC(), Until: clock.UTC(), Outcome: msg.OfferClaimed})
	if !sameSteps(r.Offers, want) {
		t.Fatalf("after the claim offers =\n%+v\nwant\n%+v", r.Offers, want)
	}
	q, _ := s.Send(msg.Message{From: offerBoss, To: "lap", Kind: msg.KindQuestion, Body: "anyone?"})
	if r, _ := s.Receipts(q.ID); r.Offers != nil || r.Escalated != nil {
		t.Errorf("a question has offers: %+v", r)
	}
}

func sameSteps(got, want []msg.OfferStep) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		g, w := got[i], want[i]
		if g.By != w.By || !g.At.Equal(w.At) || !g.Until.Equal(w.Until) || g.Outcome != w.Outcome || g.Note != w.Note {
			return false
		}
	}
	return true
}

func TestOffersStopAtTheCapAndAreForgottenOnceTheTaskIsTaken(t *testing.T) {
	s, _ := retractStore(t)
	task := offerTask(t, s, "lap")
	for i := 0; i < maxOffered; i++ {
		s.offered[task.ID] = append(s.offered[task.ID], offerRec{by: fmt.Sprintf("lap/claude.x%03d", i), at: time.Time{}})
	}
	if offered(s, "lap/claude.late", task.ID) != nil {
		t.Errorf("a task offered to %d sessions was offered to one more", maxOffered)
	}
	s.Send(msg.Message{From: "lap/claude.late", To: offerBoss, Re: task.ID, Kind: msg.KindClaim, Body: "taking it by id"})
	s.mu.Lock()
	s.pruneOffersLocked(s.now())
	_, kept := s.offered[task.ID]
	s.mu.Unlock()
	if kept {
		t.Error("the offers of a taken task are still remembered")
	}
}

func TestAnOfferIsNeverStoredOrReplicated(t *testing.T) {
	s, _ := retractStore(t)
	o := &msg.Offer{Left: 3}
	m, err := s.Send(msg.Message{From: offerBoss, To: "lap/claude.a1", Body: "x", Offer: o})
	if err != nil {
		t.Fatal(err)
	}
	r := msg.Message{ID: "0123456789", From: offerBoss, To: "lap/claude.a1", Thread: "0123456789", Body: "y", At: s.now(), Offer: o}
	if _, err := s.Merge([]msg.Record{{Origin: "n2", Seq: 1, Msg: &r}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{m.ID, r.ID} {
		for _, got := range s.Thread(id) {
			if got.Offer != nil {
				t.Errorf("message %s is held with an offer: %+v", got.ID, got.Offer)
			}
		}
	}
}
