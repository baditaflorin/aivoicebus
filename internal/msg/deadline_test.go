package msg

import (
	"testing"
	"time"
)

func TestJudgeDeadline(t *testing.T) {
	by := time.Date(2026, 10, 22, 15, 40, 0, 0, time.UTC)
	before, after := by.Add(-time.Minute), by.Add(time.Minute)
	two := 2
	read := func(who string, at time.Time, via string) Receipt { return Receipt{By: who, At: at, Via: via} }
	rc := func(replies int, reads ...Receipt) Receipts {
		return Receipts{ID: "aaaaaaaaaa", From: "mac14/claude.s", Reach: &two, Reads: reads, Replies: replies}
	}
	owner := &Receipt{By: "build/worker", At: before}
	cases := []struct {
		name    string
		kind    string
		rs      []Receipts
		now     time.Time
		outcome string
		read    int
		late    int
		replies int
	}{
		{"nobody read it by the deadline", DeadlineReply, []Receipts{rc(0)}, after, OutcomeNobodyRead, 0, 0, 0},
		{"not due and nothing happened", DeadlineReply, []Receipts{rc(0)}, before, OutcomePending, 0, 0, 0},
		{"read in time and nobody replied", DeadlineReply, []Receipts{rc(0, read("a/x", before, "hook"))}, after, OutcomeNoObjection, 1, 0, 0},
		{"read exactly at the deadline is in time", DeadlineReply, []Receipts{rc(0, read("a/x", by, "inbox"))}, after, OutcomeNoObjection, 1, 0, 0},
		{"a receipt with no time counts", DeadlineReply, []Receipts{rc(0, read("a/x", time.Time{}, ""))}, after, OutcomeNoObjection, 1, 0, 0},
		{"a count-only receipt is no read", DeadlineReply, []Receipts{rc(0, read("a/x", before, ViaHookCount))}, after, OutcomeNobodyRead, 0, 0, 0},
		{"read after the deadline is late and does not change it", DeadlineReply, []Receipts{rc(0, read("a/x", after, "hook"))}, after.Add(time.Minute), OutcomeNobodyRead, 0, 1, 0},
		{"a reply settles it before the deadline", DeadlineReply, []Receipts{rc(1, read("a/x", before, ViaReply))}, before, OutcomeObjected, 1, 0, 1},
		{"a reply to a reminder is a reply", DeadlineReply, []Receipts{rc(0), rc(1, read("b/y", after, ViaReply))}, after, OutcomeObjected, 0, 1, 1},
		{"a reminder read after the deadline is a late read", DeadlineReply, []Receipts{rc(0), rc(0, read("b/y", after, "hook"))}, after, OutcomeNobodyRead, 0, 1, 0},
		{"a read of a reminder is late whatever its time says", DeadlineReply, []Receipts{rc(0), rc(0, read("b/y", before, "hook"))}, after, OutcomeNobodyRead, 0, 1, 0},
		{"the same session reading both is one", DeadlineReply, []Receipts{rc(0, read("b/y", before, "inbox")), rc(0, read("b/y", after, "hook"))}, after, OutcomeNoObjection, 1, 0, 0},
		{"no receipts at all is pending", DeadlineReply, nil, after, OutcomePending, 0, 0, 0},
		{"an offer someone took", DeadlineTake, []Receipts{{Owner: owner, Reach: &two}}, before, OutcomeTaken, 0, 0, 0},
		{"an offer nobody took in time", DeadlineTake, []Receipts{rc(0, read("a/x", before, "hook"))}, after, OutcomeNoTaker, 1, 0, 0},
		{"an offer not due yet", DeadlineTake, []Receipts{rc(0)}, before, OutcomePending, 0, 0, 0},
	}
	for _, c := range cases {
		j := JudgeDeadline(c.kind, c.rs, by, c.now)
		if j.Outcome != c.outcome || j.Read != c.read || j.LateReads != c.late || j.Replies != c.replies {
			t.Errorf("%s: got %+v, want outcome %s read %d late %d replies %d", c.name, j, c.outcome, c.read, c.late, c.replies)
		}
		if j.Final() != (c.outcome != OutcomePending) {
			t.Errorf("%s: Final() = %v for %s", c.name, j.Final(), j.Outcome)
		}
	}
	j := JudgeDeadline(DeadlineTake, []Receipts{{Owner: owner}}, by, before)
	if j.TakenBy != "build/worker" {
		t.Errorf("TakenBy = %q", j.TakenBy)
	}
	if j := JudgeDeadline(DeadlineReply, []Receipts{rc(0)}, by, after); j.Of == nil || *j.Of != 2 {
		t.Errorf("Of = %v, want the reach of the first receipt", j.Of)
	}
}
