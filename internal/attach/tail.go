package attach

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
)

// LogStream is what a provider that can hold a live log offers.
type LogStream interface {
	Provider
	// NewStream returns a fresh unguessable stream name and the link that points at it.
	NewStream() (stream string, l Link, err error)
	// Push streams r into the stream line by line; mask may replace a line (to hide a
	// credential) and is called on every line.
	Push(ctx context.Context, stream string, r io.Reader, mask func(line string) (string, bool)) (lines, masked int, err error)
	// Read copies the stream to w, every line made safe to print: the last `lines` lines, or
	// with follow, on until it ends or ctx is done. At most maxBytes are written.
	Read(ctx context.Context, l Link, w io.Writer, lines int, follow bool, maxBytes int64) error
	// LinkFor is the link for an existing stream name.
	LinkFor(stream string) (Link, error)
}

// Tail is the tail server adapter. Its links are the tail's own stream URLs,
// https://HOST/STREAM, so `curl -N` follows them. The tail service has no access control:
// the stream name is the capability, so names must be long and random.
type Tail struct{ Base string }

var reStream = regexp.MustCompile(`^[A-Za-z0-9_-]{12,64}$`)

// Name implements Provider.
func (*Tail) Name() string { return "tail" }

func (t *Tail) stream(u *url.URL) (string, bool) {
	b, err := url.Parse(t.Base)
	if err != nil || u.User != nil || u.Scheme != b.Scheme || u.Host != b.Host || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	s := u.Path
	if len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	return s, reStream.MatchString(s)
}

// Handles reports whether u is a stream on this tail server.
func (t *Tail) Handles(u *url.URL) bool { _, ok := t.stream(u); return ok }

// Describe implements Provider.
func (*Tail) Describe(_ Link, msgID string) string {
	return fmt.Sprintf("[attachment] live log, fetch: aivoicebus get %s (--follow keeps reading)", msgID)
}

// ValidStream reports whether s may name a stream.
func ValidStream(s string) bool { return reStream.MatchString(s) }

// LinkFor implements LogStream.
func (t *Tail) LinkFor(stream string) (Link, error) {
	if !ValidStream(stream) {
		return Link{}, errors.New("a stream name must be 12 to 64 letters, digits, - or _ (it is the only thing keeping the stream private)")
	}
	return Link{URL: t.Base + "/" + stream}, nil
}

// NewStream implements LogStream (96 random bits).
func (t *Tail) NewStream() (string, Link, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", Link{}, err
	}
	s := hex.EncodeToString(b)
	l, err := t.LinkFor(s)
	return s, l, err
}

// Push implements LogStream.
func (t *Tail) Push(ctx context.Context, stream string, r io.Reader, mask func(string) (string, bool)) (lines, masked int, err error) {
	if !ValidStream(stream) {
		return 0, 0, errors.New("bad stream name")
	}
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if mask != nil {
				var hit bool
				if line, hit = mask(line); hit {
					masked++
				}
			}
			lines++
			if _, err := io.WriteString(pw, line+"\n"); err != nil {
				return // the request failed; Do reports why
			}
		}
		pw.CloseWithError(sc.Err())
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Base+"/"+stream, pr)
	if err != nil {
		return 0, 0, err
	}
	req.ContentLength = -1 // chunked, so followers see lines as they are written
	req.Header.Set("Content-Type", "text/plain")
	resp, err := httpClient(0).Do(req)
	if err != nil {
		pr.Close()
		return lines, masked, fmt.Errorf("stream to tail: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return lines, masked, fmt.Errorf("tail refused the stream: %s", resp.Status)
	}
	return lines, masked, nil
}

// Read implements LogStream.
func (t *Tail) Read(ctx context.Context, l Link, w io.Writer, lines int, follow bool, maxBytes int64) error {
	u, err := url.Parse(l.URL)
	if err != nil {
		return errors.New("bad link")
	}
	stream, ok := t.stream(u)
	if !ok {
		return errors.New("not a link to this machine's tail server")
	}
	q := url.Values{}
	if lines > 0 {
		q.Set("lines", fmt.Sprint(lines))
	}
	if !follow {
		q.Set("follow", "0")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Base+"/"+stream+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := httpClient(0).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("read from tail: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errors.New("no such stream (it ended, or idled out)")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("tail answered %s", resp.Status)
	}
	var written int64
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := Sanitize(sc.Text()) + "\n"
		if written += int64(len(line)); written > maxBytes {
			fmt.Fprintln(w, "[output cut: size limit reached]")
			return nil
		}
		io.WriteString(w, line)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
