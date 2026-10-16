package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// ── the shared vectors ───────────────────────────────────────────────────────

// vectorFile is internal/msg/testdata/retract-vectors.json, which the dashboard reads too.
type vectorFile struct {
	Cases []struct {
		Name    string       `json:"name"`
		Records []msg.Record `json:"records"`
		Expect  struct {
			Retracted map[string]struct {
				By, At string
				Leak   bool
			} `json:"retracted"`
			Ineffective []struct{ ID, By string } `json:"ineffective"`
			Reads       []struct{ ID, By string } `json:"reads"`
		} `json:"expect"`
	} `json:"cases"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	b, err := os.ReadFile("../msg/testdata/retract-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f vectorFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func permutations(xs []string) [][]string {
	if len(xs) <= 1 {
		return [][]string{slices.Clone(xs)}
	}
	var out [][]string
	for i := range xs {
		rest := append(slices.Clone(xs[:i]), xs[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]string{xs[i]}, p...))
		}
	}
	return out
}

// orders gives every way the records of different origins can reach a node: whole origins one after
// the other in each possible order (a node pulls a peer until it is drained), and one record at a
// time round the origins. Within an origin the records are always in sequence: a gap is refused.
func orders(recs []msg.Record) map[string][]msg.Record {
	var origins []string
	by := map[string][]msg.Record{}
	for _, r := range recs {
		if _, seen := by[r.Origin]; !seen {
			origins = append(origins, r.Origin)
		}
		by[r.Origin] = append(by[r.Origin], r)
	}
	out := map[string][]msg.Record{}
	for _, p := range permutations(origins) {
		var seq []msg.Record
		for _, o := range p {
			seq = append(seq, by[o]...)
		}
		out["blocks "+strings.Join(p, ",")] = seq
	}
	var rr []msg.Record
	for i := 0; ; i++ {
		any := false
		for _, o := range origins {
			if i < len(by[o]) {
				rr = append(rr, by[o][i])
				any = true
			}
		}
		if !any {
			break
		}
	}
	out["round robin"] = rr
	return out
}

type observed struct {
	Retracted   map[string]string // message id -> "retractor time leak"
	Ineffective [][2]string
	Reads       [][2]string
}

func show(by string, at time.Time, leak bool) string {
	return fmt.Sprintf("%s %s %v", by, at.UTC().Format(time.RFC3339Nano), leak)
}

// observe reads what a store has derived, through what a reader is shown (Thread) and through the
// indexes (the claims that changed nothing, and the receipts).
func observe(s *Store) observed {
	s.mu.Lock()
	ids := make([]string, 0, len(s.msgs))
	for _, m := range s.msgs {
		ids = append(ids, m.ID)
	}
	s.mu.Unlock()
	o := observed{Retracted: map[string]string{}}
	for _, id := range ids {
		for _, m := range s.Thread(id) {
			if m.ID == id && m.Retracted != nil {
				o.Retracted[id] = show(m.Retracted.By, m.Retracted.At, m.Retracted.Leak)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.retr {
		i, held := s.idx[id]
		for by := range c.by {
			if held && !msg.MayRetract(by, s.msgs[i].From) {
				o.Ineffective = append(o.Ineffective, [2]string{id, by})
			}
		}
	}
	for id, byWho := range s.acks {
		for by := range byWho {
			o.Reads = append(o.Reads, [2]string{id, by})
		}
	}
	less := func(a, b [2]string) int { return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1]) }
	slices.SortFunc(o.Ineffective, less)
	slices.SortFunc(o.Reads, less)
	return o
}

// recount is what Stats reported before the totals were kept as records arrived: it looks at every
// claim of every message held.
func recount(s *Store) (retracted, ineffective int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.retr {
		i, held := s.idx[id]
		if !held {
			continue
		}
		effective := false
		for by := range c.by {
			if msg.MayRetract(by, s.msgs[i].From) {
				effective = true
			} else {
				ineffective++
			}
		}
		if effective {
			retracted++
		}
	}
	return retracted, ineffective
}

func checkTotals(t *testing.T, s *Store, when string) {
	t.Helper()
	wantR, wantI := recount(s)
	if st := s.Stats(); st.Retracted != wantR || st.RetractionsIneffective != wantI {
		t.Fatalf("%s: Stats says %d retracted and %d ineffective, a recount of every claim says %d and %d", when, st.Retracted, st.RetractionsIneffective, wantR, wantI)
	}
}

func TestTheSharedVectorsHoldInEveryArrivalOrderAndAfterARestart(t *testing.T) {
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, c := range loadVectors(t).Cases {
		want := observed{Retracted: map[string]string{}}
		for id, r := range c.Expect.Retracted {
			at, err := time.Parse(time.RFC3339, r.At)
			if err != nil {
				t.Fatal(err)
			}
			want.Retracted[id] = show(r.By, at, r.Leak)
		}
		for _, p := range c.Expect.Ineffective {
			want.Ineffective = append(want.Ineffective, [2]string{p.ID, p.By})
		}
		for _, p := range c.Expect.Reads {
			want.Reads = append(want.Reads, [2]string{p.ID, p.By})
		}
		for name, recs := range orders(c.Records) {
			t.Run(c.Name+"/"+name, func(t *testing.T) {
				dir := t.TempDir()
				s, _ := Open(dir, "me")
				s.SetClock(func() time.Time { return clock })
				// one record per call, as a node pulling a peer that is just being written to sees it
				for _, r := range recs {
					if n, err := s.Merge([]msg.Record{r}); err != nil || n != 1 {
						t.Fatalf("merge %s/%d: %d %v", r.Origin, r.Seq, n, err)
					}
					checkTotals(t, s, fmt.Sprintf("after %s/%d", r.Origin, r.Seq))
				}
				check := func(label string, s *Store) {
					t.Helper()
					s.SetClock(func() time.Time { return clock })
					got := observe(s)
					if len(got.Ineffective) == 0 {
						got.Ineffective = nil
					}
					if len(got.Reads) == 0 {
						got.Reads = nil
					}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("%s:\n got %+v\nwant %+v", label, got, want)
					}
					st := s.Stats()
					if st.Retracted != len(want.Retracted) || st.RetractionsIneffective != len(want.Ineffective) {
						t.Errorf("%s: stats %+v", label, st)
					}
					// what a reader is shown follows from it
					inbox := map[string]bool{}
					for _, m := range s.InboxFor("reader/claude.x", false, nil) {
						inbox[m.ID] = true
					}
					all := map[string]msg.Message{}
					for _, m := range s.InboxFor("reader/claude.x", true, nil) {
						all[m.ID] = m
					}
					s.mu.Lock()
					defer s.mu.Unlock()
					for _, m := range s.msgs {
						_, retracted := want.Retracted[m.ID]
						if shown := inbox[m.ID]; shown == (retracted || m.Expired(clock)) {
							t.Errorf("%s: message %s retracted=%v expired=%v shown=%v", label, m.ID, retracted, m.Expired(clock), shown)
						}
						if a := all[m.ID]; retracted && (strings.Contains(a.Body, "SECRET") || a.Retracted == nil) || !retracted && a.Body != m.Body {
							t.Errorf("%s: --all shows %s as %+v", label, m.ID, a)
						}
					}
				}
				check("live", s)
				re, err := Open(dir, "me")
				if err != nil {
					t.Fatal(err)
				}
				check("after a restart", re)
			})
		}
	}
}

// ── what a reader is shown ───────────────────────────────────────────────────

func retractStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(t.TempDir(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	s.SetSendLimit(1 << 20)
	clock := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })
	return s, &clock
}

const sender = "laptop/claude.a1b2"

func TestARetractedMessageIsNotDeliveredButStaysAsATombstoneInAllAndThread(t *testing.T) {
	s, clock := retractStore(t)
	secret := "the deploy key is in /tmp/notes (not really)"
	m, _ := s.Send(msg.Message{From: sender, To: "all", Body: secret, Prio: msg.PrioUrgent, Kind: msg.KindQuestion})
	reply, _ := s.Send(msg.Message{From: "air/codex.q1", To: sender, Body: "on it", Re: m.ID})
	if got := s.InboxFor("air/codex.q1", false, nil); len(got) != 1 || got[0].ID != m.ID {
		t.Fatalf("before: %+v", got)
	}
	*clock = clock.Add(5*time.Minute + 42*time.Second)
	res, err := s.Retract(m.ID, "laptop/claude.c3d4", false)
	if err != nil || res.State != msg.RetractRetracted || res.Sender != sender {
		t.Fatalf("%+v %v", res, err)
	}
	if got := s.InboxFor("air/codex.q1", false, nil); len(got) != 0 {
		t.Errorf("a withdrawn message, urgent or not, is not delivered: %+v", got)
	}
	all := s.InboxFor("air/codex.q1", true, nil)
	if len(all) != 1 {
		t.Fatalf("--all keeps a place for it: %+v", all)
	}
	tomb := all[0]
	if tomb.ID != m.ID || tomb.From != sender || tomb.To != "all" || tomb.Thread != m.Thread || !tomb.At.Equal(m.At) || tomb.Prio != msg.PrioUrgent || tomb.Kind != msg.KindQuestion {
		t.Errorf("a tombstone keeps everything but the words: %+v", tomb)
	}
	if want := "[withdrawn by laptop at 09:05 UTC: text no longer shown]"; tomb.Body != want {
		t.Errorf("%q", tomb.Body)
	}
	if tomb.Retracted == nil || tomb.Retracted.By != "laptop/claude.c3d4" || tomb.Retracted.Leak || !tomb.Retracted.At.Equal(clock.Add(0)) {
		t.Errorf("%+v", tomb.Retracted)
	}
	th := s.Thread(reply.ID)
	if len(th) != 2 || th[0].ID != m.ID || th[0].Body != tomb.Body || th[1].Body != "on it" || th[1].Retracted != nil {
		t.Errorf("the thread keeps its shape, with the message withdrawn in place: %+v", th)
	}
	if strings.Contains(fmt.Sprint(s.InboxFor("air/codex.q1", true, nil), s.Thread(m.ID)), "deploy key") {
		t.Error("the words are not served by any read")
	}
}

func TestRetractingNeverChangesWhatTheLogHoldsOrServesToPeers(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	m, _ := s.Send(msg.Message{From: sender, To: "all", Body: "kept in the log, hidden from readers"})
	before, _ := os.ReadFile(logPath(dir))
	recsBefore := s.Since(map[string]uint64{}, 100)
	if _, err := s.Retract(m.ID, sender, false); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(logPath(dir))
	if !bytes.HasPrefix(after, before) || bytes.Count(after, []byte("\n")) != bytes.Count(before, []byte("\n"))+1 {
		t.Errorf("the log only grows by the retraction's record:\n%s", after)
	}
	recs := s.Since(map[string]uint64{}, 100)
	if !reflect.DeepEqual(recs[0], recsBefore[0]) || recs[0].Msg.Body != "kept in the log, hidden from readers" || recs[0].Msg.Retracted != nil {
		t.Errorf("the replication feed still carries the message untouched: %+v", recs[0].Msg)
	}
	if a := recs[1].Ack; a == nil || a.ID != "retract:"+m.ID || a.By != sender || a.Via != "" || a.Chars != 0 {
		t.Errorf("the retraction is an ordinary ack: %+v", recs[1])
	}
}

func TestARetractionIsNotAReadReceipt(t *testing.T) {
	s, _ := retractStore(t)
	m, _ := s.Send(msg.Message{From: sender, To: "all", Body: "x"})
	s.Ack(m.ID, "air/codex.q1")
	if _, err := s.Retract(m.ID, sender, false); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Acks != 1 || st.Retracted != 1 {
		t.Errorf("one read receipt, one retraction: %+v", st)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, has := s.acks["retract:"+m.ID]; has || len(s.acks) != 1 {
		t.Errorf("acks hold receipts only: %v", s.acks)
	}
}

func TestAnAckThatCarriesAReceiptStillRetractsAndAddsNothingToIt(t *testing.T) {
	s, _ := retractStore(t)
	m, _ := s.Send(msg.Message{From: sender, To: "all", Body: "x"})
	// what a release before 0.7 stores of `aivoicebus ack retract:<id>`, or a hook that wrote a receipt on it
	if err := s.AckWith(msg.Ack{ID: "retract:" + m.ID, By: sender, Via: msg.ViaHook, Chars: 999}); err != nil {
		t.Fatal(err)
	}
	if got := s.InboxFor("air/codex.q1", false, nil); len(got) != 0 {
		t.Errorf("via and chars are not read: %+v", got)
	}
	if st := s.Stats(); st.Acks != 0 || st.Retracted != 1 {
		t.Errorf("%+v", st)
	}
}

// ── questions and answers ────────────────────────────────────────────────────

func TestRetractingAnAnswerOpensTheQuestionAgainAndRetractingTheQuestionHidesIt(t *testing.T) {
	s, _ := retractStore(t)
	q, _ := s.Send(msg.Message{From: "a/claude.1", To: "@repo=x", Body: "who owns the flaky test?", Kind: msg.KindQuestion})
	ans, _ := s.Send(msg.Message{From: "d/claude.4", To: "a/claude.1", Body: "it is mine", Kind: msg.KindAnswer, Re: q.ID})
	l := msg.Labels{"repo": "x"}
	if containsID(s.InboxFor("c/codex.3", false, l), q.ID) {
		t.Fatal("answered: closed to those who have not read it")
	}
	if _, err := s.Retract(ans.ID, "d/claude.5", false); err != nil {
		t.Fatal(err)
	}
	if !containsID(s.InboxFor("c/codex.3", false, l), q.ID) {
		t.Error("the answer was withdrawn, so the question is open again for sessions that have not read it")
	}
	if th := s.Thread(q.ID); len(th) != 2 || th[1].Retracted == nil {
		t.Errorf("the thread shows the answer withdrawn: %+v", th)
	}
	// someone else answers: closed again
	s.Send(msg.Message{From: "e/claude.6", To: "a/claude.1", Body: "actually mine", Kind: msg.KindAnswer, Re: q.ID})
	if containsID(s.InboxFor("c/codex.3", false, l), q.ID) {
		t.Error("a second answer closes it")
	}
	// the question itself
	if _, err := s.Retract(q.ID, "a/claude.9", false); err != nil {
		t.Fatal(err)
	}
	if containsID(s.InboxFor("c/codex.3", false, l), q.ID) {
		t.Error("a retracted question is hidden")
	}
	if th := s.Thread(q.ID); len(th) != 3 || th[0].Retracted == nil || th[1].Retracted == nil || th[2].Retracted != nil {
		t.Errorf("its answers stay in the thread: %+v", th)
	}
}

func TestAnAnswerRetractedByAnotherMachineStillClosesTheQuestion(t *testing.T) {
	s, _ := retractStore(t)
	q, _ := s.Send(msg.Message{From: "a/claude.1", To: "all", Body: "anyone?", Kind: msg.KindQuestion})
	ans, _ := s.Send(msg.Message{From: "d/claude.4", To: "a/claude.1", Body: "me", Kind: msg.KindAnswer, Re: q.ID})
	if _, err := s.Retract(ans.ID, "x/claude.7", false); err == nil {
		t.Fatal("another machine cannot retract it")
	}
	s.AckWith(msg.Ack{ID: "retract:" + ans.ID, By: "x/claude.7"}) // a forged record, as replication could bring it
	if containsID(s.InboxFor("c/codex.3", false, nil), q.ID) {
		t.Error("an inert claim reopens nothing")
	}
}

// ── sending ──────────────────────────────────────────────────────────────────

func TestSendingTheSameWordsAfterUndoingThemStoresANewMessage(t *testing.T) {
	s, clock := retractStore(t)
	first, _ := s.Send(msg.Message{From: sender, To: "all", Body: "deploy is green"})
	again, _ := s.Send(msg.Message{From: sender, To: "all", Body: "deploy is green"})
	if again.ID != first.ID {
		t.Fatal("an identical message within ten minutes is the same one")
	}
	*clock = clock.Add(time.Minute)
	if _, err := s.Retract(first.ID, sender, false); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	redo, err := s.Send(msg.Message{From: sender, To: "all", Body: "deploy is green"})
	if err != nil || redo.ID == first.ID || redo.Retracted != nil {
		t.Fatalf("undo then send again within the window is a new message: %+v %v", redo, err)
	}
	if got := s.InboxFor("air/codex.q1", false, nil); len(got) != 1 || got[0].ID != redo.ID {
		t.Errorf("the new one is delivered, the old one is not: %+v", got)
	}
	if third, _ := s.Send(msg.Message{From: sender, To: "all", Body: "deploy is green"}); third.ID != redo.ID {
		t.Error("and it is deduplicated like any message")
	}
}

func TestResendingARetractedMessageByIDDoesNotReturnItsWordsOrStoreItAgain(t *testing.T) {
	s, _ := retractStore(t)
	id := "00aa11bb22"
	s.Send(msg.Message{ID: id, From: sender, To: "all", Body: "keep this to yourself"})
	s.Retract(id, sender, false)
	got, err := s.Send(msg.Message{ID: id, From: sender, To: "all", Body: "keep this to yourself"})
	if err != nil || got.ID != id || strings.Contains(got.Body, "keep this") || got.Retracted == nil {
		t.Errorf("the idempotent path says the message exists, and no more: %+v %v", got, err)
	}
	if n := s.Stats().Messages; n != 1 {
		t.Errorf("stored again: %d", n)
	}
}

func TestSendingDoesNotAcceptARetractedFieldFromTheSender(t *testing.T) {
	s, _ := retractStore(t)
	out, err := s.Send(msg.Message{From: sender, To: "all", Body: "x", Retracted: &msg.Retraction{By: "x/y", At: time.Now(), Leak: true}})
	if err != nil || out.Retracted != nil {
		t.Fatalf("%+v %v", out, err)
	}
	if got := s.InboxFor("air/codex.q1", false, nil); len(got) != 1 || got[0].Body != "x" || got[0].Retracted != nil {
		t.Errorf("%+v", got)
	}
}

// ── replication and import ───────────────────────────────────────────────────

func TestARetractedFieldOnAReplicatedMessageIsClearedLikeAnOlderNodeWouldClearIt(t *testing.T) {
	forged := &msg.Message{ID: "00aa11bb33", From: "x/y", To: "all", Thread: "00aa11bb33", Body: "visible", At: time.Now().UTC(),
		Retracted: &msg.Retraction{By: "x/y", At: time.Now().UTC(), Leak: true}}
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	if n, err := s.Merge([]msg.Record{{Origin: "p", Seq: 1, Msg: forged}}); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if forged.Retracted == nil {
		t.Error("the caller's record is not modified")
	}
	if got := s.InboxFor("a/b", false, nil); len(got) != 1 || got[0].Body != "visible" || got[0].Retracted != nil {
		t.Errorf("a forged tombstone hides nothing and shows nothing: %+v", got)
	}
	if recs := s.Since(map[string]uint64{}, 10); recs[0].Msg.Retracted != nil {
		t.Error("it is not passed on to other nodes")
	}
	if raw, _ := os.ReadFile(logPath(dir)); strings.Contains(string(raw), `"retracted"`) {
		t.Errorf("or written to the log: %s", raw)
	}
	// a log that already holds one (written by something else) is read the same way
	other := t.TempDir()
	os.WriteFile(logPath(other), rawLine(t, msg.Record{Origin: "p", Seq: 1, Msg: forged}), 0o600)
	re, _ := Open(other, "n1")
	if got := re.InboxFor("a/b", false, nil); len(got) != 1 || got[0].Retracted != nil || got[0].Body != "visible" {
		t.Errorf("%+v", got)
	}
	if recs := re.Since(map[string]uint64{}, 10); recs[0].Msg.Retracted != nil {
		t.Error("and served without it")
	}
}

func TestARetractionWrittenOnOneNodeAppliesOnAPeerWhateverTheClocks(t *testing.T) {
	a, clockA := retractStore(t)
	b, _ := Open(t.TempDir(), "n2")
	clockB := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) // a peer whose clock is years off
	b.SetClock(func() time.Time { return clockB })
	m, _ := a.Send(msg.Message{From: sender, To: "all", Body: "words"})
	*clockA = clockA.Add(7 * time.Minute)
	if _, err := a.Retract(m.ID, "laptop/claude.zz", true); err != nil {
		t.Fatal(err)
	}
	if n, err := b.Merge(a.Since(map[string]uint64{}, 100)); err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	ta, tb := a.Thread(m.ID), b.Thread(m.ID)
	if !reflect.DeepEqual(ta, tb) || ta[0].Retracted == nil || ta[0].Retracted.At.Year() != 2026 || !ta[0].Retracted.Leak {
		t.Errorf("the effective time is the record's, not the reader's clock:\n%+v\n%+v", ta, tb)
	}
	if !strings.Contains(ta[0].Body, "may have held a credential") {
		t.Errorf("%q", ta[0].Body)
	}
	if n, _ := b.Merge(a.Since(map[string]uint64{}, 100)); n != 0 {
		t.Errorf("a duplicate pull applies nothing: %d", n)
	}
	if recs := b.Since(map[string]uint64{}, 100); !reflect.DeepEqual(recs, a.Since(map[string]uint64{}, 100)) {
		t.Error("the two logs hold the same records")
	}
}

func TestTwoNodesThatAcceptedTheSameRetractionAgreeOnceTheyMerge(t *testing.T) {
	a, _ := Open(t.TempDir(), "a")
	b, _ := Open(t.TempDir(), "b")
	m, _ := a.Send(msg.Message{From: sender, To: "all", Body: "words"})
	b.Merge(a.Since(map[string]uint64{}, 100))
	clock := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	a.SetClock(func() time.Time { return clock.Add(time.Minute) })
	b.SetClock(func() time.Time { return clock.Add(2 * time.Minute) })
	if _, err := b.Retract(m.ID, "laptop/claude.b", false); err != nil { // the later one, written first
		t.Fatal(err)
	}
	if _, err := a.Retract(m.ID, "laptop/claude.a", true); err != nil {
		t.Fatal(err)
	}
	ra, rb := a.Since(map[string]uint64{}, 100), b.Since(map[string]uint64{}, 100)
	a.Merge(rb)
	b.Merge(ra)
	ta, tb := a.Thread(m.ID), b.Thread(m.ID)
	if !reflect.DeepEqual(ta, tb) {
		t.Fatalf("\n%+v\n%+v", ta, tb)
	}
	r := ta[0].Retracted
	if r == nil || r.By != "laptop/claude.a" || !r.At.Equal(clock.Add(time.Minute)) || !r.Leak {
		t.Errorf("the earliest claim, with the flag of any: %+v", r)
	}
}

func TestRetractionsSurviveARestartAndCostNothingWhileThereAreNone(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	m, _ := s.Send(msg.Message{From: sender, To: "all", Body: "words"})
	s.Retract(m.ID, sender, false)
	re, _ := Open(dir, "n1")
	if got := re.InboxFor("a/b", false, nil); len(got) != 0 {
		t.Errorf("rebuilt from the log: %+v", got)
	}
	if st := re.Stats(); st.Retracted != 1 || st.Acks != 0 {
		t.Errorf("%+v", st)
	}
	fresh, _ := Open(t.TempDir(), "n1")
	if len(fresh.retr) != 0 {
		t.Error("nothing is kept while nothing is retracted")
	}
}

func TestRepeatedRetractionsDoNotGrowTheIndex(t *testing.T) {
	s, _ := retractStore(t)
	m, _ := s.Send(msg.Message{From: sender, To: "all", Body: "x"})
	for i := 0; i < 200; i++ {
		s.AckWith(msg.Ack{ID: "retract:" + m.ID, By: sender}) // as an old client could keep writing it
	}
	s.mu.Lock()
	n, by := len(s.retr), len(s.retr[m.ID].by)
	s.mu.Unlock()
	if n != 1 || by != 1 {
		t.Errorf("one claim by one identity is one entry: %d %d", n, by)
	}
	if s.Stats().Acks != 0 {
		t.Error("and none of them is a receipt")
	}
}

// ── Retract ──────────────────────────────────────────────────────────────────

func TestRetractSaysWhatCameOfIt(t *testing.T) {
	s, clock := retractStore(t)
	m, _ := s.Send(msg.Message{From: sender, To: "all", Body: "x", TTL: 600})
	s.Ack(m.ID, "air/codex.q1")
	s.Ack(m.ID, "imac/claude.q2")
	s.Ack(m.ID, sender)               // the sender's own receipt is not another session
	s.Ack(m.ID, "laptop/claude.c3d4") // nor is the one retracting
	s.Ack(m.ID, QuarantineBy)         // nor the placeholder
	if got, err := s.RetractDryRun(m.ID, "laptop/claude.c3d4", false); err != nil || got.State != msg.RetractDryRun || got.Would != msg.RetractRetracted || got.ReadBy != 2 || got.Sender != sender || got.Expired {
		t.Fatalf("a dry run says what would happen: %+v %v", got, err)
	}
	if st := s.Stats(); st.Retracted != 0 || s.VV()["n1"] != 6 {
		t.Fatalf("and writes nothing: %+v %v", st, s.VV())
	}

	got, err := s.Retract(m.ID, "laptop/claude.c3d4", false)
	if err != nil || !got.OK || got.State != msg.RetractRetracted || got.ReadBy != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if again, err := s.Retract(m.ID, sender, false); err != nil || again.State != msg.RetractAlready || s.VV()["n1"] != 7 {
		t.Errorf("a second plain retraction, by anyone on the machine, is already: %+v %v %v", again, err, s.VV())
	}
	*clock = clock.Add(time.Minute)
	up, err := s.Retract(m.ID, sender, true)
	if err != nil || up.State != msg.RetractEscalated || s.VV()["n1"] != 8 {
		t.Fatalf("asking for the credential flag on a plain retraction writes a second, stronger record: %+v %v", up, err)
	}
	if r := s.Thread(m.ID)[0].Retracted; r == nil || !r.Leak || r.By != "laptop/claude.c3d4" {
		t.Errorf("the first retractor stays, the flag is added: %+v", r)
	}
	if again, _ := s.Retract(m.ID, sender, true); again.State != msg.RetractAlready {
		t.Errorf("%+v", again)
	}
	if again, _ := s.Retract(m.ID, sender, false); again.State != msg.RetractAlready {
		t.Errorf("a leak retraction already satisfies a plain request: %+v", again)
	}
	if dry, _ := s.RetractDryRun(m.ID, sender, true); dry.State != msg.RetractDryRun || dry.Would != msg.RetractAlready {
		t.Errorf("%+v", dry)
	}
	if s.VV()["n1"] != 8 {
		t.Errorf("nothing more was written: %v", s.VV())
	}
	// an expired message can still be retracted
	*clock = clock.Add(time.Hour)
	m2, _ := s.Send(msg.Message{From: sender, To: "all", Body: "y", TTL: 60})
	*clock = clock.Add(time.Hour)
	if exp, err := s.Retract(m2.ID, sender, false); err != nil || !exp.Expired || exp.State != msg.RetractRetracted {
		t.Errorf("%+v %v", exp, err)
	}
}

func TestRetractRefusesWhatItMustAndWritesNothingForIt(t *testing.T) {
	s, _ := retractStore(t)
	m, _ := s.Send(msg.Message{From: sender, To: "all", Body: "x"})
	before := s.VV()["n1"]

	_, err := s.Retract(m.ID, "imac/claude.a1b2", false)
	var ns *ErrNotSender
	if !errors.As(err, &ns) || ns.Sender != sender || !strings.Contains(err.Error(), "laptop") {
		t.Errorf("another machine: %v", err)
	}
	if _, err := s.Retract("0000000000", sender, false); !errors.Is(err, ErrUnknown) {
		t.Errorf("a message this node does not hold: %v", err)
	}
	if _, err := s.RetractDryRun("0000000000", sender, false); !errors.Is(err, ErrUnknown) {
		t.Errorf("also as a dry run: %v", err)
	}
	for _, bad := range []struct{ id, by string }{
		{"retract:x", sender}, {"", sender}, {"3F2A9C01DE", sender}, {m.ID, ""}, {m.ID, "a\nb"}, {m.ID, QuarantineBy},
	} {
		if _, err := s.Retract(bad.id, bad.by, false); !errors.Is(err, ErrBadRetract) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if s.VV()["n1"] != before || s.Stats().Retracted != 0 {
		t.Errorf("a refusal writes nothing: %v", s.VV())
	}
	if len(s.retr) != 0 {
		t.Error("not even in memory: a node never records a retraction for a message it has not got")
	}
}

func TestOnlyARetractionThatIsWrittenCountsAgainstTheAllowance(t *testing.T) {
	s, clock := retractStore(t)
	s.SetRetractLimit(3)
	var ids []string
	for i := 0; i < 6; i++ {
		m, _ := s.Send(msg.Message{From: sender, To: "all", Body: fmt.Sprint("m", i)})
		ids = append(ids, m.ID)
	}
	// a stream of refusals, repeats and dry runs costs nothing
	for i := 0; i < 50; i++ {
		s.Retract(ids[0], "imac/claude.x", false) // another machine
		s.Retract("0000000000", sender, false)    // unknown
		s.RetractDryRun(ids[1], sender, false)    // dry run
		s.Retract("retract:bad", sender, false)   // malformed
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Retract(ids[i], sender, false); err != nil {
			t.Fatalf("retraction %d: %v", i, err)
		}
		if again, err := s.Retract(ids[i], sender, false); err != nil || again.State != msg.RetractAlready { // repeats are free
			t.Fatalf("%+v %v", again, err)
		}
	}
	_, err := s.Retract(ids[3], sender, false)
	var limited *RateLimitError
	if !errors.As(err, &limited) || limited.Limit != 3 || !strings.Contains(err.Error(), "retractions") || strings.Contains(err.Error(), "messages") {
		t.Fatalf("the fourth is over: %v", err)
	}
	if got := s.Stats().Retracted; got != 3 {
		t.Errorf("the refused one is not written: %d", got)
	}
	// the allowance is the identity's own
	if _, err := s.Retract(ids[3], "laptop/claude.other", false); err != nil {
		t.Errorf("another identity has its own: %v", err)
	}
	*clock = clock.Add(61 * time.Minute)
	if _, err := s.Retract(ids[4], sender, false); err != nil {
		t.Errorf("the window slides: %v", err)
	}
}

func TestTheSendBudgetAndTheRetractionBudgetAreSeparate(t *testing.T) {
	s, _ := retractStore(t)
	s.SetSendLimit(2)
	a, _ := s.Send(msg.Message{From: sender, To: "all", Body: "1"})
	s.Send(msg.Message{From: sender, To: "all", Body: "2"})
	if _, err := s.Retract(a.ID, sender, false); err != nil {
		t.Errorf("a sender who has used up its sends can still take one back: %v", err)
	}
	if _, err := s.Send(msg.Message{From: sender, To: "all", Body: "3"}); err == nil {
		t.Error("undo does not give the send back")
	}
}

// ── the index agrees with looking at everything ──────────────────────────────

// naiveRetracted is the rule applied to every record in the log, with no index.
func naiveRetracted(s *Store, id, from string) (retraction, bool) {
	var best retraction
	found := false
	for _, recs := range s.log {
		for _, r := range recs {
			if r.Ack == nil {
				continue
			}
			mid, _, ok := msg.RetractedID(r.Ack.ID)
			if !ok || mid != id || !msg.MayRetract(r.Ack.By, from) {
				continue
			}
			_, leak, _ := msg.RetractedID(r.Ack.ID)
			if !found {
				best, found = retraction{by: r.Ack.By, at: r.Ack.At, leak: leak}, true
				continue
			}
			best.leak = best.leak || leak
			if r.Ack.At.Before(best.at) || (r.Ack.At.Equal(best.at) && r.Ack.By < best.by) {
				best.by, best.at = r.Ack.By, r.Ack.At
			}
		}
	}
	return best, found
}

func naiveClosed(s *Store, m msg.Message) bool {
	if m.Kind != msg.KindQuestion || !msg.IsBulletin(m.To) {
		return false
	}
	for _, a := range s.msgs {
		if a.Kind == msg.KindAnswer && a.Re == m.ID && a.From != m.From {
			if _, gone := naiveRetracted(s, a.ID, a.From); !gone {
				return true
			}
		}
	}
	return false
}

func TestReadsWithRetractionsGiveWhatALookAtEveryRecordGives(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	u := newUniverse()
	s, _ := Open(t.TempDir(), "n1")
	s.SetSendLimit(1 << 20)
	s.SetRetractLimit(1 << 20)
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })

	var ids []string
	from := map[string]string{}
	for i := 0; i < 500; i++ {
		clock = clock.Add(time.Minute)
		m := msg.Message{From: u.identity(r), To: u.target(r), Body: fmt.Sprintf("message %d", i), Prio: pick(r, []string{"", "fyi", "ask", "urgent"})}
		switch {
		case len(ids) > 0 && r.Intn(4) == 0:
			m.Kind, m.Re = msg.KindAnswer, pick(r, ids)
		case r.Intn(5) == 0:
			m.Kind = msg.KindQuestion
		}
		out, err := s.Send(m)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, out.ID)
		from[out.ID] = out.From
		if r.Intn(3) == 0 { // reads in the middle keep the derived answer worked out, so later claims must fold into it
			id := pick(r, ids)
			s.mu.Lock()
			s.retractionLocked(id, from[id])
			s.mu.Unlock()
		}
		switch r.Intn(6) {
		case 0, 2:
			s.AckWith(msg.Ack{ID: pick(r, ids), By: u.identity(r), Via: pick(r, []string{"", msg.ViaHook, msg.ViaHookCount, msg.ViaHookCount, msg.ViaInbox})})
		case 1: // a retraction by whoever: sometimes the sender's machine, often not (the node refuses those, so write it as a replicated record would arrive)
			id := pick(r, ids)
			by := u.identity(r)
			if r.Intn(2) == 0 {
				by = msg.HostOf(from[id]) + "/claude." + fmt.Sprint(r.Intn(3))
			}
			ack := msg.Ack{ID: msg.RetractPrefix + id, By: by, At: clock.Add(-time.Duration(r.Intn(120)) * time.Second)} // not ahead of the node: a notice is owed from a claim's time, clamped to when it arrived
			if r.Intn(4) == 0 {
				ack.ID = msg.RetractLeakPrefix + id
			}
			s.mu.Lock()
			s.commitLocked(msg.Record{Ack: &ack})
			s.mu.Unlock()
		}
	}
	// claims for messages not held, and claims that name nothing
	for i := 0; i < 20; i++ {
		s.mu.Lock()
		s.commitLocked(msg.Record{Ack: &msg.Ack{ID: msg.RetractPrefix + msg.NewID(), By: u.identity(r), At: clock}})
		s.mu.Unlock()
	}

	s.mu.Lock()
	effective := 0
	for _, m := range s.msgs {
		got, gotOK := s.retractionLocked(m.ID, m.From)
		want, wantOK := naiveRetracted(s, m.ID, m.From)
		if gotOK != wantOK || got != want {
			s.mu.Unlock()
			t.Fatalf("message %s: index %+v %v, every record %+v %v", m.ID, got, gotOK, want, wantOK)
		}
		if gotOK {
			effective++
		}
	}
	s.mu.Unlock()
	if effective < 20 {
		t.Fatalf("the test retracts too little to mean anything: %d", effective)
	}
	if got := s.Stats().Retracted; got != effective {
		t.Errorf("stats count %d, a look at every message %d", got, effective)
	}

	noticed := 0
	defer func() {
		t.Logf("%d notices shown", noticed)
		if noticed < 20 {
			t.Errorf("the test shows too few notices to mean anything: %d", noticed)
		}
	}()
	var ackers []string // who to ask for most of the time: the sessions that read something, so notices are owed
	for _, recs := range s.log {
		for _, rec := range recs {
			if rec.Ack != nil && !msg.HasRetractPrefix(rec.Ack.ID) {
				ackers = append(ackers, rec.Ack.By)
			}
		}
	}
	for q := 0; q < 300; q++ {
		me, labels, all := u.identity(r), u.labels(r), r.Intn(3) == 0
		if r.Intn(3) > 0 {
			me = pick(r, ackers)
		}
		clock = clock.Add(time.Duration(r.Intn(60)) * time.Minute)
		got := s.InboxFor(me, all, labels)
		s.mu.Lock()
		var want []msg.Message
		for _, m := range s.msgs {
			if m.From == me || !msg.MatchesLabeled(m.To, me, labels) {
				continue
			}
			ret, retracted := naiveRetracted(s, m.ID, m.From)
			if !all && (retracted || m.Expired(clock) || s.acks[m.ID][me] || naiveClosed(s, m)) {
				continue
			}
			if retracted {
				m = tombstone(m, ret)
			}
			want = append(want, m)
		}
		if !all {
			want = append(naiveNotices(s, me, clock), want...)
		}
		s.mu.Unlock()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("query %d for %s all=%v: indexed gives %d messages, a scan of every record %d\nindexed: %v\nscan:    %v", q, me, all, len(got), len(want), msgIDs(got), msgIDs(want))
		}
		for _, m := range got { // the reader acks some of the notices it is shown
			if m.From == msg.NoticeFrom {
				noticed++
				if r.Intn(2) == 0 {
					s.AckWith(msg.Ack{ID: m.ID, By: me, Via: msg.ViaHook})
				}
			}
		}
	}
	for _, id := range ids[:80] {
		s.mu.Lock()
		var want []msg.Message
		for _, m := range naiveThread(s, id) {
			if ret, ok := naiveRetracted(s, m.ID, m.From); ok {
				m = tombstone(m, ret)
			}
			want = append(want, m)
		}
		s.mu.Unlock()
		if got := s.Thread(id); !reflect.DeepEqual(got, want) {
			t.Fatalf("thread of %s differs", id)
		}
	}
}

// Reading mail must not depend on how many retractions the log holds, only on how many of the
// messages it returns have one.
func TestReadingCostsNothingExtraForMessagesWithoutRetractions(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	s.SetSendLimit(1 << 20)
	var first string
	for i := 0; i < 2000; i++ {
		m, _ := s.Send(msg.Message{From: "laptop/claude.a", To: "all", Body: fmt.Sprint("m", i), Prio: msg.PrioFYI})
		if i == 0 {
			first = m.ID
		}
	}
	s.Retract(first, "laptop/claude.a", false)
	// the lookup for a message with no claim looks at nothing
	s.mu.Lock()
	defer s.mu.Unlock()
	allocs := testing.AllocsPerRun(1000, func() { s.retractionLocked(s.msgs[1500].ID, s.msgs[1500].From) })
	if allocs != 0 {
		t.Errorf("%v allocations to find out a message was not retracted", allocs)
	}
}

func BenchmarkInboxForWith2000Retractions100k(b *testing.B) {
	s := benchStore(b)
	s.mu.Lock()
	for i := 0; i < 2000; i++ {
		m := s.msgs[i*50]
		s.addRetractionLocked(m.ID, msg.HostOf(m.From)+"/claude", time.Now(), false, "")
	}
	s.mu.Unlock()
	l := msg.Labels{"repo": "alpha"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.InboxFor("reader/claude.a1b2", false, l)
	}
}

// A read works the answer out and keeps it; claims that arrive after that are folded into it. Each
// step is held to a fresh look at every record.
func TestAClaimThatArrivesAfterTheAnswerWasWorkedOutIsFoldedIn(t *testing.T) {
	s, clock := retractStore(t)
	m, _ := s.Send(msg.Message{From: sender, To: "all", Body: "x"})
	t0 := *clock
	check := func(step string) {
		t.Helper()
		s.mu.Lock()
		defer s.mu.Unlock()
		got, gotOK := s.retractionLocked(m.ID, m.From)
		want, wantOK := naiveRetracted(s, m.ID, m.From)
		if gotOK != wantOK || got != want {
			t.Fatalf("%s: kept %+v %v, every record gives %+v %v", step, got, gotOK, want, wantOK)
		}
	}
	claim := func(id, by string, at time.Time) {
		t.Helper()
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.commitLocked(msg.Record{Ack: &msg.Ack{ID: id, By: by, At: at}}); err != nil {
			t.Fatal(err)
		}
	}
	check("nothing claimed")
	claim("retract:"+m.ID, "imac/claude.x", t0)
	check("a claim by another machine")
	claim("retract:"+m.ID, "laptop/claude.b", t0.Add(5*time.Minute))
	check("the first authorised claim")
	claim("retract:"+m.ID, "laptop/claude.c", t0.Add(9*time.Minute))
	check("a later one")
	claim("retract:"+m.ID, "laptop/claude.a", t0.Add(time.Minute))
	check("an earlier one")
	claim("retract:"+m.ID, "laptop/claude.0", t0.Add(time.Minute))
	check("the same instant, a lower identity")
	claim("retract-leak:"+m.ID, "laptop/claude.z", t0.Add(30*time.Minute))
	check("a leak, late")
	claim("retract:"+m.ID, "laptop/claude.z", t0.Add(-time.Minute))
	check("the same identity again, earlier")
	if r := s.Thread(m.ID)[0].Retracted; r == nil || r.By != "laptop/claude.z" || !r.At.Equal(t0.Add(-time.Minute)) || !r.Leak {
		t.Errorf("%+v", r)
	}
}

// ── the totals Stats reports are kept as records arrive ──────────────────────

// randomOrder interleaves the origins' records at random, each origin still in sequence.
func randomOrder(r *rand.Rand, recs []msg.Record) []msg.Record {
	by := map[string][]msg.Record{}
	var origins []string
	for _, rec := range recs {
		if _, seen := by[rec.Origin]; !seen {
			origins = append(origins, rec.Origin)
		}
		by[rec.Origin] = append(by[rec.Origin], rec)
	}
	var out []msg.Record
	for len(origins) > 0 {
		i := r.Intn(len(origins))
		o := origins[i]
		out = append(out, by[o][0])
		by[o] = by[o][1:]
		if len(by[o]) == 0 {
			origins = slices.Delete(origins, i, i+1)
		}
	}
	return out
}

// retractionHistory is records from four origins: messages from four machines, claims on them by
// the sender's own sessions, by other machines, by the same identity twice, for messages that never
// come, the same claim written on two origins, and the same message written on two.
func retractionHistory(r *rand.Rand, messages, claimCount int) []msg.Record {
	hosts := []string{"laptop", "Imac", "air", "node-a"}
	identity := func() string {
		return pick(r, hosts) + "/" + pick(r, []string{"claude", "codex"}) + "." + pick(r, []string{"a1", "b2", "c3", ""})
	}
	origins := []string{"n1", "n2", "n3", "n4"}
	seq := map[string]uint64{}
	var recs []msg.Record
	add := func(origin string, rec msg.Record) {
		seq[origin]++
		rec.Origin, rec.Seq = origin, seq[origin]
		recs = append(recs, rec)
	}
	var ids, from []string
	base := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	for i := 0; i < messages; i++ {
		id := fmt.Sprintf("%010x", r.Int63n(1<<40))
		f := identity()
		ids, from = append(ids, id), append(from, f)
		m := &msg.Message{ID: id, From: f, To: "all", Thread: id, Body: "x", At: base}
		add(pick(r, origins), msg.Record{Msg: m})
		if r.Intn(8) == 0 { // written on a second node too
			dup := *m
			add(pick(r, origins), msg.Record{Msg: &dup})
		}
	}
	for i := 0; i < claimCount; i++ {
		var id string
		switch k := r.Intn(10); {
		case k == 0:
			id = fmt.Sprintf("%010x", r.Int63n(1<<40)) // a message nobody holds
		default:
			j := r.Intn(len(ids))
			id = ids[j]
			by := identity()
			if k < 6 { // mostly the sender's own machine
				by = msg.HostOf(from[j]) + "/claude." + pick(r, []string{"a1", "b2", "z9"})
			}
			prefix := msg.RetractPrefix
			if r.Intn(4) == 0 {
				prefix = msg.RetractLeakPrefix
			}
			a := &msg.Ack{ID: prefix + id, By: by, At: base.Add(time.Duration(r.Intn(600)) * time.Second)}
			add(pick(r, origins), msg.Record{Ack: a})
			if r.Intn(6) == 0 { // the same claim written on another node
				b := *a
				add(pick(r, origins), msg.Record{Ack: &b})
			}
			continue
		}
		a := &msg.Ack{ID: msg.RetractPrefix + id, By: identity(), At: base}
		add(pick(r, origins), msg.Record{Ack: a})
	}
	return recs
}

func TestTheRetractionTotalsEqualARecountWhateverOrderTheRecordsArriveIn(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		r := rand.New(rand.NewSource(seed))
		recs := retractionHistory(r, 40, 120)
		var finalR, finalI int
		for k := 0; k < 25; k++ {
			dir := t.TempDir()
			s, _ := Open(dir, "me")
			for i, rec := range randomOrder(r, recs) {
				if _, err := s.Merge([]msg.Record{rec}); err != nil {
					t.Fatal(err)
				}
				if i%7 == 0 {
					checkTotals(t, s, fmt.Sprintf("seed %d order %d record %d", seed, k, i))
				}
			}
			checkTotals(t, s, fmt.Sprintf("seed %d order %d end", seed, k))
			st := s.Stats()
			if k == 0 {
				finalR, finalI = st.Retracted, st.RetractionsIneffective
				if finalR < 10 || finalI < 10 {
					t.Fatalf("the history should hold both kinds: %d %d", finalR, finalI)
				}
			} else if st.Retracted != finalR || st.RetractionsIneffective != finalI {
				t.Fatalf("seed %d: the totals depend on the order: %d/%d then %d/%d", seed, finalR, finalI, st.Retracted, st.RetractionsIneffective)
			}
			if k%5 == 0 { // and a restart replays them into the same totals
				re, err := Open(dir, "me")
				if err != nil {
					t.Fatal(err)
				}
				if got := re.Stats(); got.Retracted != finalR || got.RetractionsIneffective != finalI {
					t.Fatalf("after a restart: %+v", got)
				}
				checkTotals(t, re, "after a restart")
			}
		}
	}
}

// Every health read and every metrics scrape asks for Stats, under the store's lock, and a node asks
// each peer's health on every pull: it must not cost more because a peer sent a million claims.
func bigClaimStore(t testing.TB, messages, claimsPerMessage int) *Store {
	s, err := Open(t.TempDir(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := uint64(0)
	apply := func(rec msg.Record) {
		seq++
		rec.Origin, rec.Seq = "peer", seq
		s.applyLocked(rec)
	}
	for i := 0; i < messages; i++ {
		id := fmt.Sprintf("%010x", i)
		apply(msg.Record{Msg: &msg.Message{ID: id, From: "laptop/claude.a1", To: "all", Thread: id, Body: "x", At: base}})
		for c := 0; c < claimsPerMessage; c++ {
			host := "laptop" // half of them from the sender's machine, half from others
			if c%2 == 1 {
				host = "imac"
			}
			apply(msg.Record{Ack: &msg.Ack{ID: msg.RetractPrefix + id, By: fmt.Sprintf("%s/claude.s%d", host, c), At: base}})
		}
	}
	return s
}

func TestStatsStaysCheapWithAHundredThousandClaims(t *testing.T) {
	s := bigClaimStore(t, 50000, 2) // 100,000 claims
	if st := s.Stats(); st.Retracted != 50000 || st.RetractionsIneffective != 50000 {
		t.Fatalf("%+v", st)
	}
	checkTotals(t, s, "100,000 claims")
	const calls = 2000
	start := time.Now()
	for i := 0; i < calls; i++ {
		s.Stats()
	}
	// counting every claim took 60 ms at this size; the totals are kept, so a call is a few lock
	// operations. The bound is generous (a loaded machine, the race detector): 1000 times the
	// expected cost and a sixtieth of the old one.
	if per := time.Since(start) / calls; per > time.Millisecond {
		t.Errorf("Stats takes %v with 100,000 claims held", per)
	}
}

func BenchmarkStats100kClaims(b *testing.B) {
	s := bigClaimStore(b, 50000, 2)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Stats()
	}
}
