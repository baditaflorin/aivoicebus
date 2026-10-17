package tasks

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// Stuck is a task this machine sent that nobody took or answered in time.
type Stuck struct {
	ID, From, To string
	At           time.Time
}

// Unanswered lists the tasks the machine host sent (from what this machine remembers sending:
// the last few, at most a day old) that are older than after, and that nobody has claimed or
// answered, and that were not withdrawn or expired. It asks the nodes' receipts for each.
func Unanswered(c *client.Client, host string, after time.Duration, now time.Time) ([]Stuck, error) {
	var out []Stuck
	for _, s := range outbox.SentFrom(host) {
		if now.Sub(s.At) < after {
			continue
		}
		r, err := c.Receipts(s.ID)
		if err != nil {
			if client.IsUnreachable(err) {
				return out, err
			}
			continue // gone from the nodes, or an older node: nothing to say about it
		}
		if r.Kind != msg.KindTask || r.Owner != nil || r.Replies > 0 || r.Expired {
			continue
		}
		if ms, err := c.Thread(s.ID); err == nil && len(ms) > 0 && ms[0].ID == s.ID && ms[0].Retracted != nil {
			continue // taken back (task --fallback moved it on)
		}
		out = append(out, Stuck{ID: s.ID, From: s.From, To: r.To, At: s.At})
	}
	return out, nil
}

// AlertText is one stuck task as a line for a person (a phone notification, a log).
func AlertText(s Stuck, now time.Time) string {
	return fmt.Sprintf("aivoicebus: task %s to %s has not been taken after %s (sent by %s). aivoicebus status %s",
		s.ID, msg.SafeName(s.To), outbox.SpanText(now.Sub(s.At).Round(time.Minute)), msg.SafeName(s.From), s.ID)
}

// Notify posts text as a plain-text body to url, the way ntfy and most webhook relays take it.
// The text names ids and machines only, never what the task says.
func Notify(url, text string) error {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(text))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", "aivoicebus: a task is waiting")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("the notify URL's host %s did not answer", req.URL.Host) // the error would quote the URL
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the notify URL's host %s answered %s", req.URL.Host, resp.Status) // the URL itself may be a secret
	}
	return nil
}
