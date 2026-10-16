// Package credstore keeps the credentials a person hands to the agents on their machine
// (a CI or GitHub token, say), so that the person types it once, in their own
// terminal, and never pastes it into a chat. Each one is a file, <base>/<name>/token,
// mode 0600 in a 0700 directory, next to a list of the hosts it may be sent to. Agents use
// it through `aivoicebus secret curl`, which keeps it out of arguments and output.
//
// This is the same trust level as the mesh token in ~/.config/aivoicebus: anything that
// runs as the user can read the files. It is a hand-off convenience and transcript
// hygiene, not a vault and not a sandbox.
package credstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// File is the name of the credential inside its directory, and HostsFile the list of hosts
// it may be sent to.
const (
	File      = "token"
	HostsFile = "hosts"
)

// MaxLen bounds a credential. Real ones are a few hundred bytes; this only stops a runaway paste.
const MaxLen = 4096

var (
	nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
	hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

// reserved names would only confuse: aivoicebus keeps its own token one directory up.
var reserved = map[string]bool{"aivoicebus": true}

// ValidName reports why name cannot name a credential, or nil.
func ValidName(name string) error {
	switch {
	case !nameRe.MatchString(name):
		return fmt.Errorf("%q is not a credential name: 1 to 40 characters of a-z, 0-9, - and _, starting with a letter (github, hetzner-dns, deploy-key)", name)
	case reserved[name]:
		return fmt.Errorf("%q is reserved", name)
	}
	return nil
}

// ValidHost reports whether h is a plain lower-case ASCII host name (no port, no scheme).
// Look-alike Unicode names are refused: the name that was typed must be the name that is used.
func ValidHost(h string) bool { return hostRe.MatchString(h) }

// Path is where the credential called name lives under base.
func Path(base, name string) string { return filepath.Join(base, name, File) }

// Clean trims the whitespace a paste brings along and refuses what no token contains:
// anything but printable ASCII, and the two characters (a quote and a backslash) that
// would need escaping in a curl config. The error never repeats the value.
func Clean(raw string) (string, error) {
	t := strings.TrimSpace(raw)
	switch {
	case t == "":
		return "", errors.New("the credential is empty")
	case len(t) > MaxLen:
		return "", fmt.Errorf("the credential is longer than %d bytes: was the wrong thing pasted?", MaxLen)
	}
	for i := 0; i < len(t); i++ {
		if c := t[i]; c <= 0x20 || c >= 0x7f || c == '"' || c == '\\' {
			return "", errors.New("the credential has a space, line break or other character no token contains: was the wrong thing pasted?")
		}
	}
	return t, nil
}

// credDir makes sure base and <base>/<name> are real directories (not symlinks, which could
// send the credential somewhere a dotfiles repo or a backup would pick it up) and private.
func credDir(base, name string) (string, error) {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	dir := filepath.Join(base, name)
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symbolic link: refusing to put a credential behind it", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	return dir, os.Chmod(dir, 0o700)
}

// writeAtomic writes data to <dir>/<name> mode 0600 through a temporary file and a rename,
// syncing before the rename so a crash leaves the old file or the whole new one.
func writeAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// Save writes the credential and the hosts it may be sent to (which may be empty: then
// `secret curl` refuses to use it until it is saved again with some).
func Save(base, name, token string, hosts []string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	tok, err := Clean(token)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var hs []string
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if !ValidHost(h) {
			return fmt.Errorf("%q is not a host name (plain ASCII, no scheme or port)", h)
		}
		if !seen[h] {
			seen[h] = true
			hs = append(hs, h)
		}
	}
	sort.Strings(hs)
	dir, err := credDir(base, name)
	if err != nil {
		return err
	}
	if err := writeAtomic(dir, HostsFile, []byte(strings.Join(hs, "\n")+"\n")); err != nil {
		return err
	}
	return writeAtomic(dir, File, []byte(tok))
}

// openPrivate opens a regular file for reading without blocking and without following a
// swap to a special file between the check and the open.
func openPrivate(p string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s is not a regular file", p)
	}
	return f, fi, nil
}

// Load reads a credential. It refuses a file other users can read: that is a leak
// waiting to be used, and the fix is one chmod, named in the error.
func Load(base, name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	p := Path(base, name)
	f, fi, err := openPrivate(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("no credential %q saved here (run: aivoicebus secret save %s)", name, name)
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by other users (mode %04o): run chmod 600 %s", p, fi.Mode().Perm(), p)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxLen+2))
	if err != nil {
		return "", err
	}
	return Clean(string(b))
}

// Hosts are the hosts the credential may be sent to (empty if none were bound).
func Hosts(base, name string) ([]string, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	f, _, err := openPrivate(filepath.Join(base, name, HostsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 8192))
	var out []string
	for _, l := range strings.Fields(string(b)) {
		if ValidHost(l) {
			out = append(out, l)
		}
	}
	return out, nil
}

// Entry describes one saved credential. It never holds the value.
type Entry struct {
	Name    string
	Path    string
	Mode    os.FileMode
	Size    int64
	Saved   time.Time
	Private bool     // false when other users can read it
	Hosts   []string // where it may be sent
}

// List names the credentials saved under base, sorted by name.
func List(base string) ([]Entry, error) {
	dirs, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, d := range dirs {
		if !d.IsDir() || ValidName(d.Name()) != nil {
			continue
		}
		p := Path(base, d.Name())
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		hs, _ := Hosts(base, d.Name())
		out = append(out, Entry{Name: d.Name(), Path: p, Mode: fi.Mode().Perm(), Size: fi.Size(), Saved: fi.ModTime(), Private: fi.Mode().Perm()&0o077 == 0, Hosts: hs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Remove deletes a credential and its directory. It never follows a symbolic link: a
// credential directory that is one is somebody's own arrangement, and is left alone.
func Remove(base, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	dir := filepath.Join(base, name)
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no credential %q saved here", name)
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a plain directory: not touching it", dir)
	}
	if err := os.Remove(Path(base, name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no credential %q saved here", name)
		}
		return err
	}
	_ = os.Remove(filepath.Join(dir, HostsFile))
	_ = os.Remove(dir) // fails, harmlessly, if something else lives there
	return nil
}

// Preset is how a credential is checked: what to ask, and which field of the answer names its owner.
type Preset struct{ URL, Field string }

// Presets are the services whose tokens can be checked without any more input.
var Presets = map[string]Preset{
	"github": {URL: "https://api.github.com/user", Field: "login"},
}

// HostOf is the lower-case host name of an https (or, if allowLoopback, loopback http) URL, or an error.
func HostOf(rawURL string, allowLoopback bool) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q is not a URL", rawURL)
	}
	h := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowLoopback && isLoopback(h):
	default:
		return "", errors.New("the URL must be https")
	}
	if !ValidHost(h) && !isLoopback(h) {
		return "", fmt.Errorf("the host %q is not a plain ASCII name (a look-alike?)", h)
	}
	return h, nil
}

// Check asks url (with the token as a bearer credential) who it belongs to, and returns
// the value of field in the JSON answer (at most 64 characters). It proves the token works,
// and says whose it is, without printing it. Only https is allowed (plain http to a loopback
// address only when allowLoopback, which is for tests: a program on this machine can listen
// there), and a redirect is an error: the token goes to the one host that was named.
func Check(ctx context.Context, hc *http.Client, rawURL, field, token string, allowLoopback bool) (string, error) {
	host, err := HostOf(rawURL, allowLoopback)
	if err != nil {
		return "", err
	}
	if field == "" {
		field = "login"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach %s: %v", host, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("%s rejected the token (HTTP %d)", host, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%s answered HTTP %d, not 200", host, resp.StatusCode)
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return "", fmt.Errorf("%s did not answer with JSON: is this the right service?", host)
	}
	v, ok := m[field].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("the answer from %s has no %q field: is this the right service?", host, field)
	}
	if len(v) > 64 {
		v = v[:64] + "..."
	}
	return v, nil
}

func isLoopback(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }

// CurlConfig is the config for `curl -K -` that sends the credential in a header, so it
// is never an argument (visible to every process through ps). header is a header line with
// {} where the token goes, for example "Authorization: Bearer {}".
func CurlConfig(header, token string) (string, error) {
	if strings.Count(header, "{}") != 1 || strings.ContainsAny(header, "\"\\\r\n") {
		return "", errors.New(`the header must contain {} exactly once, and no quote, backslash or line break (for example "Authorization: Bearer {}")`)
	}
	if t, err := Clean(token); err != nil || t != token {
		return "", errors.New("the credential cannot be put in a header")
	}
	return fmt.Sprintf("header = \"%s\"\n", strings.Replace(header, "{}", token, 1)), nil
}

// Options of curl that an agent must not pass to `secret curl`: the ones that print the
// request headers (and so the credential: -v and the traces), read another config, follow a
// redirect to another host (which may carry the credential along), add a second request or
// reroute the first.
var (
	bannedLong = []string{"verbose", "trace", "trace-ascii", "trace-config", "trace-time", "libcurl", "config", "next",
		"location", "location-trusted", "url", "connect-to", "resolve", "proxy", "preproxy", "socks4", "socks4a", "socks5",
		"socks5-hostname", "doh-url", "parallel", "stderr", "variable", "expand-url", "expand-header", "netrc", "netrc-file"}
	bannedShort = "vKLx"
)

// CheckCurlArgs refuses the options above, and any argument that is a URL unless it is
// https to one of hosts. It returns the URLs it found: at least one is required, because a
// bare host name could not be told from the value of an option.
func CheckCurlArgs(args, hosts []string) (urls []string, err error) {
	if len(hosts) == 0 {
		return nil, errors.New("this credential is not bound to any host: save it again with --host HOST (the service it is for), so it cannot be sent anywhere else")
	}
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--"):
			name := strings.TrimPrefix(a, "--")
			if i := strings.IndexByte(name, '='); i >= 0 {
				name = name[:i]
			}
			if name == "" {
				continue
			}
			for _, b := range bannedLong { // also an abbreviation of one: curl accepts --verb for --verbose
				if strings.HasPrefix(b, name) {
					return nil, fmt.Errorf("curl option %s is not allowed with a saved credential (it could print or redirect it)", a)
				}
			}
		case strings.HasPrefix(a, "-") && len(a) > 1 && !strings.Contains(a, "://"):
			if strings.ContainsAny(a[1:], bannedShort) && !strings.HasPrefix(a, "--") {
				return nil, fmt.Errorf("curl option %s is not allowed with a saved credential (it could print or redirect it)", a)
			}
		}
		if strings.Contains(a, "://") && !strings.HasPrefix(a, "-") {
			h, err := HostOf(a, false)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", a, err)
			}
			ok := false
			for _, b := range hosts {
				ok = ok || b == h
			}
			if !ok {
				return nil, fmt.Errorf("this credential may only be sent to %s, not to %s", strings.Join(hosts, ", "), h)
			}
			urls = append(urls, a)
		}
	}
	if len(urls) == 0 {
		return nil, errors.New("give the full https URL of the request, for example https://" + hosts[0] + "/...")
	}
	return urls, nil
}

// Redactor wraps w so that the secret never gets through, even split across writes: every
// occurrence becomes "<credential>". Close flushes what it was holding back.
func Redactor(w io.Writer, secret string) io.WriteCloser {
	return &redactor{w: w, secret: []byte(secret)}
}

type redactor struct {
	w      io.Writer
	secret []byte
	held   []byte // the tail that might be the start of the secret
}

var mask = []byte("<credential>")

func (r *redactor) Write(p []byte) (int, error) {
	if len(r.secret) == 0 {
		return r.w.Write(p)
	}
	buf := append(r.held, p...)
	buf = bytes.ReplaceAll(buf, r.secret, mask)
	// hold back the longest tail that is a prefix of the secret, so a split occurrence is caught
	keep := 0
	for n := min(len(buf), len(r.secret)-1); n > 0; n-- {
		if bytes.HasSuffix(buf, r.secret[:n]) {
			keep = n
			break
		}
	}
	out := buf[:len(buf)-keep]
	r.held = append([]byte(nil), buf[len(buf)-keep:]...)
	if _, err := r.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (r *redactor) Close() error {
	if len(r.held) == 0 {
		return nil
	}
	_, err := r.w.Write(r.held)
	r.held = nil
	return err
}
