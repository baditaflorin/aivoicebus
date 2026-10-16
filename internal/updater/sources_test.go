package updater

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/release"
)

// githubStub serves what github.com serves for a release: the latest manifest, and the tagged assets.
func githubStub(t *testing.T, files map[string]string) (*GitHubSource, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"|auth="+r.Header.Get("Authorization")+"|cookie="+r.Header.Get("Cookie"))
		if body, ok := files[r.URL.Path]; ok {
			w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return &GitHubSource{Repo: "o/r", base: srv.URL, client: srv.Client()}, &seen
}

func TestGitHubSourceReadsTheLatestManifestThenThatTagsFiles(t *testing.T) {
	g, seen := githubStub(t, map[string]string{
		"/o/r/releases/latest/download/manifest.json":          `{"version":"1.2.3","files":[]}`,
		"/o/r/releases/download/v1.2.3/manifest.sig":           "SIG",
		"/o/r/releases/download/v1.2.3/aivoicebus-linux-amd64": "BIN",
	})
	if b, err := g.Fetch(release.ManifestName, 1<<20); err != nil || !strings.Contains(string(b), "1.2.3") {
		t.Fatalf("manifest: %q %v", b, err)
	}
	for name, want := range map[string]string{"manifest.sig": "SIG", "aivoicebus-linux-amd64": "BIN"} {
		if b, err := g.Fetch(name, 1<<20); err != nil || string(b) != want {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
	for _, line := range *seen {
		if !strings.HasSuffix(line, "|auth=|cookie=") {
			t.Errorf("a credential header was sent: %s", line)
		}
	}
}

func TestGitHubSourceSaysWhatANotFoundMeans(t *testing.T) {
	g, _ := githubStub(t, nil)
	_, err := g.Fetch(release.ManifestName, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "without logging in") || strings.Contains(err.Error(), "gh auth") {
		t.Errorf("%v", err)
	}
}

func TestGitHubSourceRefusesAnOversizedFile(t *testing.T) {
	g, _ := githubStub(t, map[string]string{"/o/r/releases/latest/download/manifest.json": strings.Repeat("x", 100)})
	if _, err := g.Fetch(release.ManifestName, 10); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("%v", err)
	}
}

func TestGitHubSourceRefusesBadRepositoriesAndNames(t *testing.T) {
	for _, repo := range []string{"", "o", "o/r/extra", "o/r?x=1", "../r", "o/ r", "o\\r", "-o/r"} {
		if _, err := (&GitHubSource{Repo: repo}).Fetch(release.ManifestName, 10); err == nil {
			t.Errorf("repository %q accepted", repo)
		}
	}
	if _, err := (&GitHubSource{Repo: "o/r"}).Fetch("../etc/passwd", 10); err == nil {
		t.Error("a path in the file name accepted")
	}
}

// Real redirects go to objects.githubusercontent.com; anything else, or anything not https, is refused.
func TestGitHubSourceFollowsRedirectsOnlyToGitHubOverHTTPS(t *testing.T) {
	for _, to := range []string{"https://evil.example/x", "http://objects.githubusercontent.com/x", "https://github.com.evil.example/x"} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, to, http.StatusFound)
		}))
		g := &GitHubSource{Repo: "o/r", base: srv.URL}
		c := srv.Client()
		c.CheckRedirect = (&GitHubSource{}).httpClient().CheckRedirect // the real policy, the test server's certificate
		g.client = c
		_, err := g.Fetch(release.ManifestName, 10)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "refusing a redirect") {
			t.Errorf("redirect to %s: %v", to, err)
		}
	}
	ok := (&GitHubSource{}).httpClient().CheckRedirect
	for _, to := range []string{"https://objects.githubusercontent.com/a", "https://release-assets.githubusercontent.com/a", "https://github.com/a"} {
		req, _ := http.NewRequest("GET", to, nil)
		if err := ok(req, nil); err != nil {
			t.Errorf("%s refused: %v", to, err)
		}
	}
}
