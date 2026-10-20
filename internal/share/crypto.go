package share

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/attach"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// The thread key K (32 random bytes) never reaches a node. Everything else is derived from it:
//
//	key id    sha256("aivoicebus-share-kid-v1" | K), first 8 bytes, hex
//	name id   HMAC-SHA256(HKDF(K, "aivoicebus-share-name-v1"), kind "\n" name), first 16 bytes, hex
//	a chunk   AES-256-GCM with HKDF(K, salt version id, "aivoicebus-share-chunk-v1"),
//	          a random nonce, bound to thread, version id, its index and the chunk count:
//	          "AVBS1" | nonce (12) | ciphertext
//	metadata  the same with "aivoicebus-share-meta-v1", bound to thread and version id, base64
//
// K travels sealed for each participant machine's box key, the way a file link's key is sealed
// (attach.SealBytes), under its own label and bound to the thread, the key id and the machine.

const (
	keyInfo   = "aivoicebus-sharekey-v1"
	nameInfo  = "aivoicebus-share-name-v1"
	chunkInfo = "aivoicebus-share-chunk-v1"
	metaInfo  = "aivoicebus-share-meta-v1"
)

// Kinds of things in a space.
const (
	KindFile  = "file"
	KindDiff  = "diff"
	KindNotes = "notes"
)

// meta is what a version says of itself, sealed.
type meta struct {
	Name   string    `json:"name"`
	Kind   string    `json:"kind"`
	Size   int64     `json:"size"`
	SHA256 string    `json:"sha256"`
	At     time.Time `json:"at"` // the writer's clock
}

var errOpen = errors.New("does not open with the thread's key") // nextstep: callers wrap it with what to do

func newKey() ([]byte, error) {
	k := make([]byte, 32)
	_, err := rand.Read(k)
	return k, err
}

func keyID(k []byte) string {
	h := sha256.Sum256(append([]byte("aivoicebus-share-kid-v1"), k...))
	return hex.EncodeToString(h[:8])
}

func newVersionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// nameKind is the key a name and kind are found under: notes and a file called notes differ.
func nameKind(kind, name string) string {
	if kind == KindDiff {
		kind = KindFile // a diff is a file whose text came from git
	}
	return kind + "\n" + name
}

func nameID(k []byte, kind, name string) string {
	nk, _ := hkdf.Key(sha256.New, k, nil, nameInfo, 32)
	m := hmac.New(sha256.New, nk)
	m.Write([]byte(nameKind(kind, name)))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

func gcmFor(k []byte, vid, info string) (cipher.AEAD, error) {
	salt, err := hex.DecodeString(vid)
	if err != nil {
		return nil, err
	}
	dk, err := hkdf.Key(sha256.New, k, salt, info, 32)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(dk)
	return cipher.NewGCM(block)
}

func chunkAAD(thread, vid string, i, n int) []byte {
	b := []byte("chunk\n" + thread + "\n" + vid + "\n")
	b = binary.BigEndian.AppendUint32(b, uint32(i))
	return binary.BigEndian.AppendUint32(b, uint32(n))
}

func sealChunk(k []byte, thread, vid string, i, n int, pt []byte) ([]byte, error) {
	g, err := gcmFor(k, vid, chunkInfo)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte(msg.ShareChunkMagic), nonce...)
	return g.Seal(out, nonce, pt, chunkAAD(thread, vid, i, n)), nil
}

func openChunk(k []byte, thread, vid string, i, n int, sealed []byte) ([]byte, error) {
	g, err := gcmFor(k, vid, chunkInfo)
	if err != nil {
		return nil, err
	}
	head := len(msg.ShareChunkMagic) + g.NonceSize()
	if len(sealed) < head+g.Overhead() || string(sealed[:len(msg.ShareChunkMagic)]) != msg.ShareChunkMagic {
		return nil, errOpen
	}
	pt, err := g.Open(nil, sealed[len(msg.ShareChunkMagic):head], sealed[head:], chunkAAD(thread, vid, i, n))
	if err != nil {
		return nil, errOpen
	}
	return pt, nil
}

func sealMeta(k []byte, thread, vid string, m meta) (string, error) {
	g, err := gcmFor(k, vid, metaInfo)
	if err != nil {
		return "", err
	}
	pt, _ := json.Marshal(m)
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(g.Seal(nonce, nonce, pt, []byte("meta\n"+thread+"\n"+vid))), nil
}

func openMeta(k []byte, thread, vid, sealed string) (meta, error) {
	var m meta
	g, err := gcmFor(k, vid, metaInfo)
	if err != nil {
		return m, err
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < g.NonceSize()+g.Overhead() {
		return m, errOpen
	}
	pt, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], []byte("meta\n"+thread+"\n"+vid))
	if err != nil || json.Unmarshal(pt, &m) != nil {
		return m, errOpen
	}
	return m, nil
}

func keyAAD(thread, kid, host string) []byte {
	return []byte("sharekey\n" + thread + "\n" + kid + "\n" + host)
}

// sealKeyFor seals K for host's box key.
func sealKeyFor(k []byte, thread, host string, box []byte) (string, error) {
	raw, err := attach.SealBytes(k, keyAAD(thread, keyID(k), host), box, keyInfo)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// openKey opens a sealed key record with this machine's box private key, and checks it is the key
// the record names.
func openKey(r msg.ShareKey, thread string, boxPriv []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(r.Sealed)
	if err != nil {
		return nil, errOpen
	}
	k, err := attach.OpenBytes(raw, keyAAD(thread, r.ID, r.For), boxPriv, keyInfo)
	if err != nil || len(k) != 32 || keyID(k) != r.ID {
		return nil, errOpen
	}
	return k, nil
}
