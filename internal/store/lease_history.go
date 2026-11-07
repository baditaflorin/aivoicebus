package store

// The raw replication log is the authority. This sidecar is a rebuildable index
// into that log, not another copy of the board and not a replicated format.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	bolt "go.etcd.io/bbolt"
)

type LeaseRetentionMode string

const (
	LeaseRetentionOff     LeaseRetentionMode = "off"
	LeaseRetentionObserve LeaseRetentionMode = "observe"
	LeaseRetentionOn      LeaseRetentionMode = "on"
)

type LeaseRetentionOptions struct {
	Mode          LeaseRetentionMode
	Keep          time.Duration
	Hosts         []string
	MaxHotRecords int
	MaxHotBytes   int64
	CacheBytes    int64
}

type LeaseRetentionStatus struct {
	Mode           LeaseRetentionMode
	IndexedRecords uint64
	HotRecords     uint64
	ColdRecords    uint64
	HotBytes       int64
	CacheBytes     int64
	IndexHealthy   bool
}

func WithLeaseRetention(o LeaseRetentionOptions) Option {
	o.Hosts = append([]string(nil), o.Hosts...)
	return func(s *Store) { s.leaseRetention = o }
}

func (s *Store) initLeaseRetention() error {
	o := &s.leaseRetention
	if o.Mode == "" {
		o.Mode = LeaseRetentionOff
	}
	if o.Mode != LeaseRetentionOff && o.Mode != LeaseRetentionObserve {
		return errors.New("lease retention: only off and observe modes are available in this increment")
	}
	if o.Keep == 0 {
		o.Keep = 30 * 24 * time.Hour
	}
	if o.Keep < 30*24*time.Hour {
		return errors.New("lease retention: keep must be at least 30 days")
	}
	if o.MaxHotRecords == 0 {
		o.MaxHotRecords = 4096
	}
	if o.MaxHotBytes == 0 {
		o.MaxHotBytes = 8 << 20
	}
	if o.CacheBytes == 0 {
		o.CacheBytes = 8 << 20
	}
	if o.MaxHotRecords < 1 || o.MaxHotBytes < 1 || o.CacheBytes < 1 {
		return errors.New("lease retention: budgets must be positive")
	}
	if o.Mode == LeaseRetentionOff {
		return nil
	}
	s.historyScope = newHistoryScope()
	dir, err := s.privateHistoryDir()
	if err != nil {
		return err
	}
	s.historyLock, err = lockLeaseHistory(filepath.Join(dir, "lease-history.lock"))
	if err != nil {
		return err
	}
	ready := false
	defer func() {
		if !ready {
			_ = s.historyLock.Close()
			s.historyLock = nil
		}
	}()
	// Only derived, privately owned rebuild files in this tool's namespace are
	// removed, with the exclusive writer lock held. A crash can leave these.
	if err := cleanupHistoryRebuilds(dir); err != nil {
		return err
	}
	if err := checkHistoryPaths(dir); err != nil {
		return err
	}
	// Repair the existing append format before recording byte offsets. This is
	// the same torn-tail recovery Open already performs without the sidecar.
	if err := repairTail(filepath.Join(s.dir, "log.jsonl")); err != nil {
		return err
	}
	if err := s.rebuildLeaseHistory(); err != nil {
		return err
	}
	ready = true
	return nil
}

func (s *Store) LeaseRetentionStats() LeaseRetentionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := LeaseRetentionStatus{Mode: s.leaseRetention.Mode, IndexHealthy: s.historyErr == nil}
	for _, n := range s.logCounts {
		out.IndexedRecords += n
	}
	// Observe deliberately retains every resident view; it is not a memory cap.
	out.HotRecords = out.IndexedRecords
	return out
}

func (s *Store) SweepLeaseRetention() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.historyErr
}

func (s *Store) Close() error {
	s.mu.Lock()
	if s.closeDone == nil {
		s.closeDone = make(chan struct{})
		h, lock, scope := s.history, s.historyLock, s.historyScope
		s.history, s.historyLock = nil, nil
		s.historyErr = errors.New("lease history is closed")
		go func() {
			var err error
			if scope != nil {
				scope.cancel()
			}
			if h != nil {
				err = h.close()
			}
			if scope != nil {
				scope.wg.Wait()
				err = errors.Join(err, scope.result())
			}
			if lock != nil {
				err = errors.Join(err, lock.Close())
			}
			s.mu.Lock()
			s.closeResult = err
			close(s.closeDone)
			s.mu.Unlock()
		}()
	}
	done := s.closeDone
	s.mu.Unlock()
	select {
	case <-done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closeResult
	case <-time.After(5 * time.Second):
		// Keep the writer lock until asynchronous resource finalization is done.
		return errors.New("lease history: close did not finish within five seconds")
	}
}

var historyRecords = []byte("records")
var historyIDs = []byte("ids")

type historyRef struct {
	Offset        int64
	Length        int64
	Digest        [32]byte
	LogicalDigest [32]byte
	Override      json.RawMessage `json:",omitempty"`
}

type leaseHistory struct {
	db        *bolt.DB
	raw       *os.File
	rawPath   string
	rawEnd    int64
	retention LeaseRetentionOptions
	life      historyLife
}

func historyKey(origin string, seq uint64) []byte {
	b := make([]byte, len(origin)+1+8)
	copy(b, origin)
	b[len(origin)] = 0
	binary.BigEndian.PutUint64(b[len(origin)+1:], seq)
	return b
}

func historyExtentDigest(f *os.File, offset, length int64) ([32]byte, error) {
	var out [32]byte
	fi, err := f.Stat()
	if err != nil {
		return out, err
	}
	if offset < 0 || length < 1 || length > fi.Size() || offset > fi.Size()-length {
		return out, errors.New("lease history: raw extent is unavailable")
	}
	hash := sha256.New()
	n, err := io.CopyBuffer(hash, io.NewSectionReader(f, offset, length), make([]byte, 32<<10))
	if err != nil {
		return out, err
	}
	if n != length {
		return out, io.ErrUnexpectedEOF
	}
	copy(out[:], hash.Sum(nil))
	return out, nil
}

func (h *leaseHistory) put(r msg.Record, offset, length int64, raw []byte) error {
	return h.db.Update(func(tx *bolt.Tx) error {
		if err := requireHistoryBuckets(tx); err != nil {
			return err
		}
		return putHistoryRef(tx, h.raw, r, offset, length, raw, h.retention)
	})
}

func putHistoryRef(tx *bolt.Tx, rawFile *os.File, r msg.Record, offset, length int64, raw []byte, retention LeaseRetentionOptions) error {
	ref := historyRef{Offset: offset, Length: length, Digest: sha256.Sum256(raw)}
	if length != int64(len(raw)) {
		var err error
		ref.Digest, err = historyExtentDigest(rawFile, offset, length)
		if err != nil {
			return err
		}
	}
	var decoded msg.Record
	want, _ := json.Marshal(r)
	ref.LogicalDigest = sha256.Sum256(want)
	if len(raw) > maxLine || json.Unmarshal(raw, &decoded) != nil {
		ref.Override = want
	} else if got, _ := json.Marshal(decoded); !bytes.Equal(got, want) {
		// Quarantined and response-only fields must have exactly the same logical
		// feed representation as the existing replay path.
		ref.Override = want
	}
	b, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	key := historyKey(r.Origin, r.Seq)
	if err := tx.Bucket(historyRecords).Put(key, b); err != nil {
		return err
	}
	if r.Msg != nil && tx.Bucket(historyIDs).Get([]byte(r.Msg.ID)) == nil {
		if err := tx.Bucket(historyIDs).Put([]byte(r.Msg.ID), key); err != nil {
			return err
		}
	}
	return putLeaseProjection(tx, r, offset, retention)
}

func (h *leaseHistory) read(origin string, seq uint64) (msg.Record, error) {
	var ref historyRef
	err := h.db.View(func(tx *bolt.Tx) error {
		var err error
		ref, err = historyReference(tx, origin, seq)
		return err
	})
	if err != nil {
		return msg.Record{}, err
	}
	return h.readReference(origin, seq, ref, false)
}

func historyReference(tx *bolt.Tx, origin string, seq uint64) (historyRef, error) {
	var ref historyRef
	bucket := tx.Bucket(historyRecords)
	if bucket == nil {
		return ref, errors.New("lease history: record index is missing")
	}
	b := bucket.Get(historyKey(origin, seq))
	if b == nil {
		return ref, errors.New("lease history: sequence is missing from index")
	}
	err := json.Unmarshal(b, &ref)
	return ref, err
}

func (h *leaseHistory) readReference(origin string, seq uint64, ref historyRef, pinned bool) (msg.Record, error) {
	fdInfo, err := h.raw.Stat()
	if err != nil || !fdInfo.Mode().IsRegular() {
		return msg.Record{}, errors.New("lease history: raw generation changed")
	}
	if !pinned || !h.retired() {
		pathInfo, pathErr := os.Lstat(h.rawPath)
		if pathErr != nil || !pathInfo.Mode().IsRegular() || !historyOwned(pathInfo) || !os.SameFile(fdInfo, pathInfo) {
			if !pinned || !h.retired() {
				return msg.Record{}, errors.New("lease history: raw generation changed")
			}
		}
	}
	b := []byte(ref.Override)
	if len(b) != 0 {
		// A normalized/quarantine/test-lane representation is still tied to
		// the exact raw extent and generation from which it was derived.
		digest, err := historyExtentDigest(h.raw, ref.Offset, ref.Length)
		if err != nil || digest != ref.Digest || sha256.Sum256(b) != ref.LogicalDigest {
			return msg.Record{}, errors.New("lease history: overridden raw authority changed")
		}
	} else {
		if ref.Offset < 0 || ref.Length < 1 || ref.Length > maxLine+1 {
			return msg.Record{}, errors.New("lease history: invalid indexed extent")
		}
		b = make([]byte, int(ref.Length))
		if _, err := h.raw.ReadAt(b, ref.Offset); err != nil {
			return msg.Record{}, fmt.Errorf("lease history: raw read failed: %w", err)
		}
		if sha256.Sum256(b) != ref.Digest {
			return msg.Record{}, errors.New("lease history: raw extent changed")
		}
	}
	var r msg.Record
	if !pinned || !h.retired() {
		if after, err := os.Lstat(h.rawPath); err != nil || !after.Mode().IsRegular() || !os.SameFile(fdInfo, after) {
			if !pinned || !h.retired() {
				return msg.Record{}, errors.New("lease history: raw generation changed during read")
			}
		}
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Origin != origin || r.Seq != seq {
		return msg.Record{}, errors.New("lease history: indexed record identity differs")
	}
	return r, nil
}

func (h *leaseHistory) replace(r msg.Record) error {
	return h.db.Update(func(tx *bolt.Tx) error {
		if err := requireHistoryBuckets(tx); err != nil {
			return err
		}
		bucket := tx.Bucket(historyRecords)
		key := historyKey(r.Origin, r.Seq)
		var ref historyRef
		if b := bucket.Get(key); b == nil {
			return errors.New("lease history: cannot replace missing sequence")
		} else if err := json.Unmarshal(b, &ref); err != nil {
			return err
		}
		ref.Override, _ = json.Marshal(r)
		ref.LogicalDigest = sha256.Sum256(ref.Override)
		b, err := json.Marshal(ref)
		if err != nil {
			return err
		}
		return bucket.Put(key, b)
	})
}

func (s *Store) privateHistoryDir() (string, error) {
	dir, err := filepath.EvalSymlinks(s.dir)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 || !historyOwned(fi) {
		return "", errors.New("lease history: data directory must be private and owned by this server")
	}
	return dir, nil
}

func cleanupHistoryRebuilds(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		suffix, ok := strings.CutPrefix(e.Name(), ".lease-history-rebuild-")
		if !ok || suffix == "" {
			continue
		}
		for _, c := range suffix {
			if c < '0' || c > '9' {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || !historyOwned(fi) {
			return errors.New("lease history: unsafe stale rebuild file")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func checkHistoryPaths(dir string) error {
	for _, name := range []string{"log.jsonl", "lease-history.db"} {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 || !historyOwned(fi) {
			return fmt.Errorf("lease history: unsafe %s path", name)
		}
	}
	return nil
}

func (s *Store) rebuildLeaseHistory() error {
	dir, err := s.privateHistoryDir()
	if err != nil {
		return err
	}
	if err := checkHistoryPaths(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, "lease-history.db")
	if fi, err := os.Lstat(path); err == nil {
		if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 || !historyOwned(fi) {
			return errors.New("lease history: unsafe sidecar path")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	rawPath := filepath.Join(dir, "log.jsonl")
	if fi, err := os.Lstat(rawPath); err == nil {
		if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 || !historyOwned(fi) {
			return errors.New("lease history: unsafe raw log path")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	raw, err := os.OpenFile(rawPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if fi, err := raw.Stat(); err != nil || !fi.Mode().IsRegular() {
		raw.Close()
		return errors.New("lease history: raw log must be regular")
	}
	if err := raw.Sync(); err != nil {
		raw.Close()
		return err
	}
	tmp, err := os.CreateTemp(dir, ".lease-history-rebuild-*")
	if err != nil {
		raw.Close()
		return err
	}
	name := tmp.Name()
	tmp.Close()
	defer os.Remove(name)
	db, err := bolt.Open(name, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		raw.Close()
		return err
	}
	h := &leaseHistory{db: db, raw: raw, rawPath: filepath.Join(dir, "log.jsonl"), retention: s.leaseRetention, life: historyLife{done: make(chan struct{})}}
	keep := false
	defer func() {
		if !keep {
			h.close()
		}
	}()
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range requiredHistoryBuckets {
			if _, err := tx.CreateBucket(bucket); err != nil {
				return err
			}
		}
		value, err := json.Marshal(s.now().UTC())
		if err != nil {
			return err
		}
		if err := tx.Bucket(historyLeaseWindow).Put(historyWindowFloor, value); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	// A count per origin is the version vector, not a per-record heap index.
	vetter := &Store{logCounts: map[string]uint64{}}
	var buildErr error
	batch, err := db.Begin(true)
	if err != nil {
		return err
	}
	defer func() {
		if batch != nil {
			_ = batch.Rollback()
		}
	}()
	batchN := 0
	err = readLinesAt(filepath.Join(dir, "log.jsonl"), func(line []byte, over bool, offset, length int64) {
		h.rawEnd = offset + length
		if buildErr != nil {
			return
		}
		var r msg.Record
		if over {
			var ok bool
			if r, ok = headRecord(line); !ok {
				return
			}
		} else if json.Unmarshal(line, &r) != nil {
			return
		}
		if r, ok := vetter.vetLocked(r); ok {
			buildErr = putHistoryRef(batch, raw, r, offset, length, line, s.leaseRetention)
			vetter.logCounts[r.Origin]++
			batchN++
			if buildErr == nil && batchN == 256 {
				buildErr = batch.Commit()
				batch = nil
				if buildErr == nil {
					batch, buildErr = db.Begin(true)
				}
				batchN = 0
			}
		}
	})
	if err != nil {
		return err
	}
	if buildErr != nil {
		return buildErr
	}
	if fi, err := raw.Stat(); err != nil || fi.Size() != h.rawEnd {
		return errors.New("lease history: raw size changed during indexing")
	}
	if err := batch.Commit(); err != nil {
		return err
	}
	batch = nil
	if err := db.Sync(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	fd, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = errors.Join(fd.Sync(), fd.Close())
	if err != nil {
		return err
	}
	old := s.history
	h.bindScope(s.historyScope)
	s.history, s.historyErr = h, nil
	keep = true
	if old != nil {
		return old.close()
	}
	return nil
}

func (s *Store) logCount(origin string) uint64 { return s.logCounts[origin] }

func (s *Store) logRecord(origin string, seq uint64) (msg.Record, error) {
	if s.historyErr != nil {
		return msg.Record{}, s.historyErr
	}
	if s.history != nil {
		r, err := s.history.read(origin, seq)
		if err != nil {
			s.historyErr = err
		}
		return r, err
	}
	if seq == 0 || seq > uint64(len(s.log[origin])) {
		return msg.Record{}, errors.New("log sequence missing")
	}
	return s.log[origin][seq-1], nil
}

func (s *Store) replaceLogRecord(r msg.Record) error {
	if s.history != nil {
		return s.history.replace(r)
	}
	if r.Seq == 0 || r.Seq > uint64(len(s.log[r.Origin])) {
		return errors.New("log replacement sequence missing")
	}
	s.log[r.Origin][r.Seq-1] = r
	return nil
}

func (s *Store) appendRecord(r msg.Record) error {
	if s.historyErr != nil {
		return s.historyErr
	}
	path := filepath.Join(s.dir, "log.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if s.history != nil {
		was, err := s.history.raw.Stat()
		if err != nil || !os.SameFile(was, fi) {
			return errors.New("lease history: raw generation changed before append")
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if n, err := f.Write(b); err != nil {
		s.historyErr = err
		return err
	} else if n != len(b) {
		s.historyErr = io.ErrShortWrite
		return io.ErrShortWrite
	}
	// The derived sidecar must never get ahead of an acknowledged durable raw
	// record. A failed post-append durability/index step requires recovery;
	// another request must not append the same origin sequence again.
	if s.history != nil {
		if err := f.Sync(); err != nil {
			s.historyErr = err
			return err
		}
		if fi.Size() == 0 {
			d, err := os.Open(filepath.Dir(path))
			if err == nil {
				err = errors.Join(d.Sync(), d.Close())
			}
			if err != nil {
				s.historyErr = err
				return err
			}
		}
	}
	if s.history != nil {
		if err := s.history.put(r, fi.Size(), int64(len(b)), b); err != nil {
			s.historyErr = err // raw remains authoritative; do not acknowledge an unindexed append
			return err
		}
		s.history.rawEnd = fi.Size() + int64(len(b))
	}
	return nil
}
