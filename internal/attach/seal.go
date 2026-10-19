package attach

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

// Sealed keys. A file link carries the key that decrypts the file, so anyone who can read the
// board could fetch the file once while it lives. When the recipient is one machine whose box key
// is known (the X25519 key every signed message carries, covered by its signature), the sender
// seals the link for that machine instead: the [attach] line keeps the link without its key and
// adds sealed=, which only that machine can open.
//
//	sealed=v1.base64url( ephemeral X25519 public key (32) | nonce (12) | AES-256-GCM(the whole link) )
//
// The AES key is HKDF-SHA256 over the X25519 shared secret, salted with the ephemeral and the
// recipient's public keys, under its own label (a seal is never also a sealed message). The link
// without its key is the additional data, so a seal cannot be moved to another file, and the
// opened link must be that same link with a key.

const (
	sealPrefix = "v1."
	sealInfo   = "aivoicebus-filekey-v1"
	// SealedNote starts the line that tells a reader too old to open a seal what it is looking at.
	// A reader that can open it drops the line.
	SealedNote = "[sealed file]"
)

var (
	reSeal    = regexp.MustCompile(`^v1\.[A-Za-z0-9_-]{80,1000}$`)
	reHexKey  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	rePipeID  = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)
	reForHost = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// ErrSealedElsewhere is a sealed key this machine cannot open.
var ErrSealedElsewhere = errors.New("the file's key is sealed for another machine's key, or the seal was altered: this machine cannot open it; nothing was fetched")

// Keyless is the link without its key: a node's https://NODE/v1/files/ID#KEY becomes
// https://NODE/v1/files/ID, the pipe's https://PIPE/#ID~KEY becomes https://PIPE/#ID. ok is false
// for a link that carries no key this package knows.
func Keyless(link string) (string, bool) {
	i := strings.LastIndexByte(link, '#')
	if i < 0 {
		return "", false
	}
	frag := link[i+1:]
	if reHexKey.MatchString(frag) {
		return link[:i], true
	}
	if m := reFrag.FindStringSubmatch(frag); m != nil {
		return link[:i+1] + m[1], true
	}
	return "", false
}

// keyShaped is a keyless link with a stand-in key, so a provider can say whether the link is one
// of its own without the key.
func keyShaped(keyless string) string {
	zero := strings.Repeat("0", 64)
	i := strings.LastIndexByte(keyless, '#')
	switch {
	case i < 0:
		return keyless + "#" + zero
	case rePipeID.MatchString(keyless[i+1:]):
		return keyless + "~" + zero
	}
	return keyless
}

func sealKey(shared, eph, to []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, shared, append(append([]byte{}, eph...), to...), sealInfo, 32)
}

// SealFor returns l with its key sealed for the machine whose X25519 box key is to; host names
// that machine for the reader (display only).
func SealFor(l Link, to []byte, host string) (Link, error) {
	keyless, ok := Keyless(l.URL)
	if !ok {
		return Link{}, errors.New("the link carries no key to seal")
	}
	if host != "" && !reForHost.MatchString(host) {
		return Link{}, fmt.Errorf("%q is not a machine name", host)
	}
	pub, err := ecdh.X25519().NewPublicKey(to)
	if err != nil {
		return Link{}, errors.New("not an X25519 public key")
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Link{}, err
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return Link{}, err
	}
	k, err := sealKey(shared, eph.PublicKey().Bytes(), to)
	if err != nil {
		return Link{}, err
	}
	block, _ := aes.NewCipher(k)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Link{}, err
	}
	raw := append(append(eph.PublicKey().Bytes(), nonce...), gcm.Seal(nil, nonce, []byte(l.URL), []byte(keyless))...)
	l.URL, l.Sealed, l.For = keyless, sealPrefix+base64.RawURLEncoding.EncodeToString(raw), host
	return l, nil
}

// OpenWith returns l with the key a sealed link carries, opened with the X25519 private key priv.
// A link that is not sealed is returned as it is.
func OpenWith(l Link, priv []byte) (Link, error) {
	if l.Sealed == "" {
		return l, nil
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(l.Sealed, sealPrefix)) // strict: no two texts carry one seal
	if !reSeal.MatchString(l.Sealed) || err != nil || len(raw) < 32+12+16 {
		return Link{}, errors.New("the sealed key is not one this release can read")
	}
	key, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return Link{}, errors.New("not an X25519 private key")
	}
	ephPub, err := ecdh.X25519().NewPublicKey(raw[:32])
	if err != nil {
		return Link{}, ErrSealedElsewhere
	}
	shared, err := key.ECDH(ephPub)
	if err != nil {
		return Link{}, ErrSealedElsewhere
	}
	k, err := sealKey(shared, raw[:32], key.PublicKey().Bytes())
	if err != nil {
		return Link{}, err
	}
	block, _ := aes.NewCipher(k)
	gcm, _ := cipher.NewGCM(block)
	pt, err := gcm.Open(nil, raw[32:44], raw[44:], []byte(l.URL))
	if err != nil {
		return Link{}, ErrSealedElsewhere
	}
	if kl, ok := Keyless(string(pt)); !ok || kl != l.URL {
		return Link{}, ErrSealedElsewhere
	}
	l.URL, l.Sealed = string(pt), ""
	return l, nil
}

// Unseal opens a sealed link with this machine's box key; a link that is not sealed is returned
// as it is.
func Unseal(l Link) (Link, error) {
	if l.Sealed == "" {
		return l, nil
	}
	_, box, err := config.MachineKeys()
	if err != nil {
		return Link{}, fmt.Errorf("the file's key is sealed for this machine's key, which cannot be read: %v", err)
	}
	return OpenWith(l, box)
}
