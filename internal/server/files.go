package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A node can hold a file for a few minutes so a message can point at it without a separate
// file service. The node never sees the file: the sender's machine encrypts it (the pipe's
// envelope, see the attach package) and the key travels only in the message's link. Files live
// in memory, are never replicated or written to disk, and go when read (unless the sender kept
// them), when they expire, or when the node restarts. Only token holders may store or fetch one,
// and the observer token may do neither: it is read-only, and a read can burn a file.
const (
	// MaxFileEnvelope is the largest envelope a node takes: a 7 MiB file once encrypted and
	// base64-encoded, with room for the envelope header.
	MaxFileEnvelope = 10<<20 + 1024
	maxFileTTL      = 5 * time.Minute
	maxFilesBytes   = 64 << 20 // all files a node holds at once
	maxFiles        = 256
	fileWatchWait   = 25 * time.Second
	fileUploadTime  = 10 * time.Minute // a 10 MB upload over a slow link; the node's usual read timeout is 30 s
)

var reFileID = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

type heldFile struct {
	data    []byte // nil once read with burn
	burn    bool
	expires time.Time
	read    chan struct{} // closed on the first read
}

// fileBox is the node's in-memory file store.
type fileBox struct {
	mu    sync.Mutex
	files map[string]*heldFile
	bytes int64
	now   func() time.Time
}

func newFileBox() *fileBox { return &fileBox{files: map[string]*heldFile{}, now: time.Now} }

// sweep drops expired entries. The caller holds mu.
func (b *fileBox) sweep() {
	now := b.now()
	for id, f := range b.files {
		if now.After(f.expires) {
			b.bytes -= int64(len(f.data))
			delete(b.files, id)
		}
	}
}

// errFull is a node that holds as many files as it may.
type errFull struct{}

func (errFull) Error() string {
	return "this node holds as many files as it may; try again in a few minutes, or send through the pipe (--via pipe)"
}

func (b *fileBox) put(data []byte, ttl time.Duration, burn bool) (string, time.Time, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep()
	if len(b.files) >= maxFiles || b.bytes+int64(len(data)) > maxFilesBytes {
		return "", time.Time{}, errFull{}
	}
	exp := b.now().Add(ttl)
	b.files[id] = &heldFile{data: data, burn: burn, expires: exp, read: make(chan struct{})}
	b.bytes += int64(len(data))
	return id, exp, nil
}

// get returns the file and marks it read; a burn file is gone after this.
func (b *fileBox) get(id string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep()
	f, ok := b.files[id]
	if !ok || f.data == nil {
		return nil, false
	}
	data := f.data
	select {
	case <-f.read:
	default:
		close(f.read)
	}
	if f.burn { // the entry stays, without the data, so the sender can still learn it was read
		b.bytes -= int64(len(f.data))
		f.data = nil
	}
	return data, true
}

// watch waits up to wait for the first read. found is false for an entry that is not there
// (expired, never stored).
func (b *fileBox) watch(ctx context.Context, id string, wait time.Duration) (read, found bool) {
	b.mu.Lock()
	b.sweep()
	f, ok := b.files[id]
	b.mu.Unlock()
	if !ok {
		return false, false
	}
	t := time.NewTimer(min(wait, time.Until(f.expires)))
	defer t.Stop()
	select {
	case <-f.read:
		return true, true
	case <-t.C:
	case <-ctx.Done():
	}
	select {
	case <-f.read:
		return true, true
	default:
		return false, true
	}
}

// looksSealed reports whether body has the shape of the pipe's envelope (base64 of a version
// byte, a 16-byte IV, a 32-byte MAC and at least one byte), so a node holds only ciphertext
// someone sealed, never a plaintext file sent by mistake.
func looksSealed(body []byte) bool {
	s := strings.TrimSpace(string(body))
	if len(s) < base64.StdEncoding.EncodedLen(50) {
		return false
	}
	head, err := base64.StdEncoding.DecodeString(s[:68]) // 68 characters: the first 51 bytes
	if err != nil || head[0] != 1 {
		return false
	}
	_, err = base64.StdEncoding.DecodeString(s)
	return err == nil
}

// fileRoutes adds /v1/files (store) and /v1/files/ID (fetch) and /v1/files/ID/read (has it been read).
func fileRoutes(mux *http.ServeMux, box *fileBox, upload, agent func(principalHandler) http.HandlerFunc) {
	js := func(w http.ResponseWriter, code int, v string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		w.Write([]byte(v + "\n"))
	}
	mux.HandleFunc("/v1/files", upload(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(fileUploadTime))
		ttl := maxFileTTL
		if v, err := strconv.Atoi(r.URL.Query().Get("ttl")); err == nil && v > 0 && time.Duration(v)*time.Second < ttl {
			ttl = time.Duration(v) * time.Second
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "the file is larger than a node takes (7 MiB before encryption)", http.StatusRequestEntityTooLarge)
			return
		}
		if !looksSealed(body) {
			http.Error(w, "bad request: a node holds only an encrypted envelope (aivoicebus send --file makes one)", http.StatusBadRequest)
			return
		}
		id, exp, err := box.put(body, ttl, r.URL.Query().Get("burn") == "1")
		if err != nil {
			if _, full := err.(errFull); full {
				w.Header().Set("Retry-After", "60")
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		js(w, http.StatusCreated, `{"id":"`+id+`","expires":"`+exp.UTC().Format(time.RFC3339)+`"}`)
	}))
	mux.HandleFunc("/v1/files/", agent(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		rest := strings.TrimPrefix(r.URL.Path, "/v1/files/")
		id, sub, _ := strings.Cut(rest, "/")
		if r.Method != http.MethodGet || !reFileID.MatchString(id) || (sub != "" && sub != "read") {
			http.NotFound(w, r)
			return
		}
		if sub == "read" {
			read, found := box.watch(r.Context(), id, fileWatchWait)
			if !found {
				http.NotFound(w, r)
				return
			}
			js(w, http.StatusOK, `{"read":`+strconv.FormatBool(read)+`}`)
			return
		}
		data, ok := box.get(id)
		if !ok {
			http.Error(w, "gone: it expired or was already read", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "no-store")
		longWrite(w)
		w.Write(data)
	}))
}
