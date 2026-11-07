package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

type tokenWaitingBarrierKey struct{}

// The hand can observe accepted progress before its POST response reaches the
// installer. Its stop handler must already be armed throughout that callback.
func TestTokenInstallSignalDuringAcceptedWaitingProgress(t *testing.T) {
	n := tokenMachine(t, nil)
	if _, e := config.CreateApprover("synthetic fixture person passphrase"); e != nil {
		t.Fatal(e)
	}
	task := n.handTask(t, "hands air its token; stop while progress response is in flight")
	target, e := url.Parse(n.url)
	if e != nil {
		t.Fatal(e)
	}
	accepted, unblock := make(chan struct{}), make(chan struct{})
	var acceptedOnce, unblockOnce sync.Once
	release := func() { unblockOnce.Do(func() { close(unblock) }) }
	reverse := httputil.NewSingleHostReverseProxy(target)
	reverse.ModifyResponse = func(r *http.Response) error {
		if held, _ := r.Request.Context().Value(tokenWaitingBarrierKey{}).(bool); held {
			acceptedOnce.Do(func() { close(accepted) })
			select {
			case <-unblock:
			case <-r.Request.Context().Done():
				return r.Request.Context().Err()
			}
		}
		return nil
	}
	reverse.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "synthetic proxy request ended", http.StatusBadGateway)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/send" {
			raw, e := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if e != nil {
				http.Error(w, "fixture read", 400)
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var m msg.Message
			if json.Unmarshal(raw, &m) == nil && m.Kind == msg.KindProgress && strings.HasPrefix(m.Body, tokenWaitingTag+" air") {
				r = r.WithContext(context.WithValue(r.Context(), tokenWaitingBarrierKey{}, true))
			}
		}
		reverse.ServeHTTP(w, r)
	}))
	cmd := asCLI(t, os.Getenv("HOME"), "air/claude.1", proxy.URL, "token", "install", task.ID, "--wait", "2m")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if e := cmd.Start(); e != nil {
		release()
		proxy.Close()
		t.Fatal(e)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	defer func() {
		release()
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("owned installer did not reap")
		}
		proxy.Close()
	}()
	select {
	case <-accepted:
	case <-done:
		t.Fatalf("installer exited before waiting progress: %v %s", waitErr, output.String())
	case <-time.After(30 * time.Second):
		t.Fatal("waiting progress never reached controlled response barrier")
	}
	if e := cmd.Process.Signal(syscall.SIGTERM); e != nil {
		t.Fatal(e)
	}
	// Keep the response withheld while the signal is delivered. The old code
	// exits here with default SIGTERM because registration followed the callback.
	select {
	case <-done:
		release()
		t.Fatalf("SIGTERM killed installer inside waiting callback: %v %s", waitErr, output.String())
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("installer did not stop after callback completed")
	}
	if waitErr == nil {
		t.Fatal("stopped installer exited successfully")
	}
	failed := n.inThread(t, task, tokenFailedTag+" air", 5*time.Second)
	if failed.ID == "" || !strings.Contains(failed.Body, "stopped") || msg.HoldReason(failed) != "" {
		t.Fatalf("hand did not receive visible stop notification: %+v %s", failed, output.String())
	}
	if tokenNow(t) != thShared {
		t.Fatal("stopped installer changed token")
	}
	noTempTokens(t)
}
