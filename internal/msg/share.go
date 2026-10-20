package msg

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Shared spaces. A thread can have a place for files, diffs and a notes page that its
// participants keep for days: the nodes hold it encrypted, with versions, and replicate it to each
// other. A node never sees a name, a byte of content or a key: the writer's machine encrypts every
// chunk and the metadata with the thread's key, and that key travels only sealed for each
// participant machine's box key (see the share package). What a node knows is which thread, an
// opaque name id, sizes, version numbers and who wrote.
//
// A node keeps them in a log of its own (not the message log), so a node from before shared spaces
// sees nothing it could quarantine: it answers 404 to /v1/share/..., and a client that finds no
// FeatureShare on its node says so and changes nothing.

// FeatureShare is the node feature for /v1/share/...
const FeatureShare = "share"

// ShareSince is the first release whose nodes hold shared spaces.
const ShareSince = "0.12.0"

// Limits of a shared space. A node may be started with lower ones (tests).
const (
	ShareChunk      = 4 << 20                       // plaintext bytes in one chunk
	ShareChunkMax   = ShareChunk + 64               // a sealed chunk: magic, nonce, tag and room
	ShareThreadMax  = 256 << 20                     // all versions of everything in one thread, sealed
	ShareNodeMax    = 2 << 30                       // everything a node holds
	ShareTTL        = 7 * 24 * time.Hour            // after the thread's last message or write
	ShareMetaMax    = 4096                          // the sealed metadata of one version, base64
	ShareSealedMax  = 512                           // a sealed key, base64
	ShareMaxRecords = 4000                          // versions and keys in one thread
	ShareMaxChunks  = ShareThreadMax/ShareChunk + 1 // chunks in one version
	SharePendingTTL = time.Hour                     // a chunk uploaded but never used by a version
)

// ShareChunkMagic starts every sealed chunk, so a node holds only what a client sealed, never a
// plaintext file sent by mistake.
const ShareChunkMagic = "AVBS1"

// ShareRecord is one entry of a node's share log, replicated between nodes like a message record:
// Origin is the node that took it, Seq its place in that node's numbering, At when it took it.
type ShareRecord struct {
	Origin string        `json:"origin"`
	Seq    uint64        `json:"seq"`
	At     time.Time     `json:"at"`
	Thread string        `json:"thread"`
	Ver    *ShareVersion `json:"ver,omitempty"`
	Key    *ShareKey     `json:"key,omitempty"`
}

// ShareVersion is one version of one name in a thread's space.
type ShareVersion struct {
	ID     string   `json:"id"`             // 32 hex, chosen by the writer
	Name   string   `json:"name"`           // 32 hex: a keyed hash of the name, opaque to nodes
	Num    int      `json:"num"`            // 1, 2, ...: set by the node that took it
	Base   int      `json:"base,omitempty"` // the version this one edits; a node refuses it unless that is still the latest
	Notes  bool     `json:"notes,omitempty"`
	KeyID  string   `json:"kid"`    // 16 hex: which thread key sealed it
	Chunks []string `json:"chunks"` // sha256 (hex) of each sealed chunk, in order
	Size   int64    `json:"size"`   // the sealed chunks' bytes: set by the node
	Meta   string   `json:"meta"`   // base64 of the sealed metadata (name, size, hash, kind)
	By     string   `json:"by"`     // the writer's identity
	SigKey string   `json:"sig_key,omitempty"`
	Sig    string   `json:"sig,omitempty"`
}

// ShareKey is a thread key sealed for one machine.
type ShareKey struct {
	ID     string `json:"id"`     // 16 hex: the key's id (a hash of it)
	For    string `json:"for"`    // the machine it is sealed for
	Box    string `json:"box"`    // that machine's box key it was sealed to, base64
	Sealed string `json:"sealed"` // base64
	By     string `json:"by"`     // who sealed it
	SigKey string `json:"sig_key,omitempty"`
	Sig    string `json:"sig,omitempty"`
}

// ShareIndex is what a node holds of one thread's space: every record whose chunks it has.
type ShareIndex struct {
	Thread  string        `json:"thread"`
	Expires time.Time     `json:"expires"` // when the node drops it, unless the thread goes on
	Bytes   int64         `json:"bytes"`
	Max     int64         `json:"max"` // the per-thread cap
	Records []ShareRecord `json:"records"`
}

// SharePut asks a node to take a version (POST /v1/share/put).
type SharePut struct {
	Thread string       `json:"thread"`
	Ver    ShareVersion `json:"ver"`
}

// ShareKeyPut asks a node to keep a sealed key (POST /v1/share/key).
type ShareKeyPut struct {
	Thread string   `json:"thread"`
	Key    ShareKey `json:"key"`
}

// ShareChunkPut carries one sealed chunk (POST /v1/share/chunk?thread=).
type ShareChunkPut struct {
	Data []byte `json:"data"`
}

// ShareChunkDone is a node's answer to a chunk: its hash.
type ShareChunkDone struct {
	SHA256 string `json:"sha256"`
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == lowerASCII(s)
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// ValidShareChunkID reports whether s is a chunk's sha256 in lowercase hex.
func ValidShareChunkID(s string) bool { return isHex(s, 64) }

// ValidShareThread reports whether s names a thread (a message id).
func ValidShareThread(s string) bool { return ValidID(s) && s == lowerASCII(s) }

func validB64(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	_, err := base64.StdEncoding.Strict().DecodeString(s)
	return err == nil
}

// Validate checks a version's shape (not its signature, which a reader judges).
func (v ShareVersion) Validate() error {
	switch {
	case !isHex(v.ID, 32):
		return errors.New("bad request: a version id is 32 lowercase hex digits; this aivoicebus and the node may be different releases: run `aivoicebus update`, then try again")
	case !isHex(v.Name, 32):
		return errors.New("bad request: a name id is 32 lowercase hex digits; run `aivoicebus update`, then try again")
	case !isHex(v.KeyID, 16):
		return errors.New("bad request: a key id is 16 lowercase hex digits; run `aivoicebus update`, then try again")
	case v.Base < 0 || v.Num < 0:
		return errors.New("bad request: a version number is never negative; run `aivoicebus update`, then try again")
	case len(v.Chunks) == 0 || len(v.Chunks) > ShareMaxChunks:
		return fmt.Errorf("bad request: a version has 1 to %d chunks; run `aivoicebus update`, then try again", ShareMaxChunks)
	case !validB64(v.Meta, ShareMetaMax):
		return errors.New("bad request: the metadata must be base64 of at most 4096 characters; run `aivoicebus update`, then try again")
	case !ValidIdentity(v.By) || HostOf(v.By) == "":
		return errors.New("bad request: by must be an identity (host/agent); run `aivoicebus update`, then try again")
	case len(v.SigKey) > 64 || len(v.Sig) > 128:
		return errors.New("bad request: a signature or key that long is not one; run `aivoicebus update`, then try again")
	}
	for _, c := range v.Chunks {
		if !ValidShareChunkID(c) {
			return errors.New("bad request: a chunk is named by its sha256 in lowercase hex; run `aivoicebus update`, then try again")
		}
	}
	return nil
}

// Validate checks a sealed key's shape.
func (k ShareKey) Validate() error {
	switch {
	case !isHex(k.ID, 16):
		return errors.New("bad request: a key id is 16 lowercase hex digits; run `aivoicebus update`, then try again")
	case k.For == "" || len(k.For) > 64 || HostOf(k.For) != k.For || hasControl(k.For):
		return errors.New("bad request: for must name one machine; run `aivoicebus update`, then try again")
	case !validB64(k.Box, 64) || !validB64(k.Sealed, ShareSealedMax):
		return errors.New("bad request: box and sealed must be base64; run `aivoicebus update`, then try again")
	case !ValidIdentity(k.By) || HostOf(k.By) == "":
		return errors.New("bad request: by must be an identity (host/agent); run `aivoicebus update`, then try again")
	case len(k.SigKey) > 64 || len(k.Sig) > 128:
		return errors.New("bad request: a signature or key that long is not one; run `aivoicebus update`, then try again")
	}
	return nil
}

// Validate checks a replicated record's shape.
func (r ShareRecord) Validate() error {
	switch {
	case r.Origin == "" || len(r.Origin) > 120 || hasControl(r.Origin) || r.Seq == 0:
		return errors.New("a share record names its origin node and sequence")
	case !ValidShareThread(r.Thread):
		return errors.New("a share record names its thread")
	case (r.Ver == nil) == (r.Key == nil):
		return errors.New("a share record is a version or a key")
	case r.Ver != nil:
		if r.Ver.Num < 1 {
			return errors.New("a replicated version has its number")
		}
		return r.Ver.Validate()
	}
	return r.Key.Validate()
}

const shareSigDomain = "aivoicebus-share-v1\n"

// SignedBytes is what a version's signature covers: everything its writer decides.
func (v ShareVersion) SignedBytes(thread string) []byte {
	b := []byte(shareSigDomain + "ver\n" + thread + "\n" + v.ID + "\n" + v.Name + "\n" + fmt.Sprint(v.Base) + "\n" +
		fmt.Sprint(v.Notes) + "\n" + v.KeyID + "\n" + v.By + "\n" + v.SigKey + "\n" + v.Meta + "\n")
	for _, c := range v.Chunks {
		b = append(b, c...)
		b = append(b, '\n')
	}
	return b
}

// SignedBytes is what a sealed key's signature covers.
func (k ShareKey) SignedBytes(thread string) []byte {
	return []byte(shareSigDomain + "key\n" + thread + "\n" + k.ID + "\n" + k.For + "\n" + k.Box + "\n" + k.Sealed + "\n" + k.By + "\n" + k.SigKey + "\n")
}
