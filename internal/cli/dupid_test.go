package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// collidingMesh is a node that says "that id is held by another message" to the first send it gets, as a node
// does when another sender drew the same id, and takes every later one.
func collidingMesh(t *testing.T) *atomic.Int64 {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	var sends atomic.Int64
	h := server.NewHandler(s, "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/send" && sends.Add(1) == 1 {
			w.Header().Set(msg.HeaderRefused, msg.RefusedIDTaken)
			http.Error(w, store.ErrIDTaken.Error(), http.StatusConflict)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_ME", "imac/claude.1111")
	return &sends
}

func TestSendAfterAnIdCollisionIsAnOrdinarySendNotARepeat(t *testing.T) {
	sends := collidingMesh(t)
	code, out, e := run(t, "", "send", "--queue", "--to", "air/codex.2222", "hello there")
	if code != 0 || !strings.HasPrefix(out, "sent ") {
		t.Fatalf("one collision does not fail the send: %d %q %q", code, out, e)
	}
	if sends.Load() != 2 {
		t.Errorf("the node was asked twice: %d", sends.Load())
	}
	if strings.Contains(e, "the same words went") {
		t.Errorf("the second id is not a repeat of earlier words, and must not be called one:\n%s", e)
	}
	id := strings.Fields(out)[1]
	if !strings.Contains(e, "retract "+id) {
		t.Errorf("undo names the id that went out (%s):\n%s", id, e)
	}
}

func TestSendingTheSameWordsAgainIsStillSaidToBeARepeat(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "imac/claude.1111")
	if code, out, e := run(t, "", "send", "--queue", "--to", "air/codex.2222", "hello there"); code != 0 || !strings.HasPrefix(out, "sent ") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	code, out, e := run(t, "", "send", "--queue", "--to", "air/codex.2222", "hello there")
	if code != 0 || !strings.HasPrefix(out, "sent ") || !strings.Contains(e, "the same words went to the same target") {
		t.Errorf("a node's own dedupe is still told: %d %q\n%s", code, out, e)
	}
}
