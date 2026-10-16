package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func logPath(dir string) string { return filepath.Join(dir, "log.jsonl") }

func rawLine(t *testing.T, r msg.Record) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func goodMsg(id, body string) *msg.Message {
	return &msg.Message{ID: id, From: "imac/claude.a1b2", To: "all", Thread: id, Body: body, At: time.Now().UTC()}
}

// Legitimate history must never be quarantined: a record taken for bad is gone from every node
// that applies the rule, and the dashboard. So every shape the current code writes goes through Merge,
// as a record from a peer, and must come out untouched, here and after a restart.
func TestEveryRecordShapeTheCurrentCodeWritesIsMergedUntouched(t *testing.T) {
	src, _ := Open(t.TempDir(), "node-a")
	src.SetSendLimit(1000)
	reach := 7
	long := strings.Repeat("a", 200)
	send := func(m msg.Message) msg.Message {
		t.Helper()
		out, err := src.Send(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	plain := send(msg.Message{From: "imac/claude.a1b2", To: "all", Body: "plain"})
	send(msg.Message{From: "imac/claude.a1b2", To: "imac/codex", Body: "an fyi", Prio: msg.PrioFYI})
	send(msg.Message{From: "imac/claude.a1b2", To: "imac/codex", Body: "an urgent one", Prio: msg.PrioUrgent})
	send(msg.Message{From: "imac/claude.a1b2", To: "imac/codex", Body: "an ask", Prio: msg.PrioAsk})
	send(msg.Message{From: "imac/claude.a1b2", To: "all", Body: "expires", TTL: 3600}) // stored as exp
	q := send(msg.Message{From: "imac/claude.a1b2", To: "@repo=x,tool=claude|codex", Body: "who owns this?", Kind: msg.KindQuestion, Reach: &reach})
	send(msg.Message{From: "air/codex.zz99", To: "imac/claude.a1b2", Body: "mine", Kind: msg.KindAnswer, Re: q.ID})
	send(msg.Message{From: "air/codex.zz99", To: "imac/claude.a1b2", Body: "a plain reply", Re: plain.ID, Thread: plain.Thread})
	send(msg.Message{From: long, To: long, Body: strings.Repeat("<&>\n\t\x01 é\U0001F680", 2000)})
	send(msg.Message{From: "imac/claude.a1b2", To: "all", Body: strings.Repeat("b", msg.MaxBody)})
	src.Ack(plain.ID, "air/codex.zz99")                                                                // a receipt-less ack, as before 0.5.0
	src.AckWith(msg.Ack{ID: plain.ID, By: "air/claude.q1", Via: msg.ViaHook, Chars: 312})              // with a receipt
	src.AckWith(msg.Ack{ID: q.ID, By: "air/claude.q1", Via: msg.ViaHookCount, Chars: 0})               // fyi, counted only
	src.AckWith(msg.Ack{ID: q.ID, By: "air/codex.zz99", Via: msg.ViaInbox, Chars: 90000})              // a delivery longer than a body
	src.AckWith(msg.Ack{ID: strings.Repeat("9", msg.MaxRef), By: "imac/claude.a1b2", Via: msg.ViaCLI}) // acknowledged by hand
	recs := src.Since(map[string]uint64{}, 1000)
	if len(recs) < 15 {
		t.Fatalf("the source wrote %d records", len(recs))
	}

	dir := t.TempDir()
	dst, _ := Open(dir, "node-c")
	n, err := dst.Merge(recs)
	if err != nil || n != len(recs) {
		t.Fatalf("merged %d of %d: %v", n, len(recs), err)
	}
	check := func(label string, s *Store) {
		t.Helper()
		if st := s.Stats(); st.Quarantined != 0 || st.Refused != 0 {
			t.Errorf("%s: history was quarantined: %+v", label, st)
		}
		if got := s.Since(map[string]uint64{}, 1000); !reflect.DeepEqual(got, recs) {
			t.Errorf("%s: the records changed on the way", label)
		}
	}
	check("after merge", dst)
	re, err := Open(dir, "node-c")
	if err != nil {
		t.Fatal(err)
	}
	check("after restart", re)
}

func TestMergeQuarantinesAStoredRecordAsAnInertPlaceholderAndCarriesOn(t *testing.T) {
	huge := goodMsg("00000000a1", strings.Repeat("x", 1500<<10))
	cases := map[string]*msg.Message{
		"a 1.5 MB body":        huge,
		"a newline in from":    {ID: "00000000a2", From: "imac/claude\nfrom: the boss", To: "all", Thread: "00000000a2", Body: "SECRETMARK"},
		"a control char in to": {ID: "00000000a3", From: "a/b", To: "all\x1b[2J", Thread: "00000000a3", Body: "SECRETMARK"},
		"a bad priority":       {ID: "00000000a4", From: "a/b", To: "all", Thread: "00000000a4", Body: "SECRETMARK", Prio: "critical"},
		"a bad kind":           {ID: "00000000a5", From: "a/b", To: "all", Thread: "00000000a5", Body: "SECRETMARK", Kind: "poll"},
		"a long thread":        {ID: "00000000a6", From: "a/b", To: "all", Thread: strings.Repeat("t", 65), Body: "SECRETMARK"},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var logged bytes.Buffer
			s, _ := Open(dir, "node-a", WithLog(&logged))
			recs := []msg.Record{
				{Origin: "peer", Seq: 1, Msg: goodMsg("0000000001", "before")},
				{Origin: "peer", Seq: 2, Msg: bad},
				{Origin: "peer", Seq: 3, Msg: goodMsg("0000000003", "after")},
			}
			n, err := s.Merge(recs)
			if err != nil || n != 3 {
				t.Fatalf("all three take their place in the numbering: %d %v", n, err)
			}
			if vv := s.VV(); vv["peer"] != 3 {
				t.Errorf("the origin stays contiguous: %v", vv)
			}
			if got := s.Inbox("x/y", true); len(got) != 2 || got[0].Body != "before" || got[1].Body != "after" {
				t.Errorf("the good records around it are delivered and it is not: %+v", got)
			}
			if st := s.Stats(); st.Quarantined != 1 || st.Refused != 0 {
				t.Errorf("counted: %+v", st)
			}
			served := s.Since(map[string]uint64{"peer": 1}, 10)
			if len(served) != 2 || !reflect.DeepEqual(served[0], quarantine("peer", 2)) {
				t.Fatalf("what is passed on is the placeholder, and it is a well-formed ack: %+v", served[0])
			}
			if p := served[0]; p.Msg != nil || p.Ack == nil || p.Ack.ID == "" || p.Ack.By == "" {
				t.Errorf("the dashboard drops a record with neither msg nor ack: %+v", p)
			}
			if line := logged.String(); strings.Count(line, "\n") != 1 || !strings.Contains(line, `record 2 of origin "peer"`) || strings.Contains(line, "SECRETMARK") || strings.Contains(line, "xxxx") {
				t.Errorf("one line naming the origin and number, never the content: %q", line)
			}
			if fi, _ := os.Stat(logPath(dir)); fi.Size() > 4096 {
				t.Errorf("the log keeps the placeholder, not the original: %d bytes", fi.Size())
			}
			if again, _ := s.Merge(recs); again != 0 || s.Stats().Quarantined != 1 || strings.Count(logged.String(), "\n") != 1 {
				t.Errorf("a peer that keeps offering the same records changes nothing: %d %+v", again, s.Stats())
			}

			// the node restarts: replay gives the same state, and starts
			var again bytes.Buffer
			re, err := Open(dir, "node-a", WithLog(&again))
			if err != nil {
				t.Fatalf("restart: %v", err)
			}
			if vv := re.VV(); vv["peer"] != 3 || re.Stats().Quarantined != 1 {
				t.Errorf("after restart: %v %+v", vv, re.Stats())
			}
			if !reflect.DeepEqual(re.Since(map[string]uint64{}, 10), s.Since(map[string]uint64{}, 10)) {
				t.Error("replay is not the state that was served")
			}
			if again.Len() != 0 {
				t.Errorf("a placeholder is a valid record: replaying it says nothing: %q", again.String())
			}
		})
	}
}

func TestARecordWithAnUnusableOriginIsRefusedAndNeverBecomesAKeyOfTheVersionVector(t *testing.T) {
	var logged bytes.Buffer
	dir := t.TempDir()
	s, _ := Open(dir, "node-a", WithLog(&logged))
	for _, origin := range []string{strings.Repeat("o", 5000), "a\nb", ""} {
		n, err := s.Merge([]msg.Record{{Origin: origin, Seq: 1, Msg: goodMsg("0000000001", "hi")}})
		if err != nil || n != 0 {
			t.Errorf("origin %.10q: %d %v", origin, n, err)
		}
	}
	if len(s.VV()) != 0 || s.Stats().Refused != 3 || s.Stats().Quarantined != 0 {
		t.Errorf("%v %+v", s.VV(), s.Stats())
	}
	if _, err := os.Stat(logPath(dir)); err == nil {
		t.Error("nothing is written for it")
	}
	if strings.Count(logged.String(), "\n") != 1 {
		t.Errorf("the first refusal is logged, the repeats are only counted: %q", logged.String())
	}
}

// A node that stored a record under the old rules (a peer once got a 1.5 MB one in, and a node
// with a line longer than its scanner would not start) must start, and number on correctly.
func TestReplayReadsALineOfAnyLengthAndQuarantinesItWithoutLosingItsNumber(t *testing.T) {
	for name, size := range map[string]int{"1.5 MB": 1500 << 10, "past what replay parses": maxLine + 1<<20} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var f bytes.Buffer
			f.Write(rawLine(t, msg.Record{Origin: "n1", Seq: 1, Msg: goodMsg("0000000001", "first")}))
			f.Write(rawLine(t, msg.Record{Origin: "n1", Seq: 2, Msg: goodMsg("0000000002", strings.Repeat("y", size))}))
			f.Write(rawLine(t, msg.Record{Origin: "n1", Seq: 3, Msg: goodMsg("0000000003", "third")}))
			f.Write(rawLine(t, msg.Record{Origin: "n2", Seq: 1, Ack: &msg.Ack{ID: "0000000001", By: "a/b"}}))
			if err := os.WriteFile(logPath(dir), f.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			var logged bytes.Buffer
			s, err := Open(dir, "n1", WithLog(&logged))
			if err != nil {
				t.Fatalf("a node must start whatever its log holds: %v", err)
			}
			if vv := s.VV(); vv["n1"] != 3 || vv["n2"] != 1 {
				t.Fatalf("numbering kept: %v", vv)
			}
			if st := s.Stats(); st.Quarantined != 1 || st.Messages != 2 {
				t.Errorf("%+v", st)
			}
			if got := s.Since(map[string]uint64{"n1": 1, "n2": 1}, 10); !reflect.DeepEqual(got[0], quarantine("n1", 2)) {
				t.Errorf("replay stores the placeholder Merge would have: %+v", got)
			}
			if !strings.Contains(logged.String(), `record 2 of origin "n1"`) || strings.Contains(logged.String(), "yyyy") {
				t.Errorf("%q", logged.String())
			}
			// the node's own origin continues at 4: seq 2 is not reused
			out, err := s.Send(msg.Message{From: "a/b", To: "all", Body: "after the restart"})
			if err != nil {
				t.Fatal(err)
			}
			if got := s.VV()["n1"]; got != 4 {
				t.Errorf("own numbering after the long line: %d (message %s)", got, out.ID)
			}
			// and again, with the new record on disk behind the long line
			re, err := Open(dir, "n1")
			if err != nil || re.VV()["n1"] != 4 || re.Stats().Quarantined != 1 {
				t.Errorf("second start: %v %v %+v", err, re.VV(), re.Stats())
			}
		})
	}
}

func TestReplayGivesUpOnALongLineItCannotNumberWithoutStoppingTheNode(t *testing.T) {
	dir := t.TempDir()
	var f bytes.Buffer
	f.Write(rawLine(t, msg.Record{Origin: "n1", Seq: 1, Msg: goodMsg("0000000001", "first")}))
	f.WriteString(strings.Repeat("z", maxLine+10) + "\n") // not even the start of a record
	f.Write(rawLine(t, msg.Record{Origin: "n1", Seq: 2, Msg: goodMsg("0000000002", "second")}))
	f.WriteString(`{"origin":"n1","seq":3,"msg":{"id":"0000000003","from":"a/b","to":"al`) // and a torn end
	os.WriteFile(logPath(dir), f.Bytes(), 0o600)
	s, err := Open(dir, "n1")
	if err != nil {
		t.Fatalf("must start: %v", err)
	}
	if vv := s.VV(); vv["n1"] != 2 {
		t.Errorf("the line that cannot be numbered is skipped, as an unparseable one always was: %v", vv)
	}
	if b, _ := os.ReadFile(logPath(dir) + ".torn"); !strings.Contains(string(b), `"seq":3`) {
		t.Error("the torn end is still set aside")
	}
}

func TestReadLinesDeliversEveryLengthAroundTheBufferSize(t *testing.T) {
	var want []string
	var f bytes.Buffer
	for _, n := range []int{0, 1, 4095, 65535, 65536, 65537, 131072, 200000} {
		l := strings.Repeat("a", n)
		want = append(want, l)
		f.WriteString(l + "\n")
	}
	f.WriteString("no newline at the end")
	want = append(want, "no newline at the end")
	path := filepath.Join(t.TempDir(), "log")
	os.WriteFile(path, f.Bytes(), 0o600)
	var got []string
	err := readLines(path, func(b []byte, over bool) {
		if over {
			t.Error("none is over the limit")
		}
		got = append(got, strings.TrimSuffix(string(b), "\n"))
	})
	if err != nil {
		t.Fatal(err)
	}
	// an empty line is given too (the caller finds nothing to parse in it)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %d lines of lengths %v, want %d of %v", len(got), lengths(got), len(want), lengths(want))
	}
}

func lengths(ss []string) []int {
	var out []int
	for _, s := range ss {
		out = append(out, len(s))
	}
	return out
}

func TestSinceBytesStopsAtTheBudgetButAlwaysReturnsOneRecord(t *testing.T) {
	src, _ := Open(t.TempDir(), "n1")
	src.SetSendLimit(1000)
	for i := 0; i < 10; i++ {
		if _, err := src.Send(msg.Message{From: "a/b", To: "all", Body: fmt.Sprintf("%d%s", i, strings.Repeat("p", msg.MaxBody-1))}); err != nil {
			t.Fatal(err)
		}
	}
	page := src.SinceBytes(map[string]uint64{}, 500, 100<<10)
	if len(page) < 2 || len(page) > 4 {
		t.Errorf("100 KiB holds about three 32 KiB messages: %d", len(page))
	}
	if got := src.SinceBytes(map[string]uint64{}, 500, 10); len(got) != 1 {
		t.Errorf("a record larger than the budget still goes out, alone: %d", len(got))
	}
	if got := src.SinceBytes(map[string]uint64{}, 500, 0); len(got) != 10 {
		t.Errorf("no budget, no limit: %d", len(got))
	}
	if got := src.SinceBytes(map[string]uint64{"n1": 10}, 500, 10); len(got) != 0 {
		t.Errorf("nothing missing, nothing returned: %d", len(got))
	}
	// A consumer that asks again until it gets nothing (every one in this repo, and the dashboard) loses nothing.
	dst, _ := Open(t.TempDir(), "n2")
	pages := 0
	for {
		recs := src.SinceBytes(dst.VV(), 500, 100<<10)
		if len(recs) == 0 {
			break
		}
		if n, _ := dst.Merge(recs); n == 0 {
			t.Fatal("a page that adds nothing would end the pull early")
		}
		pages++
	}
	if dst.Stats().Messages != 10 || pages < 3 {
		t.Errorf("drained %d messages in %d pages", dst.Stats().Messages, pages)
	}
}

// An append that crashed half way through a record longer than replay parses leaves a last
// line with no end. It was never acknowledged, so it takes no place in the numbering: the next
// record this node writes reuses its number, and must still be there after the next restart.
func TestATornOverLongTailKeepsNoPlaceInTheNumbering(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "n1")
	if _, err := s.Send(msg.Message{From: "imac/claude.a1b2", To: "all", Body: "first"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(logPath(dir), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"origin":"n1","seq":2,"msg":{"id":"0000000002","body":"` + strings.Repeat("A", maxLine+1<<20)) // no end
	f.Close()

	s, err = Open(dir, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Stats(); got.Quarantined != 0 || got.Messages != 1 {
		t.Fatalf("the torn tail is set aside, not numbered: %+v", got)
	}
	if _, err := s.Send(msg.Message{From: "imac/claude.a1b2", To: "all", Body: "second"}); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Stats(); got.Messages != 2 || s.VV()["n1"] != 2 {
		t.Errorf("the record written after the repair survives a restart: %+v", got)
	}
}
