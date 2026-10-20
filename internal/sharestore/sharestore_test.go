package sharestore

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

const th = "0a1b2c3d4e"

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func sealed(n int) []byte {
	b := make([]byte, n+28) // a nonce and a tag at least
	_, _ = rand.Read(b)
	return append([]byte(msg.ShareChunkMagic), b...)
}

func hexN(n int) string {
	b := make([]byte, n/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func ver(name string, chunks ...string) msg.ShareVersion {
	return msg.ShareVersion{ID: hexN(32), Name: name, KeyID: hexN(16), Chunks: chunks, Meta: "bWV0YQ==", By: "lap/claude.a1"}
}

func open(t *testing.T, dir, node string, lim Limits, c *clock, act func(string) time.Time) *Store {
	t.Helper()
	if act == nil {
		act = func(id string) time.Time {
			if id == th {
				return c.t
			}
			return time.Time{}
		}
	}
	s, err := Open(dir, node, lim, act, WithClock(c.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func put(t *testing.T, s *Store, v msg.ShareVersion) msg.ShareRecord {
	t.Helper()
	r, err := s.Put(th, v)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVersionsAreNumberedPerName(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), "n1", DefaultLimits(), c, nil)
	a, _ := s.PutChunk(th, sealed(100))
	b, _ := s.PutChunk(th, sealed(200))
	name, other := hexN(32), hexN(32)
	if r := put(t, s, ver(name, a)); r.Ver.Num != 1 || r.Ver.Size != int64(128+len(msg.ShareChunkMagic)) {
		t.Fatalf("first version: %+v", r.Ver)
	}
	if r := put(t, s, ver(name, a, b)); r.Ver.Num != 2 {
		t.Fatalf("second version is %d", r.Ver.Num)
	}
	if r := put(t, s, ver(other, b)); r.Ver.Num != 1 {
		t.Fatalf("another name starts at 1, got %d", r.Ver.Num)
	}
	v := ver(name, a)
	r1 := put(t, s, v)
	if r2 := put(t, s, v); r2.Seq != r1.Seq || r2.Ver.Num != 3 {
		t.Fatalf("a retried write is the same record: %d/%d vs %d/%d", r1.Seq, r1.Ver.Num, r2.Seq, r2.Ver.Num)
	}
	idx, ok := s.Index(th)
	if !ok || len(idx.Records) != 4 {
		t.Fatalf("index: %v %d", ok, len(idx.Records))
	}
	if _, err := s.Put(th, ver(name, hexN(64))); !errors.As(err, new(*ErrMissing)) {
		t.Fatalf("a version naming a chunk not here: %v", err)
	}
}

func TestNotesRefuseAStaleBase(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), "n1", DefaultLimits(), c, nil)
	a, _ := s.PutChunk(th, sealed(10))
	notes := hexN(32)
	n := func(base int) msg.ShareVersion { v := ver(notes, a); v.Notes, v.Base = true, base; return v }
	put(t, s, n(0))
	if _, err := s.Put(th, n(0)); !errors.As(err, new(*ErrConflict)) {
		t.Fatalf("a second first page: %v", err)
	}
	put(t, s, n(1))
	var ce *ErrConflict
	if _, err := s.Put(th, n(1)); !errors.As(err, &ce) || ce.Latest != 2 || ce.Base != 1 {
		t.Fatalf("an edit of version 1 after version 2: %v", err)
	}
	if r := put(t, s, n(2)); r.Ver.Num != 3 {
		t.Fatalf("after merging: version %d", r.Ver.Num)
	}
}

func TestCapsAndShape(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	lim := DefaultLimits()
	lim.Thread, lim.Node = 1000, 1500
	other := "ffeeddccbb"
	act := func(string) time.Time { return c.t }
	s := open(t, t.TempDir(), "n1", lim, c, act)
	if _, err := s.PutChunk(th, append([]byte("plain text, "), make([]byte, 100)...)); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("plaintext: %v", err)
	}
	if _, err := s.PutChunk(th, sealed(900)); err != nil {
		t.Fatal(err)
	}
	var fe *ErrFull
	if _, err := s.PutChunk(th, sealed(200)); !errors.As(err, &fe) || fe.Node {
		t.Fatalf("past the thread cap: %v", err)
	}
	if _, err := s.PutChunk(other, sealed(500)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutChunk(other, sealed(200)); !errors.As(err, &fe) || !fe.Node {
		t.Fatalf("past the node cap: %v", err)
	}
	s2 := open(t, t.TempDir(), "n2", lim, c, func(string) time.Time { return time.Time{} })
	if _, err := s2.PutChunk(th, sealed(10)); !errors.Is(err, ErrUnknownThread) {
		t.Fatalf("a thread the node never saw: %v", err)
	}
}

func TestExpiryFollowsTheThread(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	last := c.t
	act := func(id string) time.Time { return last }
	dir := t.TempDir()
	s := open(t, dir, "n1", DefaultLimits(), c, act)
	a, _ := s.PutChunk(th, sealed(10))
	put(t, s, ver(hexN(32), a))
	c.t = c.t.Add(6 * 24 * time.Hour)
	last = c.t // a new message in the thread keeps it
	c.t = c.t.Add(6 * 24 * time.Hour)
	if _, ok := s.Index(th); !ok {
		t.Fatal("gone 12 days after the write though the thread spoke 6 days ago")
	}
	c.t = c.t.Add(25 * time.Hour)
	if _, ok := s.Index(th); ok {
		t.Fatal("still there 7 days after the thread's last message")
	}
	if _, ok := s.Chunk(th, a); ok {
		t.Fatal("its chunk is still served")
	}
	if st := s.Stats(); st.Bytes != 0 || st.Records != 0 {
		t.Fatalf("stats after expiry: %+v", st)
	}
	s.Close()
	s = open(t, dir, "n1", DefaultLimits(), c, act)
	if vv := s.VV(); vv["n1"] != 1 {
		t.Fatalf("a restart forgot the numbering of what expired: %v", vv)
	}
}

func TestUnusedChunksGo(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), "n1", DefaultLimits(), c, nil)
	a, _ := s.PutChunk(th, sealed(10))
	b, _ := s.PutChunk(th, sealed(10))
	put(t, s, ver(hexN(32), a))
	c.t = c.t.Add(2 * time.Hour)
	s.Sweep()
	if _, ok := s.Chunk(th, a); !ok {
		t.Fatal("a used chunk went")
	}
	if _, ok := s.Chunk(th, b); ok {
		t.Fatal("an unused chunk stayed past an hour")
	}
}

func TestRestartKeepsEverything(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	s := open(t, dir, "n1", DefaultLimits(), c, nil)
	a, _ := s.PutChunk(th, sealed(4000))
	name := hexN(32)
	put(t, s, ver(name, a))
	put(t, s, ver(name, a))
	k := msg.ShareKey{ID: hexN(16), For: "lap", Box: "AAAA", Sealed: "AAAA", By: "lap/claude.a1"}
	if _, err := s.PutKey(th, k); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Index(th)
	s.Close()
	s = open(t, dir, "n1", DefaultLimits(), c, nil)
	after, _ := s.Index(th)
	if len(after.Records) != 3 || after.Bytes != before.Bytes || s.VV()["n1"] != 3 {
		t.Fatalf("after a restart: %d records, %d bytes, vv %v", len(after.Records), after.Bytes, s.VV())
	}
	if r := put(t, s, ver(name, a)); r.Ver.Num != 3 || r.Seq != 4 {
		t.Fatalf("numbering after a restart: num %d seq %d", r.Ver.Num, r.Seq)
	}
}

// replicate copies what b lacks from a, as the node's replicator does.
func replicate(t *testing.T, from, to *Store) int {
	t.Helper()
	total := 0
	for {
		n, _, err := to.Merge(from.Since(to.VV(), 2, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		for _, ref := range to.Missing() {
			data, ok := from.Chunk(ref.Thread, ref.SHA)
			if !ok {
				t.Fatalf("source lacks %v", ref)
			}
			if err := to.PutReplica(ref.Thread, ref.SHA, data); err != nil {
				t.Fatal(err)
			}
		}
		total += n
		if n == 0 {
			return total
		}
	}
}

func TestMergeBetweenNodes(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	a := open(t, t.TempDir(), "na", DefaultLimits(), c, nil)
	b := open(t, t.TempDir(), "nb", DefaultLimits(), c, nil)
	name := hexN(32)
	var chunks []string
	for range 3 {
		h, _ := a.PutChunk(th, sealed(1000))
		chunks = append(chunks, h)
	}
	put(t, a, ver(name, chunks...))
	if _, err := a.PutKey(th, msg.ShareKey{ID: hexN(16), For: "lap", Box: "AAAA", Sealed: "AAAA", By: "lap/claude.a1"}); err != nil {
		t.Fatal(err)
	}
	// b holds the version record before its chunks: it is not listed yet
	if n, need, _ := b.Merge(a.Since(b.VV(), 10, 1<<20)); n != 2 || len(need) != 3 {
		t.Fatalf("merge: %d records, %d chunks needed", n, len(need))
	}
	if idx, _ := b.Index(th); len(idx.Records) != 1 {
		t.Fatalf("listed before its chunks came: %d", len(idx.Records))
	}
	if got := len(b.Missing()); got != 3 {
		t.Fatalf("missing: %d", got)
	}
	if err := b.PutReplica(th, chunks[0], sealed(5)); err == nil || len(b.Missing()) != 3 {
		t.Fatal("a chunk that does not hash to its name was kept")
	}
	replicate(t, a, b)
	for _, h := range chunks {
		x, _ := a.Chunk(th, h)
		if y, ok := b.Chunk(th, h); !ok || !bytes.Equal(x, y) {
			t.Fatal("chunk differs after replication")
		}
	}
	// both write version 2 at once: each keeps both, numbered as written
	h, _ := b.PutChunk(th, sealed(10))
	put(t, b, ver(name, h))
	put(t, a, ver(name, chunks[0]))
	replicate(t, a, b)
	replicate(t, b, a)
	ia, _ := a.Index(th)
	ib, _ := b.Index(th)
	if len(ia.Records) != 4 || len(ib.Records) != 4 {
		t.Fatalf("after both wrote: %d and %d records", len(ia.Records), len(ib.Records))
	}
	if replicate(t, a, b)+replicate(t, b, a) != 0 {
		t.Fatal("replication does not settle")
	}
}

func TestMergeDropsWhatExpired(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	never := func(string) time.Time { return time.Time{} }
	a := open(t, t.TempDir(), "na", DefaultLimits(), c, nil)
	dir := t.TempDir()
	b := open(t, dir, "nb", DefaultLimits(), c, never)
	h, _ := a.PutChunk(th, sealed(10))
	put(t, a, ver(hexN(32), h))
	recs := a.Since(nil, 10, 1<<20)
	c.t = c.t.Add(8 * 24 * time.Hour)
	if n, need, _ := b.Merge(recs); n != 1 || len(need) != 0 {
		t.Fatalf("an expired record: %d taken, %d chunks wanted", n, len(need))
	}
	if _, ok := b.Index(th); ok {
		t.Fatal("an expired record is listed")
	}
	b.Close()
	b = open(t, dir, "nb", DefaultLimits(), c, never)
	if b.VV()["na"] != 1 {
		t.Fatalf("a restart would ask for the expired record again: %v", b.VV())
	}
}
