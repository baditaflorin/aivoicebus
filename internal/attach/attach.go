// Package attach lets a message point at something held by another fleet service instead
// of carrying it. aivoicebus stays the small core: it knows a generic attachment link and a
// Provider interface, and each service (pipe server for files, tail server for live
// logs) is an adapter behind it. The link is the service's own native URL, so any agent can
// use the service directly, with curl or the service's own client, without aivoicebus.
//
// A message carries one "[attach]" line: the URL, then optional name=, size= and sha256=
// fields. Everything on it is untrusted. The reader fetches only from a service it is
// configured with (never from a host named in a message), validates every field, and the
// view shows a summary instead of the line so a key in the URL never reaches an agent's
// context.
package attach

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

const linePrefix = "[attach]"

// Link is an attachment: the service URL and what the sender says about it.
type Link struct {
	URL    string // the service's own link, e.g. https://pipe.example.com/#id~key
	Name   string // the sender's file name; display only, never a path
	Size   int64  // plaintext bytes, when known
	SHA256 string // of the plaintext, when known: 64 hex chars
}

// Line is the one body line that carries l.
func (l Link) Line() string {
	s := linePrefix + " " + l.URL
	if l.Name != "" {
		s += " name=" + url.PathEscape(l.Name)
	}
	if l.Size > 0 {
		s += " size=" + strconv.FormatInt(l.Size, 10)
	}
	if l.SHA256 != "" {
		s += " sha256=" + l.SHA256
	}
	return s
}

// Provider is a service a message can point at. Handles says whether a link is one of this
// service's own (same host as the configured server, and the shape it issues); a provider
// may also implement FileStore or LogStream for what it can do with such a link.
type Provider interface {
	Name() string
	Handles(u *url.URL) bool
	// Describe is the one-line summary a reader sees instead of the link.
	Describe(l Link, msgID string) string
}

// Providers are the services this machine is configured to use, in the order they are tried:
// the pipe, the tail, then the mesh's own nodes (whose token the caller adds before using it).
func Providers() []Provider {
	var ps []Provider
	if b, err := baseURL("AIVOICEBUS_PIPE_URL", DefaultPipeURL); err == nil {
		ps = append(ps, &Pipe{Base: b})
	}
	if b, err := baseURL("AIVOICEBUS_TAIL_URL", DefaultTailURL); err == nil {
		ps = append(ps, &Tail{Base: b})
	}
	if urls := config.URLs(); len(urls) > 0 {
		ps = append(ps, &Node{URLs: urls})
	}
	return ps
}

// ProviderFor returns the provider that handles l, or nil. A link to a host that is not a
// configured service has no provider and is never fetched.
func ProviderFor(l Link) Provider {
	u, err := url.Parse(l.URL)
	if err != nil {
		return nil
	}
	for _, p := range Providers() {
		if p.Handles(u) {
			return p
		}
	}
	return nil
}

// Find returns the link in body. The first [attach] line wins. A line that cannot be read
// is reported as an error so a reader can say the attachment is unusable.
func Find(body string) (Link, bool, error) {
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), linePrefix+" ")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			return Link{}, true, fmt.Errorf("empty attachment")
		}
		l := Link{URL: f[0]}
		for _, kv := range f[1:] {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "name":
				l.Name, _ = url.PathUnescape(v)
			case "size":
				l.Size, _ = strconv.ParseInt(v, 10, 64)
			case "sha256":
				if len(v) == 64 && isHex(v) {
					l.SHA256 = strings.ToLower(v)
				}
			}
		}
		if l.Size < 0 || l.Size > MaxFile {
			l.Size = 0
		}
		return l, true, nil
	}
	return Link{}, false, nil
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// Render is body as people and agents should see it: the [attach] line replaced by a
// summary that says how to fetch it, so the URL (which can hold a key) stays out of their
// context. A body without one is returned unchanged, so Render is safe to apply twice.
func Render(id, body string) string {
	if !strings.Contains(body, linePrefix+" ") {
		return body
	}
	l, found, err := Find(body)
	if !found {
		return body
	}
	var summary string
	switch p := ProviderFor(l); {
	case err != nil:
		summary = "[attachment] " + err.Error()
	case p == nil:
		summary = "[attachment] a link to a service this machine is not set up to use; not fetched"
	default:
		summary = p.Describe(l, id)
	}
	var out []string
	replaced := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), linePrefix+" ") {
			if !replaced {
				out = append(out, summary)
				replaced = true
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// HumanSize writes n bytes the short way.
func HumanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// DisplayName is a sender's file name cut to something safe to print: no control
// characters, at most 80 runes.
func DisplayName(name string) string { return clean(name, 80) }

// SafeName is a file name that cannot leave the directory it is written to or hide: the last
// path element only, printable characters, no leading dot, at most 100 runes. Never empty.
func SafeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimLeft(clean(name, 100), ". ")
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	if name == "" {
		return "attachment"
	}
	return name
}

func clean(s string, max int) string {
	s = strings.ToValidUTF8(s, "?")
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			r = '?'
		}
		if n++; n > max {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Sanitize makes text from a peer safe to print to a terminal and into an agent's context:
// escape sequences and other control characters (not newline or tab) become "?".
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
}

// ErrOff is a service the user turned off (AIVOICEBUS_PIPE_URL=off).
var ErrOff = errors.New("turned off")

// baseURL reads a service's server from the environment: https only, except plain http to a
// loopback address (tests, a local instance). No trailing slash. "off" or "none" turns it off.
func baseURL(env, def string) (string, error) {
	raw := strings.TrimRight(strings.TrimSpace(os.Getenv(env)), "/")
	switch strings.ToLower(raw) {
	case "":
		raw = def
	case "off", "none":
		return "", fmt.Errorf("%s: %w", env, ErrOff)
	}
	if raw == "" {
		return "", fmt.Errorf("%s: %w", env, ErrOff)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%s: not a usable server URL", env)
	}
	loop := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loop) {
		return "", fmt.Errorf("%s must be an https URL", env)
	}
	return raw, nil
}
