package attach

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultPipeURL is the public pipe run by the project's author: it holds only ciphertext (the
// file is encrypted on the sender's machine and the key never leaves the message), for five
// minutes, in memory. AIVOICEBUS_PIPE_URL picks another pipe, or "off" for none. The tail has
// no built-in address: nothing is streamed unless AIVOICEBUS_TAIL_URL names a server.
const (
	DefaultPipeURL = "https://pipe.0exec.com"
	DefaultTailURL = ""
)

// MaxFile is the largest file that fits the pipe's 10 MiB limit once encrypted and
// base64-encoded (4/3 of the size plus a 49-byte envelope).
const MaxFile = 7 << 20

// PipeTTL is the longest a pipe entry lives; the pipe clamps anything above it.
const PipeTTL = 5 * time.Minute

const maxEnvelope = 10<<20 + 1024

// FileStore is what a provider that can hold a file offers.
type FileStore interface {
	Provider
	// Put encrypts data on this machine and stores the ciphertext. burn makes the first
	// read delete it. watch, when not empty, reports the first read.
	Put(ctx context.Context, data []byte, burn bool) (l Link, watch string, err error)
	// Get fetches and decrypts the file l points at.
	Get(ctx context.Context, l Link) ([]byte, error)
	// WatchRead waits up to wait for the first read of what watch URL covers.
	WatchRead(ctx context.Context, watch string, wait time.Duration) (bool, error)
}

// Pipe is the pipe server adapter. Its links are the pipe's own end-to-end links,
// https://HOST/#ID~KEY, so `p open` and the pipe's browser app open them too.
type Pipe struct{ Base string }

var reFrag = regexp.MustCompile(`^([A-Za-z0-9_-]{4,64})~([0-9a-f]{64})$`)

// Name implements Provider.
func (*Pipe) Name() string { return "pipe" }

// Handles reports whether u is an end-to-end link to this pipe's own server.
func (p *Pipe) Handles(u *url.URL) bool {
	b, err := url.Parse(p.Base)
	return err == nil && u.User == nil && u.Scheme == b.Scheme && u.Host == b.Host && (u.Path == "" || u.Path == "/") &&
		u.RawQuery == "" && reFrag.MatchString(u.Fragment)
}

// Describe implements Provider.
func (*Pipe) Describe(l Link, msgID string) string {
	name := "a file"
	if l.Name != "" {
		name = fmt.Sprintf("file %q", DisplayName(l.Name))
	}
	size := ""
	if l.Size > 0 {
		size = ", " + HumanSize(l.Size)
	}
	return fmt.Sprintf("[attachment] %s%s, encrypted, readable once for a few minutes, fetch: aivoicebus get %s", name, size, msgID)
}

func (p *Pipe) idKey(l Link) (id, key string, err error) {
	u, perr := url.Parse(l.URL)
	if perr != nil || !p.Handles(u) {
		return "", "", errors.New("not a link to this machine's pipe")
	}
	m := reFrag.FindStringSubmatch(u.Fragment)
	return m[1], m[2], nil
}

// Put implements FileStore.
func (p *Pipe) Put(ctx context.Context, data []byte, burn bool) (Link, string, error) {
	key, err := newKey()
	if err != nil {
		return Link{}, "", err
	}
	env, err := seal(key, data)
	if err != nil {
		return Link{}, "", err
	}
	q := url.Values{"ttl": {fmt.Sprint(int(PipeTTL / time.Second))}, "notify": {"1"}}
	if burn {
		q.Set("burn", "1")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Base+"/?"+q.Encode(), bytes.NewReader(env))
	if err != nil {
		return Link{}, "", err
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := httpClient(2 * time.Minute).Do(req)
	if err != nil {
		return Link{}, "", fmt.Errorf("upload to the pipe: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode != http.StatusCreated {
		return Link{}, "", fmt.Errorf("the pipe refused the upload: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	u, err := url.Parse(strings.TrimSpace(string(b)))
	if err != nil {
		return Link{}, "", errors.New("the pipe's answer was not a link")
	}
	id := u.Path[strings.LastIndex(u.Path, "/")+1:]
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`).MatchString(id) {
		return Link{}, "", errors.New("the pipe's answer was not a usable id")
	}
	watch := ""
	if w := resp.Header.Get("X-Pipe-Watch"); strings.HasPrefix(w, p.Base+"/") {
		watch = w
	}
	sum := sha256.Sum256(data)
	return Link{URL: p.Base + "/#" + id + "~" + key, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, watch, nil
}

// ErrGone is a pipe entry that is not there: expired, or already read.
var ErrGone = errors.New("gone: it expired (files live five minutes) or someone already read it (it can be read once)")

// Get implements FileStore. The server is always the configured one, never one from a message.
func (p *Pipe) Get(ctx context.Context, l Link) ([]byte, error) {
	id, key, err := p.idKey(l)
	if err != nil {
		return nil, err
	}
	resp, err := getOnce(ctx, p.Base+"/"+id, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch from the pipe: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrGone
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("the pipe answered %s", resp.Status)
	}
	env, err := io.ReadAll(io.LimitReader(resp.Body, maxEnvelope+1))
	if err != nil {
		return nil, err
	}
	if len(env) > maxEnvelope {
		return nil, errors.New("the pipe's answer is larger than any file it accepts")
	}
	return open(key, env)
}

// WatchRead implements FileStore: it long-polls the watch URL, which answers about every 25s.
func (p *Pipe) WatchRead(ctx context.Context, watch string, wait time.Duration) (bool, error) {
	if !strings.HasPrefix(watch, p.Base+"/") {
		return false, errors.New("watch URL is not on this pipe")
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, watch, nil)
		if err != nil {
			return false, err
		}
		resp, err := httpClient(40 * time.Second).Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return false, nil
			}
			return false, err
		}
		var r struct {
			Read bool `json:"read"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return false, nil // expired before anyone read it
		}
		if json.Unmarshal(b, &r) == nil && r.Read {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, nil
		}
	}
}

// getOnce GETs u, trying a second time when the first attempt got no answer at all (a refused
// connection, a TLS handshake that timed out). That is safe for a burn-after-read entry: a request
// that got no response was never served, so nothing was burned. Once any answer arrives, it is
// what the caller gets. header may add request headers.
func getOnce(ctx context.Context, u string, header http.Header) (*http.Response, error) {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, last
			case <-time.After(2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := httpClient(2 * time.Minute).Do(req)
		if err == nil {
			return resp, nil
		}
		last = err
	}
	return nil, last
}

// httpClient never follows a redirect: a hostile or misconfigured server could otherwise send
// a request, which may carry a body, somewhere else.
func httpClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// The envelope is pipe server's own end-to-end format:
//
//	base64( 0x01 | iv(16) | mac(32) | ciphertext )
//
// AES-256-CTR, then HMAC-SHA256 over iv|ciphertext (encrypt-then-MAC). The key is 32 random
// bytes as 64 hex characters; the cipher key is sha256(keyhex+":enc") and the MAC key is the
// hex text of sha256(keyhex+":mac").

func newKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// subkeys derives the cipher key (raw bytes) and the MAC key. The pipe's helpers pass the MAC key
// to openssl as the hex text of the digest, so that text, not the raw digest, is the HMAC key.
func subkeys(key string) (enc [32]byte, mac []byte) {
	m := sha256.Sum256([]byte(key + ":mac"))
	return sha256.Sum256([]byte(key + ":enc")), []byte(hex.EncodeToString(m[:]))
}

func seal(key string, plaintext []byte) ([]byte, error) {
	ek, mk := subkeys(key)
	block, err := aes.NewCipher(ek[:])
	if err != nil {
		return nil, err
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	ct := make([]byte, len(plaintext))
	cipher.NewCTR(block, iv).XORKeyStream(ct, plaintext)
	h := hmac.New(sha256.New, mk)
	h.Write(iv)
	h.Write(ct)
	raw := make([]byte, 0, 49+len(ct))
	raw = append(raw, 1)
	raw = append(raw, iv...)
	raw = append(raw, h.Sum(nil)...)
	raw = append(raw, ct...)
	out := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
	base64.StdEncoding.Encode(out, raw)
	return out, nil
}

// ErrAuth means the key is wrong or the bytes were changed on the way.
var ErrAuth = errors.New("authentication failed: wrong key, or the file was tampered with")

func open(key string, envelope []byte) ([]byte, error) {
	envelope = bytes.TrimSpace(envelope)
	raw := make([]byte, base64.StdEncoding.DecodedLen(len(envelope)))
	n, err := base64.StdEncoding.Decode(raw, envelope)
	if err != nil || n < 49 || raw[0] != 1 {
		return nil, errors.New("not an encrypted pipe")
	}
	raw = raw[:n]
	iv, tag, ct := raw[1:17], raw[17:49], raw[49:]
	ek, mk := subkeys(key)
	h := hmac.New(sha256.New, mk)
	h.Write(iv)
	h.Write(ct)
	if subtle.ConstantTimeCompare(h.Sum(nil), tag) != 1 {
		return nil, ErrAuth
	}
	block, err := aes.NewCipher(ek[:])
	if err != nil {
		return nil, err
	}
	pt := make([]byte, len(ct))
	cipher.NewCTR(block, iv).XORKeyStream(pt, ct)
	return pt, nil
}
