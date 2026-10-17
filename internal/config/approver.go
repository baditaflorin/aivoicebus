package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The approver key signs approvals of write-mode worker tasks. Any process on a machine can sign
// with the machine key, a prompt-injected agent included, so an approval signed only by the machine
// proves nothing about a person. This key is kept encrypted with a passphrase only the person knows
// (PBKDF2-SHA256, 600000 rounds, then AES-256-GCM), and `approve` asks for it on the terminal.

const (
	approverFile   = "approver.key"
	approverRounds = 600000
	MinPassphrase  = 10
)

type approverJSON struct {
	V      int    `json:"v"`
	Rounds int    `json:"rounds"`
	Salt   string `json:"salt"`
	Nonce  string `json:"nonce"`
	Sealed string `json:"sealed"` // the ed25519 seed, encrypted
	Pub    string `json:"pub"`    // the public key, readable without the passphrase
}

// ApproverPath is where the encrypted approver key lives.
func ApproverPath() string { return filepath.Join(Dir(), approverFile) }

func approverAEAD(pass string, salt []byte, rounds int) (cipher.AEAD, error) {
	k, err := pbkdf2.Key(sha256.New, pass, salt, rounds, 32)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// CreateApprover makes a new approver key sealed with pass. An existing one is never replaced.
func CreateApprover(pass string) (ed25519.PublicKey, error) {
	if len(pass) < MinPassphrase {
		return nil, fmt.Errorf("the passphrase is at least %d characters", MinPassphrase)
	}
	if _, err := os.Stat(ApproverPath()); err == nil {
		return nil, errors.New("an approver key exists already (" + ApproverPath() + "); remove it yourself to make a new one, and give workers the new public key")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt, nonce := make([]byte, 16), make([]byte, 12)
	_, _ = rand.Read(salt)
	_, _ = rand.Read(nonce)
	aead, err := approverAEAD(pass, salt, approverRounds)
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString
	j := approverJSON{V: 1, Rounds: approverRounds, Salt: enc(salt), Nonce: enc(nonce), Sealed: enc(aead.Seal(nil, nonce, priv.Seed(), []byte("aivoicebus approver v1"))), Pub: enc(pub)}
	b, _ := json.MarshalIndent(j, "", "  ")
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(ApproverPath(), append(b, '\n'), 0o600); err != nil {
		return nil, err
	}
	return pub, nil
}

func readApprover() (approverJSON, error) {
	var j approverJSON
	b, err := os.ReadFile(ApproverPath())
	if err != nil {
		return j, errors.New("no approver key on this machine: aivoicebus approver init")
	}
	if err := json.Unmarshal(b, &j); err != nil || j.V != 1 {
		return j, errors.New(ApproverPath() + " is not an approver key this release reads")
	}
	return j, nil
}

// ApproverPublic is the approver key's public half (no passphrase needed).
func ApproverPublic() (ed25519.PublicKey, error) {
	j, err := readApprover()
	if err != nil {
		return nil, err
	}
	pub, err := base64.StdEncoding.DecodeString(j.Pub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New(ApproverPath() + ": bad public key")
	}
	return pub, nil
}

// OpenApprover unseals the approver key with pass.
func OpenApprover(pass string) (ed25519.PrivateKey, error) {
	j, err := readApprover()
	if err != nil {
		return nil, err
	}
	dec := base64.StdEncoding.DecodeString
	salt, err1 := dec(j.Salt)
	nonce, err2 := dec(j.Nonce)
	sealed, err3 := dec(j.Sealed)
	if err1 != nil || err2 != nil || err3 != nil || j.Rounds < 100000 {
		return nil, errors.New(ApproverPath() + " is damaged")
	}
	aead, err := approverAEAD(pass, salt, j.Rounds)
	if err != nil {
		return nil, err
	}
	seed, err := aead.Open(nil, nonce, sealed, []byte("aivoicebus approver v1"))
	if err != nil {
		return nil, errors.New("wrong passphrase")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
