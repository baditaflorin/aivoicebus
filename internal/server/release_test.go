package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// releaseNode starts a node serving a release directory with a manifest of the given version.
func releaseNode(t *testing.T, version string) (srv *httptest.Server, dir string) {
	t.Helper()
	dir = t.TempDir()
	writeManifest(t, dir, version, time.Now())
	os.WriteFile(filepath.Join(dir, "aivoicebus-linux-amd64"), []byte("the binary"), 0o755)
	os.WriteFile(filepath.Join(dir, "manifest.sig"), []byte("c2ln\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "README.txt"), []byte("not served"), 0o644)
	s, _ := store.Open(t.TempDir(), "n1")
	srv = httptest.NewServer(NewHandler(s, "tok", WithReleaseDir(dir)))
	t.Cleanup(srv.Close)
	return srv, dir
}

func writeManifest(t *testing.T, dir, version string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(dir, "manifest.json")
	os.WriteFile(p, []byte(`{"schema":1,"version":"`+version+`","files":{}}`), 0o644)
	os.Chtimes(p, mtime, mtime)
}

func get(t *testing.T, srv *httptest.Server, path, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestReleaseFilesNeedTheTokenAndAreServedVerbatim(t *testing.T) {
	srv, _ := releaseNode(t, "0.3.0")
	if resp, _ := get(t, srv, "/v1/release/manifest.json", ""); resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	if resp, _ := get(t, srv, "/v1/release/manifest.json", "wrong"); resp.StatusCode != 401 {
		t.Errorf("wrong token: %d", resp.StatusCode)
	}
	resp, body := get(t, srv, "/v1/release/aivoicebus-linux-amd64", "tok")
	if resp.StatusCode != 200 || body != "the binary" {
		t.Errorf("binary: %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("release files must not be cached")
	}
	if _, body := get(t, srv, "/v1/release/manifest.sig", "tok"); body != "c2ln\n" {
		t.Errorf("signature: %q", body)
	}
}

func TestOnlyReleaseFileNamesAreServed(t *testing.T) {
	srv, _ := releaseNode(t, "0.3.0")
	for _, path := range []string{
		"/v1/release/", "/v1/release/README.txt", "/v1/release/..%2fmanifest.json", "/v1/release/%2e%2e/%2e%2e/etc/passwd",
		"/v1/release/aivoicebus-linux-amd64/", "/v1/release/aivoicebus-darwin-arm64", "/v1/release/MANIFEST.JSON",
	} {
		if resp, body := get(t, srv, path, "tok"); resp.StatusCode == 200 {
			t.Errorf("%s was served: %q", path, body)
		}
	}
}

func TestNoReleaseDirMeansNothingToServeAndNoHeader(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "tok"))
	defer srv.Close()
	resp, _ := get(t, srv, "/v1/release/manifest.json", "tok")
	if resp.StatusCode != 404 {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
	if resp, _ := get(t, srv, "/v1/who", "tok"); resp.Header.Get(msg.HeaderLatest) != "" {
		t.Error("a node without a release must not announce one")
	}
}

func TestLatestHeaderOnAuthenticatedResponsesAndFollowsTheManifest(t *testing.T) {
	srv, dir := releaseNode(t, "0.3.0")
	if resp, _ := get(t, srv, "/v1/who", "tok"); resp.Header.Get(msg.HeaderLatest) != "0.3.0" {
		t.Errorf("header = %q", resp.Header.Get(msg.HeaderLatest))
	}
	if resp, _ := get(t, srv, "/v1/who", ""); resp.Header.Get(msg.HeaderLatest) != "" {
		t.Error("an unauthenticated response must not leak the release number")
	}
	if resp, _ := get(t, srv, "/healthz", ""); resp.Header.Get(msg.HeaderLatest) != "" {
		t.Error("/healthz carries no release hint")
	}
	writeManifest(t, dir, "0.3.1", time.Now().Add(time.Minute))
	if resp, _ := get(t, srv, "/v1/who", "tok"); resp.Header.Get(msg.HeaderLatest) != "0.3.1" {
		t.Errorf("after the manifest changed, header = %q", resp.Header.Get(msg.HeaderLatest))
	}
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("garbage"), 0o644)
	os.Chtimes(filepath.Join(dir, "manifest.json"), time.Now().Add(2*time.Minute), time.Now().Add(2*time.Minute))
	if resp, _ := get(t, srv, "/v1/who", "tok"); resp.Header.Get(msg.HeaderLatest) != "" {
		t.Error("an unreadable manifest must not produce a hint")
	}
}

func TestReleaseRoutesAreReadOnly(t *testing.T) {
	srv, _ := releaseNode(t, "0.3.0")
	req, _ := http.NewRequest("PUT", srv.URL+"/v1/release/manifest.json", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT status %d, want 405", resp.StatusCode)
	}
}
