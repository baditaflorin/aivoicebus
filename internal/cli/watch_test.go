package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
)

// A task this machine sent that nobody took is alerted on once, as a line and a POST to the notify
// URL that names the task, never its text; a taken or withdrawn one is not.
func TestWatchAlertsOnceOnAnUntakenTask(t *testing.T) {
	mesh(t)
	var mu sync.Mutex
	var posts []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts = append(posts, string(b))
		mu.Unlock()
	}))
	defer hook.Close()
	t.Setenv("AIVOICEBUS_NOTIFY_URL", hook.URL)
	stuck := sendTask(t, boss, "lap", "rotate the secret plan")
	taken := sendTask(t, boss, "box", "taken one")
	gone := sendTask(t, boss, "far", "withdrawn one")
	t.Setenv("AIVOICEBUS_ME", sa)
	run(t, "", "claim", taken) // not lap's, but a claim is a claim
	t.Setenv("AIVOICEBUS_ME", boss)
	if code, _, e := run(t, "", "retract", gone); code != 0 {
		t.Fatalf("retract: %s", e)
	}
	c, _ := client.FromEnv()
	var out strings.Builder
	n, err := watchOnce(c, 0, 0, &out)
	if err != nil || n != 1 || !strings.Contains(out.String(), "task "+stuck+" to lap has not been taken") {
		t.Fatalf("first look: %d %v\n%s", n, err, out.String())
	}
	mu.Lock()
	if len(posts) != 1 || !strings.Contains(posts[0], stuck) || strings.Contains(posts[0], "secret plan") {
		t.Errorf("notify: %q", posts)
	}
	mu.Unlock()
	if n, _ := watchOnce(c, 0, 0, io.Discard); n != 0 {
		t.Errorf("alerted twice: %d", n)
	}
	if code, _, e := run(t, "", "watch", "--once"); code != 0 || e != "" {
		t.Errorf("nothing older than 15m: %d %q", code, e)
	}
}
