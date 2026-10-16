package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

const goodToken = "good-token-0123456789-0123456789"

// setUpMachine configures a scratch HOME that passes `doctor --offline`: node URL, token file,
// hook entries, an "installed" binary. The environment then names url and the token the
// shell holds, which may differ from the file's (a retired token still in the shell).
func setUpMachine(t *testing.T, url, shellToken string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_URL", url)
	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_ME", "imac/claude")
	if code, out, e := run(t, "", "setup", "--yes", "--url", url); code != 0 {
		t.Fatalf("setup: %d\n%s\n%s", code, out, e)
	}
	os.MkdirAll(filepath.Join(home, ".local/bin"), 0o755)
	os.WriteFile(filepath.Join(home, ".local/bin/aivoicebus"), []byte("x"), 0o755)
	os.WriteFile(filepath.Join(home, ".config/aivoicebus/token"), []byte(shellToken), 0o600)
	if code, out, _ := run(t, "", "doctor", "--offline"); code != 0 || strings.Contains(out, "FAIL") {
		t.Fatalf("the scratch machine should pass offline: %d\n%s", code, out)
	}
	t.Setenv("AIVOICEBUS_TOKEN", shellToken)
}

func nodeHandler(t *testing.T) http.Handler {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	return server.NewHandler(s, goodToken)
}

// A node from before /v1/token answers 404 to it, whoever asks.
func oldNode(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/token" {
			http.NotFound(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func TestDoctorFailsOnARejectedTokenAndOnlyWarnsWhenTheMeshIsAway(t *testing.T) {
	retired := "retired-token-0123456789-012345"
	cases := []struct {
		name    string
		handler func(*testing.T) http.Handler // nil: nothing listens
		token   string
		code    int
		want    []string
	}{
		{"accepted", nodeHandler, goodToken, 0, []string{"ok    mesh", "reachable"}},
		{"rejected", nodeHandler, retired, 1, []string{"FAIL  mesh", "rejected this machine's token", "401"}},
		{"nobody answers", nil, goodToken, 0, []string{"WARN  mesh", "unreachable"}},
		{"old node, accepted", func(t *testing.T) http.Handler { return oldNode(nodeHandler(t)) }, goodToken, 0, []string{"ok    mesh", "reachable"}},
		{"old node, rejected", func(t *testing.T) http.Handler { return oldNode(nodeHandler(t)) }, retired, 1, []string{"FAIL  mesh", "rejected this machine's token", "401"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url := "http://127.0.0.1:1" // nothing listens there
			if c.handler != nil {
				srv := httptest.NewServer(c.handler(t))
				t.Cleanup(srv.Close)
				url = srv.URL
			}
			setUpMachine(t, url, c.token)
			code, out, e := run(t, "", "doctor")
			if code != c.code {
				t.Errorf("exit %d, want %d\n%s%s", code, c.code, out, e)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			if strings.Contains(out, c.token) {
				t.Errorf("doctor printed the token:\n%s", out)
			}
		})
	}
}

func TestDoctorShowsWhatTheHooksFoundButTheLiveProbeDecidesTheExitCode(t *testing.T) {
	srv := httptest.NewServer(nodeHandler(t))
	defer srv.Close()
	setUpMachine(t, srv.URL, goodToken)
	st := config.HookStatus{LastRun: time.Now().Add(-5 * time.Minute), Outcome: config.HookRejected, Failures: 2, Node: "n1:7777"}
	if err := st.Save(config.HookStatusPath()); err != nil {
		t.Fatal(err)
	}
	// the token works now (it was fixed since the hook last ran): a note, not a failure
	code, out, _ := run(t, "", "doctor")
	if code != 0 || !strings.Contains(out, "WARN  hook runs") || !strings.Contains(out, "ok    mesh") {
		t.Errorf("%d\n%s", code, out)
	}
	if code, out, _ := run(t, "", "doctor", "--offline"); code != 0 || !strings.Contains(out, "WARN  hook runs") {
		t.Errorf("offline: %d\n%s", code, out)
	}
}
