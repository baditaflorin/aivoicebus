package forward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
)

// PostError is why a delivery failed, worded without the webhook's URL or key (a URL error from
// net/http quotes the whole URL, which is a capability).
type PostError struct {
	Status     int           // the HTTP status, 0 when none came back
	Msg        string        // what to tell a person
	RetryAfter time.Duration // what the endpoint asked for with Retry-After, if anything
	Permanent  bool          // a refusal that waiting will not fix (bad key, wrong URL): retried slowly
}

func (e *PostError) Error() string { return e.Msg }

// Poster delivers one payload to the webhook.
type Poster interface {
	Post(ctx context.Context, body []byte, idempotency string) error
}

// HTTPPoster posts JSON to a config.Webhook.
type HTTPPoster struct {
	W       config.Webhook
	Timeout time.Duration
	hc      *http.Client
}

// NewHTTPPoster is a poster for w. It never follows a redirect: a custom key header would go to
// wherever the endpoint points, and a webhook has no business redirecting.
func NewHTTPPoster(w config.Webhook) *HTTPPoster {
	p := &HTTPPoster{W: w, Timeout: 20 * time.Second}
	p.hc = &http.Client{
		Timeout:       p.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return p
}

func (p *HTTPPoster) redact(s string) string {
	for _, v := range p.W.Secrets() {
		s = strings.ReplaceAll(s, v, "[webhook]")
	}
	return s
}

// Post sends body. 2xx is delivered; 429 and 5xx are retried by the caller; 401, 403, 404 and the
// other 4xx are Permanent (the key, the header or the URL is wrong).
func (p *HTTPPoster) Post(ctx context.Context, body []byte, idempotency string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.W.URL, bytes.NewReader(body))
	if err != nil {
		return &PostError{Msg: "cannot build the request: the webhook URL is not usable", Permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "aivoicebus/"+client.Release())
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	if p.W.Key != "" {
		req.Header.Set(p.W.Header, p.W.Prefix+p.W.Key)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL error's text is the URL
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &PostError{Msg: "the webhook at " + p.W.Host() + " did not answer: " + p.redact(err.Error())}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return &PostError{Status: resp.StatusCode, RetryAfter: retryAfter(resp), Msg: fmt.Sprintf("the webhook at %s answered %s", p.W.Host(), resp.Status)}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return &PostError{Status: resp.StatusCode, Permanent: true, Msg: fmt.Sprintf("the webhook at %s answered %s (a redirect: not followed; use the final URL)", p.W.Host(), resp.Status)}
	}
	hint := "check the URL and the key"
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		hint = fmt.Sprintf("the key was refused: it is sent as %q (AIVOICEBUS_WEBHOOK_HEADER and _PREFIX change that)", p.W.Header)
	}
	return &PostError{Status: resp.StatusCode, Permanent: true, Msg: fmt.Sprintf("the webhook at %s answered %s: %s", p.W.Host(), resp.Status, hint)}
}

func retryAfter(r *http.Response) time.Duration {
	v := strings.TrimSpace(r.Header.Get("Retry-After"))
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(min(n, 3600)) * time.Second
	}
	return 0
}
