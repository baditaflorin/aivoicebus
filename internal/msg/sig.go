package msg

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// Signed senders. A message's From is what its client says; a signature says it came from a
// machine that holds a key, and a reader that has seen that machine's key before (pinned the first
// time, like ssh's known_hosts) can tell a message from that machine from one that only claims to
// be. Every session on a machine signs with the machine's ed25519 key. The signature covers what
// the sender decides and no node changes: the id, sender, target, reply-to, kind, priority, body
// and the sender's two public keys. Thread and ttl are left out (a node fills the thread in and
// turns the ttl into an expiry), as are the title and model (cleaned by every node).
//
// Private messages. A message can be sealed to the recipient machine's X25519 key: the body is
// then "[sealed v1] " and base64 of an ephemeral public key, a nonce and AES-256-GCM ciphertext,
// which nodes store and replicate like any text and only that machine can open.

// SignSince is the first release whose nodes keep a message's key, box and sig. An older node drops
// fields it does not know, and the copy it stores and relays would differ from everyone else's for
// good, so a client signs only when every node it can see runs this release or later: during a
// rolling upgrade messages go unsigned, and once the last node is updated they are all signed.
const SignSince = "0.7.0"

// Verification results a client sets on a message it read (response only: never stored).
const (
	VerifiedSigned   = "signed"   // a valid signature by the key pinned for the sender's machine (or seen for the first time)
	VerifiedUnsigned = "unsigned" // no signature (a client older than 0.7.0, or a system notice)
	VerifiedBad      = "bad"      // a signature that does not verify: altered, or not what it says
	VerifiedChanged  = "changed"  // a valid signature by a key other than the one pinned for that machine
)

const sigDomain = "aivoicebus-msg-v1\n"

// SignedBytes is what a message's signature covers.
func (m Message) SignedBytes() []byte {
	var b strings.Builder
	b.WriteString(sigDomain)
	for _, f := range []string{m.ID, m.From, m.To, m.Re, m.Kind, m.Prio, m.Key, m.Box, strconv.Itoa(len(m.Body))} {
		b.WriteString(f)
		b.WriteByte('\n')
	}
	b.WriteString(m.Body)
	return []byte(b.String())
}

// Sign fills in Key, Box and Sig with the machine's signing key and box (X25519) public key.
func (m *Message) Sign(priv ed25519.PrivateKey, boxPub []byte) {
	m.Key = base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	m.Box = base64.StdEncoding.EncodeToString(boxPub)
	m.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.SignedBytes()))
}

// VerifySig reports whether the message carries a signature, and whether it verifies with the
// key it names. It says nothing of whose key that is: see the client's pins.
func (m Message) VerifySig() (signed, valid bool) {
	if m.Sig == "" && m.Key == "" {
		return false, false
	}
	key, err1 := base64.StdEncoding.DecodeString(m.Key)
	sig, err2 := base64.StdEncoding.DecodeString(m.Sig)
	if err1 != nil || err2 != nil || len(key) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return true, false
	}
	return true, ed25519.Verify(ed25519.PublicKey(key), m.SignedBytes(), sig)
}

// BoxPublic is the X25519 public key of a box private key ("" when it is not one).
func BoxPublic(priv []byte) []byte {
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return nil
	}
	return k.PublicKey().Bytes()
}

// SealedPrefix starts the body of a sealed message.
const SealedPrefix = "[sealed v1] "

// IsSealed reports whether body is a sealed one.
func IsSealed(body string) bool { return strings.HasPrefix(body, SealedPrefix) }

func sealKey(shared, eph, to []byte) []byte {
	h := sha256.New()
	h.Write([]byte("aivoicebus-seal-v1"))
	h.Write(shared)
	h.Write(eph)
	h.Write(to)
	return h.Sum(nil)
}

// Seal encrypts text to the X25519 public key to.
func Seal(text string, to []byte) (string, error) {
	pub, err := ecdh.X25519().NewPublicKey(to)
	if err != nil {
		return "", errors.New("not an X25519 public key")
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return "", err
	}
	block, _ := aes.NewCipher(sealKey(shared, eph.PublicKey().Bytes(), to))
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := append(append(eph.PublicKey().Bytes(), nonce...), gcm.Seal(nil, nonce, []byte(text), nil)...)
	return SealedPrefix + base64.StdEncoding.EncodeToString(out), nil
}

// Open decrypts a sealed body with the recipient's X25519 private key.
func Open(body string, priv []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(body), SealedPrefix))
	if err != nil || len(raw) < 32+12+16 {
		return "", errors.New("not a sealed message")
	}
	key, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return "", errors.New("not an X25519 private key")
	}
	ephPub, err := ecdh.X25519().NewPublicKey(raw[:32])
	if err != nil {
		return "", err
	}
	shared, err := key.ECDH(ephPub)
	if err != nil {
		return "", err
	}
	block, _ := aes.NewCipher(sealKey(shared, raw[:32], key.PublicKey().Bytes()))
	gcm, _ := cipher.NewGCM(block)
	pt, err := gcm.Open(nil, raw[32:44], raw[44:], nil)
	if err != nil {
		return "", errors.New("this machine cannot open it: it was sealed for another machine, or altered")
	}
	return string(pt), nil
}
