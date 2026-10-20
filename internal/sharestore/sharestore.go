// Package sharestore is a node's half of the shared spaces (msg/share.go): sealed chunks on disk
// and a log of versions and sealed keys, replicated between nodes by version vector like the
// message log but kept apart from it. It never sees a key, a name or a byte of content.
//
// On disk, under its directory: log.jsonl (one record per line; a {"vv":...} line first after a
// compaction keeps the numbering of records it dropped) and chunks/THREAD/SHA256 (one sealed chunk
// each). A version shows only once every chunk it names is on this node: a replicated version
// whose chunks are still on their way is held but not listed.
package sharestore

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Limits bound what a node holds.
type Limits struct {
	Thread  int64         // sealed bytes in one thread's space
	Node    int64         // sealed bytes on this node
	TTL     time.Duration // a space lives this long after the thread's last message or write
	Pending time.Duration // a chunk no version uses is dropped after this
}

// DefaultLimits are the release's.
func DefaultLimits() Limits {
	return Limits{Thread: msg.ShareThreadMax, Node: msg.ShareNodeMax, TTL: msg.ShareTTL, Pending: msg.SharePendingTTL}
}

// ErrConflict is a write that names a version which is no longer the latest.
type ErrConflict struct{ Latest, Base int }

func (e *ErrConflict) Error() string {
	return fmt.Sprintf("conflict: it is at version %d and this write edits version %d: get version %d, merge your change into it, and put again naming version %d", e.Latest, e.Base, e.Latest, e.Latest)
}

// ErrFull is a write that would take a space, or the node, past its cap.
type ErrFull struct {
	Node     bool
	Has, Max int64
}

func (e *ErrFull) Error() string {
	if e.Node {
		return fmt.Sprintf("this node holds as much shared data as it may (%d of %d MiB): try another node, or send the file once through the pipe (`aivoicebus share put ID FILE --via pipe`)", e.Has>>20, e.Max>>20)
	}
	return fmt.Sprintf("this thread's shared space holds %d of %d MiB: share it in a new thread, or send it once through the pipe (`aivoicebus share put ID FILE --via pipe`)", e.Has>>20, e.Max>>20)
}

var (
	// ErrUnknownThread is a write for a thread this node has never seen.
	ErrUnknownThread = errors.New("this node holds no message of that thread: name a thread by a message id from `aivoicebus inbox`, or try again in a minute if it was just sent")
	// ErrNotSealed is a chunk that is not shaped like one a client sealed.
	ErrNotSealed = errors.New("bad request: a node holds only sealed chunks (`aivoicebus share put` makes them)")
	// ErrTooMany is a thread with as many versions and keys as a space holds.
	ErrTooMany = fmt.Errorf("this thread's shared space holds %d versions and keys already: share it in a new thread", msg.ShareMaxRecords)
)

// ErrMissing is a version naming chunks this node does not hold.
type ErrMissing struct{ N int }

func (e *ErrMissing) Error() string {
	return fmt.Sprintf("bad request: %d chunk(s) of this version are not on this node: upload them to this node, then put again", e.N)
}

// ChunkRef names one chunk of one thread.
type ChunkRef struct{ Thread, SHA string }

type thread struct {
	idx     []int            // into Store.recs
	chunks  map[string]int64 // on disk: sha -> size
	pending map[string]time.Time
	bytes   int64
	last    time.Time // the newest record's At
}

// Store is one node's shared spaces.
type Store struct {
	mu       sync.Mutex
	dir      string
	node     string
	lim      Limits
	now      func() time.Time
	activity func(thread string) time.Time
	vv       map[string]uint64
	recs     []msg.ShareRecord
	threads  map[string]*thread
	bytes    int64
	log      *os.File
	out      io.Writer
}

// Option customises Open.
type Option func(*Store)

// WithClock sets the clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// WithLog sends a line about anything dropped while reading to w.
func WithLog(w io.Writer) Option { return func(s *Store) { s.out = w } }

// Open reads the store in dir (made if missing). activity says when a thread last had a message
// (zero for none known), which keeps its space alive and lets a writer use it.
func Open(dir, node string, lim Limits, activity func(string) time.Time, opts ...Option) (*Store, error) {
	if activity == nil {
		activity = func(string) time.Time { return time.Time{} }
	}
	s := &Store{dir: dir, node: node, lim: lim, now: time.Now, activity: activity, vv: map[string]uint64{}, threads: map[string]*thread{}, out: io.Discard}
	for _, o := range opts {
		o(s)
	}
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o700); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanChunksLocked()
	s.sweepLocked(true)
	f, err := os.OpenFile(s.logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	s.log = f
	return s, nil
}

func (s *Store) logPath() string { return filepath.Join(s.dir, "log.jsonl") }

type header struct {
	VV map[string]uint64 `json:"vv"`
}

func (s *Store) load() error {
	f, err := os.Open(s.logPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	rd := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 && err == nil { // a last line without its newline was cut off mid-write
			s.loadLine(line)
		}
		if err != nil {
			break
		}
	}
	return nil
}

func (s *Store) loadLine(line []byte) {
	if bytes.HasPrefix(line, []byte(`{"vv"`)) {
		var h header
		if json.Unmarshal(line, &h) == nil {
			for o, n := range h.VV {
				s.vv[o] = max(s.vv[o], n)
			}
		}
		return
	}
	var r msg.ShareRecord
	if json.Unmarshal(line, &r) != nil || r.Validate() != nil {
		fmt.Fprintln(s.out, "share: dropped an unreadable record from the log")
		return
	}
	if r.Seq <= s.vv[r.Origin] && s.has(r) {
		return
	}
	s.vv[r.Origin] = max(s.vv[r.Origin], r.Seq)
	s.addLocked(r)
}

// has reports whether the exact record is held (a log can repeat a line after a crash).
func (s *Store) has(r msg.ShareRecord) bool {
	t := s.threads[r.Thread]
	if t == nil {
		return false
	}
	for _, i := range t.idx {
		if s.recs[i].Origin == r.Origin && s.recs[i].Seq == r.Seq {
			return true
		}
	}
	return false
}

func (s *Store) threadLocked(id string) *thread {
	t := s.threads[id]
	if t == nil {
		t = &thread{chunks: map[string]int64{}, pending: map[string]time.Time{}}
		s.threads[id] = t
	}
	return t
}

func (s *Store) addLocked(r msg.ShareRecord) {
	t := s.threadLocked(r.Thread)
	s.recs = append(s.recs, r)
	t.idx = append(t.idx, len(s.recs)-1)
	if r.At.After(t.last) {
		t.last = r.At
	}
	if r.Ver != nil {
		for _, c := range r.Ver.Chunks {
			delete(t.pending, c)
		}
	}
}

func (s *Store) chunkDir(thread string) string { return filepath.Join(s.dir, "chunks", thread) }

// scanChunksLocked finds the chunks on disk; one no record uses is pending from its file's time.
func (s *Store) scanChunksLocked() {
	dirs, _ := os.ReadDir(filepath.Join(s.dir, "chunks"))
	for _, d := range dirs {
		if !d.IsDir() || !msg.ValidShareThread(d.Name()) {
			continue
		}
		t := s.threadLocked(d.Name())
		used := map[string]bool{}
		for _, i := range t.idx {
			if v := s.recs[i].Ver; v != nil {
				for _, c := range v.Chunks {
					used[c] = true
				}
			}
		}
		files, _ := os.ReadDir(s.chunkDir(d.Name()))
		for _, f := range files {
			info, err := f.Info()
			if err != nil || !info.Mode().IsRegular() || !msg.ValidShareChunkID(f.Name()) {
				if strings.HasSuffix(f.Name(), ".tmp") {
					_ = os.Remove(filepath.Join(s.chunkDir(d.Name()), f.Name()))
				}
				continue
			}
			t.chunks[f.Name()] = info.Size()
			t.bytes += info.Size()
			s.bytes += info.Size()
			if !used[f.Name()] {
				t.pending[f.Name()] = info.ModTime()
			}
		}
	}
}

// expiresLocked is when thread id's space goes.
func (s *Store) expiresLocked(id string, t *thread) time.Time {
	last := t.last
	if a := s.activity(id); a.After(last) {
		last = a
	}
	for _, at := range t.pending {
		if at.After(last) {
			last = at
		}
	}
	return last.Add(s.lim.TTL)
}

// sweepLocked drops expired spaces and chunks nobody used in time; it rewrites the log when it
// dropped a record, or when force is set and the log holds lines it no longer needs.
func (s *Store) sweepLocked(force bool) {
	now := s.now()
	dropped := false
	for id, t := range s.threads {
		for c, at := range t.pending {
			if now.Sub(at) > s.lim.Pending {
				s.removeChunkLocked(id, t, c)
			}
		}
		if len(t.idx) > 0 && now.After(s.expiresLocked(id, t)) {
			_ = os.RemoveAll(s.chunkDir(id))
			s.bytes -= t.bytes
			delete(s.threads, id)
			dropped = true
			continue
		}
		if len(t.idx) == 0 && len(t.chunks) == 0 {
			delete(s.threads, id)
		}
	}
	if dropped {
		keep := s.recs[:0]
		for _, r := range s.recs {
			if s.threads[r.Thread] != nil {
				keep = append(keep, r)
			}
		}
		s.recs = keep
		s.reindexLocked()
	}
	if dropped || force {
		if err := s.compactLocked(); err != nil {
			fmt.Fprintln(s.out, "share: could not rewrite the log:", err)
		}
	}
}

func (s *Store) removeChunkLocked(id string, t *thread, c string) {
	if n, ok := t.chunks[c]; ok {
		_ = os.Remove(filepath.Join(s.chunkDir(id), c))
		t.bytes -= n
		s.bytes -= n
		delete(t.chunks, c)
	}
	delete(t.pending, c)
}

func (s *Store) reindexLocked() {
	for _, t := range s.threads {
		t.idx, t.last = t.idx[:0], time.Time{}
	}
	for i, r := range s.recs {
		t := s.threadLocked(r.Thread)
		t.idx = append(t.idx, i)
		if r.At.After(t.last) {
			t.last = r.At
		}
	}
}

// compactLocked rewrites the log as the numbering and the records held now.
func (s *Store) compactLocked() error {
	tmp := s.logPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	_ = enc.Encode(header{VV: s.vv})
	for _, r := range s.recs {
		_ = enc.Encode(r)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if err := os.Rename(tmp, s.logPath()); err != nil {
		return err
	}
	if s.log != nil {
		s.log.Close()
		s.log, err = os.OpenFile(s.logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	}
	return err
}

func (s *Store) appendLocked(r msg.ShareRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := s.log.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := s.log.Sync(); err != nil {
		return err
	}
	s.vv[r.Origin] = r.Seq
	s.addLocked(r)
	return nil
}

// Sweep drops what expired. The node calls it now and then.
func (s *Store) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(false)
}

// knownLocked reports whether this node may take a write for thread id: it has a message of it,
// or a space for it already.
func (s *Store) knownLocked(id string) bool {
	if t := s.threads[id]; t != nil && len(t.idx) > 0 {
		return true
	}
	return !s.activity(id).IsZero()
}

// looksSealed checks a chunk's shape: the magic, a 12-byte nonce, and at least a GCM tag.
func looksSealed(data []byte) bool {
	return len(data) >= len(msg.ShareChunkMagic)+12+16 && len(data) <= msg.ShareChunkMax && string(data[:len(msg.ShareChunkMagic)]) == msg.ShareChunkMagic
}

// PutChunk stores a sealed chunk a client uploaded and returns its hash. Until a version names it
// it is pending, and goes after Limits.Pending.
func (s *Store) PutChunk(id string, data []byte) (string, error) {
	if !looksSealed(data) {
		return "", ErrNotSealed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(false)
	if !s.knownLocked(id) {
		return "", ErrUnknownThread
	}
	return s.storeChunkLocked(id, data, "")
}

// PutReplica stores a chunk fetched from a peer, which must hash to sha.
func (s *Store) PutReplica(id, sha string, data []byte) error {
	if !looksSealed(data) {
		return ErrNotSealed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.threads[id] == nil || !s.wantedLocked(id, sha) {
		return nil // expired meanwhile, or not wanted: nothing to keep
	}
	_, err := s.storeChunkLocked(id, data, sha)
	return err
}

func (s *Store) wantedLocked(id, sha string) bool {
	t := s.threads[id]
	if _, have := t.chunks[sha]; have {
		return false
	}
	for _, i := range t.idx {
		if v := s.recs[i].Ver; v != nil {
			for _, c := range v.Chunks {
				if c == sha {
					return true
				}
			}
		}
	}
	return false
}

func (s *Store) storeChunkLocked(id string, data []byte, want string) (string, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	if want != "" && sha != want {
		return "", fmt.Errorf("the chunk a peer sent does not hash to %s", want) // nextstep: the replicator logs it and asks again
	}
	t := s.threadLocked(id)
	if _, have := t.chunks[sha]; have {
		if want == "" && !s.usedLocked(t, sha) {
			t.pending[sha] = s.now()
		}
		return sha, nil
	}
	n := int64(len(data))
	if t.bytes+n > s.lim.Thread {
		return "", &ErrFull{Has: t.bytes, Max: s.lim.Thread}
	}
	if s.bytes+n > s.lim.Node {
		return "", &ErrFull{Node: true, Has: s.bytes, Max: s.lim.Node}
	}
	if err := os.MkdirAll(s.chunkDir(id), 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(s.chunkDir(id), sha)
	if err := os.WriteFile(p+".tmp", data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		return "", err
	}
	t.chunks[sha] = n
	t.bytes += n
	s.bytes += n
	if !s.usedLocked(t, sha) {
		t.pending[sha] = s.now()
	}
	return sha, nil
}

func (s *Store) usedLocked(t *thread, sha string) bool {
	for _, i := range t.idx {
		if v := s.recs[i].Ver; v != nil {
			for _, c := range v.Chunks {
				if c == sha {
					return true
				}
			}
		}
	}
	return false
}

// Chunk returns a sealed chunk.
func (s *Store) Chunk(id, sha string) ([]byte, bool) {
	s.mu.Lock()
	t := s.threads[id]
	ok := t != nil && t.chunks[sha] > 0
	s.mu.Unlock()
	if !ok || !msg.ValidShareThread(id) || !msg.ValidShareChunkID(sha) {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(s.chunkDir(id), sha))
	return b, err == nil
}

// latestLocked is the highest number any version of name holds in thread t (0 for none).
func (s *Store) latestLocked(t *thread, name string) int {
	n := 0
	for _, i := range t.idx {
		if v := s.recs[i].Ver; v != nil && v.Name == name && v.Num > n {
			n = v.Num
		}
	}
	return n
}

// Put takes a version a client wrote: every chunk it names must be here. A version that names a
// base (notes always do) is refused unless that base is still the latest. The node numbers it.
func (s *Store) Put(id string, v msg.ShareVersion) (msg.ShareRecord, error) {
	v.Num, v.Size = 0, 0
	if err := v.Validate(); err != nil {
		return msg.ShareRecord{}, err
	}
	if v.Notes && v.Base == 0 {
		v.Base = -1 // notes always name what they edit; 0 means a first page, checked below
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(false)
	if !s.knownLocked(id) {
		return msg.ShareRecord{}, ErrUnknownThread
	}
	t := s.threadLocked(id)
	if len(t.idx) >= msg.ShareMaxRecords {
		return msg.ShareRecord{}, ErrTooMany
	}
	for _, i := range t.idx {
		if r := s.recs[i]; r.Ver != nil && r.Ver.ID == v.ID {
			return r, nil // the same write again (a retry): it is already held
		}
	}
	missing := 0
	for _, c := range v.Chunks {
		if n, ok := t.chunks[c]; ok {
			v.Size += n
		} else {
			missing++
		}
	}
	if missing > 0 {
		return msg.ShareRecord{}, &ErrMissing{N: missing}
	}
	latest := s.latestLocked(t, v.Name)
	switch {
	case v.Base == -1:
		if latest != 0 {
			return msg.ShareRecord{}, &ErrConflict{Latest: latest, Base: 0}
		}
		v.Base = 0
	case (v.Notes || v.Base > 0) && v.Base != latest:
		return msg.ShareRecord{}, &ErrConflict{Latest: latest, Base: v.Base}
	}
	v.Num = latest + 1
	r := msg.ShareRecord{Origin: s.node, Seq: s.vv[s.node] + 1, At: s.now().UTC(), Thread: id, Ver: &v}
	return r, s.appendLocked(r)
}

// PutKey keeps a sealed key. The same key sealed for the same machine again is a no-op.
func (s *Store) PutKey(id string, k msg.ShareKey) (msg.ShareRecord, error) {
	if err := k.Validate(); err != nil {
		return msg.ShareRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(false)
	if !s.knownLocked(id) {
		return msg.ShareRecord{}, ErrUnknownThread
	}
	t := s.threadLocked(id)
	if len(t.idx) >= msg.ShareMaxRecords {
		return msg.ShareRecord{}, ErrTooMany
	}
	for _, i := range t.idx {
		if r := s.recs[i]; r.Key != nil && r.Key.ID == k.ID && r.Key.For == k.For && r.Key.By == k.By {
			return r, nil
		}
	}
	r := msg.ShareRecord{Origin: s.node, Seq: s.vv[s.node] + 1, At: s.now().UTC(), Thread: id, Key: &k}
	return r, s.appendLocked(r)
}

func (s *Store) completeLocked(t *thread, r msg.ShareRecord) bool {
	if r.Ver == nil {
		return true
	}
	for _, c := range r.Ver.Chunks {
		if _, ok := t.chunks[c]; !ok {
			return false
		}
	}
	return true
}

// Index is what this node holds of thread id's space: every key, and every version whose chunks
// are all here. ok is false when it holds nothing of it.
func (s *Store) Index(id string) (msg.ShareIndex, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(false)
	t := s.threads[id]
	if t == nil || len(t.idx) == 0 {
		return msg.ShareIndex{Thread: id, Max: s.lim.Thread, Records: []msg.ShareRecord{}}, false
	}
	out := msg.ShareIndex{Thread: id, Expires: s.expiresLocked(id, t).UTC(), Bytes: t.bytes, Max: s.lim.Thread, Records: []msg.ShareRecord{}}
	for _, i := range t.idx {
		if r := s.recs[i]; s.completeLocked(t, r) {
			out.Records = append(out.Records, r)
		}
	}
	sort.SliceStable(out.Records, func(i, j int) bool { return out.Records[i].At.Before(out.Records[j].At) })
	return out, true
}

// VV is the highest contiguous sequence held per origin.
func (s *Store) VV() map[string]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]uint64, len(s.vv))
	for k, v := range s.vv {
		out[k] = v
	}
	return out
}

// Empty reports whether this node has never held a record (a fresh disk).
func (s *Store) Empty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.vv) == 0
}

// Since returns the records held beyond vv, in each origin's order, at most limit of them and
// about maxBytes of JSON.
func (s *Store) Since(vv map[string]uint64, limit, maxBytes int) []msg.ShareRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []msg.ShareRecord{}
	size := 0
	for _, r := range s.recs {
		if r.Seq <= vv[r.Origin] {
			continue
		}
		b, _ := json.Marshal(r)
		if len(out) > 0 && (len(out) >= limit || size+len(b) > maxBytes) {
			break
		}
		out = append(out, r)
		size += len(b)
	}
	return out
}

// Merge takes records from a peer. Each origin's records are taken in order: a gap stops that
// origin until the missing ones come. A record for a space that has expired here is counted and
// dropped. It returns how many records were new and the chunks they need.
func (s *Store) Merge(recs []msg.ShareRecord) (int, []ChunkRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	var need []ChunkRef
	now := s.now()
	for _, r := range recs {
		if r.Seq != s.vv[r.Origin]+1 {
			continue
		}
		if err := r.Validate(); err != nil {
			fmt.Fprintf(s.out, "share: dropped record %s/%d from a peer: %v\n", r.Origin, r.Seq, err)
			if err := s.skipLocked(r.Origin, r.Seq); err != nil { // never asked for again; the numbering stays whole
				return n, need, err
			}
			n++
			continue
		}
		last := r.At
		if a := s.activity(r.Thread); a.After(last) {
			last = a
		}
		if t := s.threads[r.Thread]; t != nil && t.last.After(last) {
			last = t.last
		}

		t := s.threads[r.Thread]
		if now.After(last.Add(s.lim.TTL)) || (t != nil && len(t.idx) >= msg.ShareMaxRecords) {
			if err := s.skipLocked(r.Origin, r.Seq); err != nil {
				return n, need, err
			}
			n++
			continue
		}
		t = s.threadLocked(r.Thread)
		if err := s.appendLocked(r); err != nil {
			return n, need, err
		}
		n++
		if r.Ver != nil {
			for _, c := range r.Ver.Chunks {
				if _, ok := t.chunks[c]; !ok {
					need = append(need, ChunkRef{r.Thread, c})
				}
			}
		}
	}
	return n, need, nil
}

// skipLocked moves the numbering past a record that is not kept, and writes that down, so a
// restart does not ask peers for it again.
func (s *Store) skipLocked(origin string, seq uint64) error {
	s.vv[origin] = seq
	b, _ := json.Marshal(header{VV: map[string]uint64{origin: seq}})
	_, err := s.log.Write(append(b, '\n'))
	return err
}

// Missing lists the chunks versions held here still need.
func (s *Store) Missing() []ChunkRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ChunkRef
	seen := map[ChunkRef]bool{}
	for _, r := range s.recs {
		if r.Ver == nil {
			continue
		}
		t := s.threads[r.Thread]
		for _, c := range r.Ver.Chunks {
			k := ChunkRef{r.Thread, c}
			if _, ok := t.chunks[c]; !ok && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// Stats is what a node says of its shared spaces.
type Stats struct {
	Threads, Records int
	Bytes            int64
}

// Stats counts what is held.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.threads {
		if len(t.idx) > 0 {
			n++
		}
	}
	return Stats{Threads: n, Records: len(s.recs), Bytes: s.bytes}
}

// Close closes the log.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		return nil
	}
	err := s.log.Close()
	s.log = nil
	return err
}
