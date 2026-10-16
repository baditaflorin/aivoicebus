package store

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// naiveInbox is what InboxFor did before the indexes: look at every message.
func naiveInbox(s *Store, me string, all bool, labels msg.Labels, now time.Time) []msg.Message {
	var out []msg.Message
	for _, m := range s.msgs {
		if m.From == me || !msg.MatchesLabeled(m.To, me, labels) {
			continue
		}
		if !all && m.Expired(now) {
			continue
		}
		if !all && s.acks[m.ID][me] {
			continue
		}
		out = append(out, m)
	}
	return out
}

func naiveThread(s *Store, id string) []msg.Message {
	t := id
	for _, m := range s.msgs {
		if m.ID == id {
			t = m.Thread
		}
	}
	var out []msg.Message
	for _, m := range s.msgs {
		if m.Thread == t {
			out = append(out, m)
		}
	}
	return out
}

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.Intn(len(xs))] }

// universe makes identities, targets and labels that exercise every way a target
// can reach (or fail to reach) an identity, including odd and malformed ones.
type universe struct {
	hosts, tools, sessions []string
	repos                  []string
}

func newUniverse() universe {
	return universe{
		hosts:    []string{"imac", "laptop-a", "air", "node-a", "Mixed"},
		tools:    []string{"claude", "codex", "gemini", "agent"},
		sessions: []string{"", "", "a1b2", "c3d4"},
		repos:    []string{"alpha", "beta", "aivoicebus", "al"},
	}
}

func (u universe) identity(r *rand.Rand) string {
	id := pick(r, u.hosts) + "/" + pick(r, u.tools)
	if s := pick(r, u.sessions); s != "" {
		id += "." + s
	}
	return id
}

func (u universe) target(r *rand.Rand) string {
	h, tool := pick(r, u.hosts), pick(r, u.tools)
	switch r.Intn(16) {
	case 0:
		return "all"
	case 1:
		return h
	case 2:
		return h + "/" + tool
	case 3:
		return u.identity(r)
	case 4:
		return "*/" + tool
	case 5:
		return h + "/*"
	case 6:
		return "*/*"
	case 7:
		return "@repo=" + pick(r, u.repos)
	case 8:
		return "@repo=" + pick(r, u.repos)[:1] + "*"
	case 9:
		return "@tool=" + tool + ",repo=" + pick(r, u.repos)
	case 10:
		return "@branch=feat/*"
	case 11:
		return "@bad key=x" // malformed: reaches nobody
	case 12:
		return h + "/"
	case 13:
		return h + "/" + tool + "/extra"
	case 14:
		return "@repo=" + pick(r, u.repos) + "|" + pick(r, u.repos) // not valid until selectors v2
	default:
		return h + "/" + tool + "." + pick(r, []string{"a1b2", "zzzz"})
	}
}

func (u universe) labels(r *rand.Rand) msg.Labels {
	l := msg.Labels{}
	if r.Intn(4) > 0 {
		l["repo"] = pick(r, u.repos)
	}
	if r.Intn(3) == 0 {
		l["branch"] = pick(r, []string{"main", "feat/x", "feat/y"})
	}
	if r.Intn(2) == 0 {
		l["tool"] = pick(r, u.tools)
	}
	return l
}

func TestIndexedReadsReturnExactlyWhatAFullScanReturns(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	u := newUniverse()
	s, _ := Open(t.TempDir(), "n1")
	s.SetSendLimit(1 << 20)
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })

	var ids []string
	for i := 0; i < 600; i++ {
		clock = clock.Add(time.Minute)
		m := msg.Message{From: u.identity(r), To: u.target(r), Body: fmt.Sprintf("message %d", i), Prio: pick(r, []string{"", "fyi", "ask", "urgent"})}
		if r.Intn(5) == 0 {
			m.TTL = int64(60 * (1 + r.Intn(600))) // some expire before the end
		}
		if len(ids) > 0 && r.Intn(5) == 0 {
			m.Re = pick(r, ids)
		}
		out, err := s.Send(m)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, out.ID)
		if r.Intn(3) == 0 {
			s.Ack(pick(r, ids), u.identity(r))
		}
	}
	// the same message arriving a second time from another node is one message
	dup := *s.msgsPtr(ids[0])
	if n, _ := s.Merge([]msg.Record{{Origin: "n2", Seq: 1, Msg: &dup}}); n != 1 {
		t.Fatal("a record from another node is applied")
	}
	if len(s.msgs) != len(ids) {
		t.Fatalf("a duplicate id must not add a message: %d vs %d", len(s.msgs), len(ids))
	}

	for q := 0; q < 400; q++ {
		me, labels, all := u.identity(r), u.labels(r), r.Intn(3) == 0
		clock = clock.Add(time.Duration(r.Intn(5)) * time.Hour)
		got := s.InboxFor(me, all, labels)
		s.mu.Lock()
		want := naiveInbox(s, me, all, labels, clock)
		s.mu.Unlock()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("query %d for %s all=%v %v: indexed gives %d messages, a full scan %d\nindexed: %v\nscan:    %v", q, me, all, labels, len(got), len(want), msgIDs(got), msgIDs(want))
		}
	}
	for _, id := range ids[:100] {
		s.mu.Lock()
		want := naiveThread(s, id)
		s.mu.Unlock()
		if got := s.Thread(id); !reflect.DeepEqual(got, want) {
			t.Fatalf("thread of %s: %d vs %d messages", id, len(got), len(want))
		}
	}
	if got := s.Thread("0000000000"); got != nil {
		t.Errorf("an unknown thread has no messages: %v", got)
	}
	// who and the roster come from the per-sender index and the sessions that polled;
	// compare with a scan of the messages plus the polls
	want := map[string]time.Time{}
	for _, m := range s.msgs {
		if m.At.After(want[m.From]) {
			want[m.From] = m.At
		}
	}
	for me, info := range s.seen {
		if info.at.After(want[me]) {
			want[me] = info.at
		}
	}
	got := s.Who()
	if len(got) != len(want) {
		t.Errorf("who has %d identities, a scan %d", len(got), len(want))
	}
	for me, at := range want {
		if !got[me].Equal(at) {
			t.Errorf("who(%s) = %v, want %v", me, got[me], at)
		}
	}
	if roster := s.Roster(); len(roster) != len(want) {
		t.Errorf("roster has %d identities, want %d", len(roster), len(want))
	}
	if st := s.Stats(); st.Messages != len(ids) {
		t.Errorf("stats %+v", st)
	}
}

func msgIDs(ms []msg.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func (s *Store) msgsPtr(id string) *msg.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.msgs[s.idx[id]]
	return &m
}

func TestAckCountIsKeptWithoutScanning(t *testing.T) {
	s, _ := Open(t.TempDir(), "n1")
	m, _ := s.Send(msg.Message{From: "a/b", To: "all", Body: "x"})
	s.Ack(m.ID, "p/q")
	s.Ack(m.ID, "p/q") // the same receipt twice
	s.Ack(m.ID, "r/s")
	if st := s.Stats(); st.Acks != 2 {
		t.Errorf("two distinct receipts: %+v", st)
	}
}

// 100,000 messages, almost all for someone else, and a few for the reader.
func benchStore(b *testing.B) *Store {
	b.Helper()
	s, _ := Open(b.TempDir(), "n1")
	s.SetSendLimit(1 << 30)
	r := rand.New(rand.NewSource(1))
	u := newUniverse()
	for i := 0; i < 100000; i++ {
		to := "other" + fmt.Sprint(r.Intn(500)) + "/claude.s" + fmt.Sprint(r.Intn(50))
		if i%5000 == 0 {
			to = "reader/claude.a1b2"
		}
		if i%9000 == 0 {
			to = "@repo=alpha"
		}
		s.mu.Lock()
		s.addMsgLocked(msg.Message{ID: fmt.Sprintf("%010x", i), From: u.identity(r), To: to, Thread: fmt.Sprintf("%010x", i), Body: "x", At: time.Now()})
		s.mu.Unlock()
	}
	return s
}

func BenchmarkInboxForIndexed100k(b *testing.B) {
	s := benchStore(b)
	l := msg.Labels{"repo": "alpha"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.InboxFor("reader/claude.a1b2", false, l)
	}
}

func BenchmarkInboxForFullScan100k(b *testing.B) {
	s := benchStore(b)
	l := msg.Labels{"repo": "alpha"}
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		naiveInbox(s, "reader/claude.a1b2", false, l, now)
	}
}

var _ = slices.Contains[[]string]

// BenchmarkOpen100k is the cost of a node start: replaying a 100,000-record log
// from disk and building every index. It also reports the log's size per record.
func BenchmarkOpen100k(b *testing.B) {
	dir := b.TempDir()
	s, _ := Open(dir, "n1")
	s.SetSendLimit(1 << 30)
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 100000; i++ {
		to := "other" + fmt.Sprint(r.Intn(500)) + "/claude.s" + fmt.Sprint(r.Intn(50))
		m, err := s.Send(msg.Message{From: "a/claude.1", To: to, Body: fmt.Sprintf("a typical message body of moderate length, number %d: build is green on main, please rebase when you can", i)})
		if err != nil {
			b.Fatal(err)
		}
		if i%2 == 0 {
			s.AckWith(msg.Ack{ID: m.ID, By: to, Via: msg.ViaHook, Chars: 120})
		}
	}
	fi, _ := os.Stat(filepath.Join(dir, "log.jsonl"))
	s = nil
	b.ResetTimer()
	var held *Store
	for i := 0; i < b.N; i++ {
		var err error
		if held, err = Open(dir, "n1"); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	b.ReportMetric(float64(fi.Size())/150000, "log-bytes/record")
	b.ReportMetric(float64(ms.HeapAlloc)/(1<<20), "live-heap-MB")
	runtime.KeepAlive(held)
}
