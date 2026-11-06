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
	// Repair the existing append format before recording byte offsets. This is
	// the same torn-tail recovery Open already performs without the sidecar.
	if err := repairTail(filepath.Join(s.dir, "log.jsonl")); err != nil {
		return err
	}
	return s.rebuildLeaseHistory()
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
	defer s.mu.Unlock()
	if s.history == nil {
		return nil
	}
	err := s.history.close()
	s.history = nil
	s.historyErr = errors.New("lease history is closed")
	return err
}

var historyRecords = []byte("records")
var historyIDs = []byte("ids")

type historyRef struct {
	Offset   int64
	Length   int64
	Digest   [32]byte
	Override json.RawMessage `json:",omitempty"`
}

type leaseHistory struct {
	db  *bolt.DB
	raw *os.File
}

func historyKey(origin string, seq uint64) []byte {
	b := make([]byte, len(origin)+1+8)
	copy(b, origin)
	b[len(origin)] = 0
	binary.BigEndian.PutUint64(b[len(origin)+1:], seq)
	return b
}

func (h *leaseHistory) close() error { return errors.Join(h.db.Close(), h.raw.Close()) }

func (h *leaseHistory) put(r msg.Record, offset, length int64, raw []byte) error {
	return h.db.Update(func(tx *bolt.Tx) error { return putHistoryRef(tx, r, offset, length, raw) })
}

func putHistoryRef(tx *bolt.Tx, r msg.Record, offset, length int64, raw []byte) error {
	ref := historyRef{Offset: offset, Length: length, Digest: sha256.Sum256(raw)}
	var decoded msg.Record
	want, _ := json.Marshal(r)
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
		return tx.Bucket(historyIDs).Put([]byte(r.Msg.ID), key)
	}
	return nil
}

func (h *leaseHistory) read(origin string, seq uint64) (msg.Record, error) {
	var ref historyRef
	err := h.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(historyRecords).Get(historyKey(origin, seq))
		if b == nil {
			return errors.New("lease history: sequence is missing from index")
		}
		return json.Unmarshal(b, &ref)
	})
	if err != nil {
		return msg.Record{}, err
	}
	b := []byte(ref.Override)
	if len(b) == 0 {
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
	if err := json.Unmarshal(b, &r); err != nil || r.Origin != origin || r.Seq != seq {
		return msg.Record{}, errors.New("lease history: indexed record identity differs")
	}
	return r, nil
}

func (h *leaseHistory) replace(r msg.Record) error {
	return h.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(historyRecords)
		key := historyKey(r.Origin, r.Seq)
		var ref historyRef
		if b := bucket.Get(key); b == nil {
			return errors.New("lease history: cannot replace missing sequence")
		} else if err := json.Unmarshal(b, &ref); err != nil {
			return err
		}
		ref.Override, _ = json.Marshal(r)
		b, err := json.Marshal(ref)
		if err != nil {
			return err
		}
		return bucket.Put(key, b)
	})
}

func (s *Store) rebuildLeaseHistory() error {
	dir, err := filepath.EvalSymlinks(s.dir)
	if err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return errors.New("lease history: data directory must be private")
	}
	path := filepath.Join(dir, "lease-history.db")
	if fi, err := os.Lstat(path); err == nil {
		if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
			return errors.New("lease history: unsafe sidecar path")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	raw, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if fi, err := raw.Stat(); err != nil || !fi.Mode().IsRegular() {
		raw.Close()
		return errors.New("lease history: raw log must be regular")
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
	h := &leaseHistory{db: db, raw: raw}
	keep := false
	defer func() {
		if !keep {
			h.close()
		}
	}()
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucket(historyRecords); err != nil {
			return err
		}
		_, err := tx.CreateBucket(historyIDs)
		return err
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
			buildErr = putHistoryRef(batch, r, offset, length, line)
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
		return err
	} else if n != len(b) {
		return io.ErrShortWrite
	}
	if s.history != nil {
		if err := s.history.put(r, fi.Size(), int64(len(b)), b); err != nil {
			s.historyErr = err // raw remains authoritative; do not acknowledge an unindexed append
			return err
		}
	}
	return nil
}
