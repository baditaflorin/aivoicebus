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
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
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
	// Put encrypts data on this machine and stores the ciphertext. meta (name, and the
	// size and checksum, which Put fills in) travels inside the encrypted payload when
	// MetaInside says so. burn makes the first read delete it. watch, when not empty,
	// reports the first read.
	Put(ctx context.Context, data []byte, meta Meta, burn bool) (l Link, watch string, err error)
	// Get fetches and decrypts the file l points at.
	Get(ctx context.Context, l Link) ([]byte, error)
	// GetFile is Get plus the metadata the sender sealed inside, when there is any.
	GetFile(ctx context.Context, l Link) (File, error)
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
func (p *Pipe) Put(ctx context.Context, data []byte, meta Meta, burn bool) (Link, string, error) {
	key, err := newKey()
	if err != nil {
		return Link{}, "", err
	}
	sum := sha256.Sum256(data)
	meta.Size, meta.SHA256 = int64(len(data)), hex.EncodeToString(sum[:])
	var env []byte
	if MetaInside() || len(meta.Extra) > 0 { // metadata asked for by the sender is always sealed
		env, err = sealMeta(key, meta, data)
	} else {
		env, err = seal(key, data)
	}
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
	return Link{URL: p.Base + "/#" + id + "~" + key, Name: meta.Name, Size: meta.Size, SHA256: meta.SHA256}, watch, nil
}

// ErrGone is a pipe entry that is not there: expired, or already read.
var ErrGone = errors.New("gone: it expired (files live five minutes) or someone already read it (it can be read once)")

// Get implements FileStore: the file's bytes, without its metadata.
func (p *Pipe) Get(ctx context.Context, l Link) ([]byte, error) {
	f, err := p.GetFile(ctx, l)
	return f.Data, err
}

// GetFile implements FileStore. The server is always the configured one, never one from a
// message. When the sender sealed metadata inside, it is checked against the bytes (size and
// checksum) and returned; it is authenticated by the same MAC as the file, unlike anything
// that rode alongside the link in a message.
func (p *Pipe) GetFile(ctx context.Context, l Link) (File, error) {
	id, key, err := p.idKey(l)
	if err != nil {
		return File{}, err
	}
	resp, err := getOnce(ctx, p.Base+"/"+id, nil)
	if err != nil {
		return File{}, fmt.Errorf("fetch from the pipe: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return File{}, ErrGone
	case resp.StatusCode != http.StatusOK:
		return File{}, fmt.Errorf("the pipe answered %s", resp.Status)
	}
	env, err := io.ReadAll(io.LimitReader(resp.Body, maxEnvelope+1))
	if err != nil {
		return File{}, err
	}
	if len(env) > maxEnvelope {
		return File{}, errors.New("the pipe's answer is larger than any file it accepts")
	}
	return fileFrom(key, env)
}

// fileFrom opens an envelope and, when metadata is sealed inside, checks it against the bytes.
func fileFrom(key string, env []byte) (File, error) {
	data, meta, err := openAny(key, env)
	if err != nil {
		return File{}, err
	}
	f := File{Data: data}
	if meta != nil {
		sum := sha256.Sum256(data)
		if meta.Size != int64(len(data)) || meta.SHA256 != hex.EncodeToString(sum[:]) {
			return File{}, errors.New("the file does not match the size or checksum sealed inside it; not using it")
		}
		f.Meta = *meta
	}
	return f, nil
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

func seal(key string, plaintext []byte) ([]byte, error) { return sealVer(key, verPlain, plaintext) }

func sealVer(key string, ver byte, plaintext []byte) ([]byte, error) {
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
	raw = append(raw, ver)
	raw = append(raw, iv...)
	raw = append(raw, h.Sum(nil)...)
	raw = append(raw, ct...)
	out := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
	base64.StdEncoding.Encode(out, raw)
	return out, nil
}

// Meta is what a sender seals inside a file. Name, Size and SHA256 are the keys this client
// acts on (it verifies them); Extra is the sender's own key/value data, open on purpose: any
// agent can add keys and any reader may use or ignore them. Nothing in Extra is ever run or
// obeyed here, it is shown to the reader as untrusted data. On the wire it is one flat JSON
// object, so go-fleet-pipe's own helper and browser page read and write the same header.
type Meta struct {
	Name   string            // display only, never a path
	Size   int64             // plaintext bytes
	SHA256 string            // of the plaintext, 64 hex chars
	Extra  map[string]string // everything else the sender chose to say
}

var reMetaKey = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

// ValidMetaKey reports whether k can be an Extra key: the same alphabet and length the pipe's
// shell helper accepts, and not one of the three keys this client acts on.
func ValidMetaKey(k string) bool {
	return reMetaKey.MatchString(k) && k != "name" && k != "size" && k != "sha256"
}

// MarshalJSON writes the flat object, Extra keys first so a clash cannot override the verified three.
func (m Meta) MarshalJSON() ([]byte, error) {
	o := map[string]any{}
	for k, v := range m.Extra {
		if !ValidMetaKey(k) {
			return nil, fmt.Errorf("metadata key %q: use 1-32 of letters, digits, _ . - (name, size and sha256 are set by aivoicebus)", k)
		}
		o[k] = v
	}
	if m.Name != "" {
		o["name"] = m.Name
	}
	if m.Size > 0 {
		o["size"] = m.Size
	}
	if m.SHA256 != "" {
		o["sha256"] = m.SHA256
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep <, > and & readable for clients that parse the header with sed
	if err := enc.Encode(o); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b.Bytes()), nil
}

// UnmarshalJSON reads the flat object. Keys it does not act on are kept as text when they are
// strings or numbers and dropped otherwise (nested values are not data this client displays).
func (m *Meta) UnmarshalJSON(b []byte) error {
	var o map[string]json.RawMessage
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	*m = Meta{}
	for k, raw := range o {
		switch k {
		case "name":
			_ = json.Unmarshal(raw, &m.Name)
		case "size":
			_ = json.Unmarshal(raw, &m.Size)
		case "sha256":
			_ = json.Unmarshal(raw, &m.SHA256)
		default:
			if !reMetaKey.MatchString(k) {
				continue
			}
			var str string
			var num json.Number
			switch {
			case json.Unmarshal(raw, &str) == nil:
			case json.Unmarshal(raw, &num) == nil:
				str = num.String()
			default:
				continue
			}
			if m.Extra == nil {
				m.Extra = map[string]string{}
			}
			m.Extra[k] = str
		}
	}
	return nil
}

// ExtraLines is Extra as sorted "key=value" lines, safe to print: control characters are gone
// and a value is cut at 300 runes. It is the sender's data, so callers label it untrusted.
func (m Meta) ExtraLines() []string {
	keys := make([]string, 0, len(m.Extra))
	for k := range m.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v := clean(m.Extra[k], 300)
		out = append(out, k+"="+strings.ReplaceAll(v, "\n", "?"))
	}
	return out
}

// File is a fetched file and the metadata that came sealed with it (zero when the sender's
// client did not seal any).
type File struct {
	Data []byte
	Meta Meta
}

// MetaInside reports whether Put seals metadata into the payload. A reader older than the
// release that added version 3 rejects it ("not an encrypted pipe") and a read-once file is
// then gone, so this stays off until the fleet has updated: AIVOICEBUS_PIPE_META=1 opts in,
// =0 forces it off. Flip metaDefault in the release after every node reads version 3.
func MetaInside() bool {
	switch strings.TrimSpace(os.Getenv("AIVOICEBUS_PIPE_META")) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return metaDefault
}

const metaDefault = false

// Version 3 is version 1 with a header in the plaintext, so the MAC covers it too:
//
//	plaintext = metaLen(uint32 big endian) | meta(JSON, at most maxMeta bytes) | file bytes
const (
	verPlain = 1
	verMeta  = 3
	maxMeta  = 4096
)

func sealMeta(key string, m Meta, data []byte) ([]byte, error) {
	var jb bytes.Buffer
	enc := json.NewEncoder(&jb)
	enc.SetEscapeHTML(false) // keep <, > and & readable for clients that parse the header with sed
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	js := bytes.TrimSpace(jb.Bytes())
	if len(js) > maxMeta {
		return nil, errors.New("file name is too long")
	}
	pt := make([]byte, 4, 4+len(js)+len(data))
	binary.BigEndian.PutUint32(pt, uint32(len(js)))
	pt = append(pt, js...)
	pt = append(pt, data...)
	return sealVer(key, verMeta, pt)
}

// openAny opens version 1 (meta nil) and version 3.
func openAny(key string, envelope []byte) (data []byte, meta *Meta, err error) {
	pt, ver, err := openVer(key, envelope)
	if err != nil || ver == verPlain {
		return pt, nil, err
	}
	if len(pt) < 4 {
		return nil, nil, errors.New("the file's metadata header is cut short")
	}
	n := binary.BigEndian.Uint32(pt)
	if n > maxMeta || int(n) > len(pt)-4 {
		return nil, nil, errors.New("the file's metadata header is not valid")
	}
	var m Meta
	if err := json.Unmarshal(pt[4:4+n], &m); err != nil {
		return nil, nil, errors.New("the file's metadata is not valid")
	}
	if len(m.SHA256) != 64 || !isHex(m.SHA256) {
		m.SHA256 = ""
	}
	m.SHA256 = strings.ToLower(m.SHA256)
	return pt[4+n:], &m, nil
}

// ErrAuth means the key is wrong or the bytes were changed on the way.
var ErrAuth = errors.New("authentication failed: wrong key, or the file was tampered with")

func open(key string, envelope []byte) ([]byte, error) {
	pt, _, err := openVer(key, envelope)
	return pt, err
}

func openVer(key string, envelope []byte) ([]byte, byte, error) {
	envelope = bytes.TrimSpace(envelope)
	raw := make([]byte, base64.StdEncoding.DecodedLen(len(envelope)))
	n, err := base64.StdEncoding.Decode(raw, envelope)
	if err != nil || n < 49 || (raw[0] != verPlain && raw[0] != verMeta) {
		return nil, 0, errors.New("not an encrypted pipe")
	}
	raw = raw[:n]
	iv, tag, ct := raw[1:17], raw[17:49], raw[49:]
	ek, mk := subkeys(key)
	h := hmac.New(sha256.New, mk)
	h.Write(iv)
	h.Write(ct)
	if subtle.ConstantTimeCompare(h.Sum(nil), tag) != 1 {
		return nil, 0, ErrAuth
	}
	block, err := aes.NewCipher(ek[:])
	if err != nil {
		return nil, 0, err
	}
	pt := make([]byte, len(ct))
	cipher.NewCTR(block, iv).XORKeyStream(pt, ct)
	return pt, raw[0], nil
}
