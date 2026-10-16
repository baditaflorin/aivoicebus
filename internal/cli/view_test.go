package cli

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestTheViewerListensOnLoopbackOnly(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:7788", "[::1]:7788", "localhost:0"} {
		if err := loopbackOnly(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:7788", ":7788", "10.0.0.5:7788", "example.com:80", "7788"} {
		if err := loopbackOnly(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if code, _, e := run(t, "", "view", "--listen", "0.0.0.0:7788", "--demo"); code == 0 || !strings.Contains(e, "not a loopback address") {
		t.Errorf("%d %q", code, e)
	}
}

func TestTheViewerWantsTheObserverToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := store.Open(t.TempDir(), "n1")
	full, obs := "full-token-0123456789-0123456789", "observer-token-0123456789-012345"
	srv := httptest.NewServer(server.NewHandler(s, full, server.WithObserverToken(obs)))
	defer srv.Close()
	dir := t.TempDir()
	write := func(name, tok string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(tok+"\n"), mode)
		return p
	}
	if _, err := observerClient(srv.URL, write("full", full, 0o600), false, Streams{Err: os.Stderr}); err == nil || !strings.Contains(err.Error(), "not only observe") {
		t.Errorf("a full token: %v", err)
	}
	if _, err := observerClient(srv.URL, write("full2", full, 0o600), true, Streams{Err: &strings.Builder{}}); err != nil {
		t.Errorf("a full token with --allow-full-token: %v", err)
	}
	if _, err := observerClient(srv.URL, write("loose", obs, 0o644), false, Streams{Err: os.Stderr}); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a token file others can read: %v", err)
	}
	if c, err := observerClient(srv.URL, write("obs", obs, 0o600), false, Streams{Err: os.Stderr}); err != nil || c == nil {
		t.Errorf("the observer token: %v", err)
	}
	if _, err := observerClient("", write("obs2", obs, 0o600), false, Streams{Err: os.Stderr}); err == nil || !strings.Contains(err.Error(), "--demo") {
		t.Errorf("no node: %v", err)
	}
}
