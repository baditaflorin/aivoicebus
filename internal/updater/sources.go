package updater

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/release"
)

// MeshSource reads the release from the mesh nodes, over the same authenticated
// connection the rest of aivoicebus uses.
type MeshSource struct{ C *client.Client }

func (m MeshSource) Describe() string { return "the mesh nodes" }

func (m MeshSource) Fetch(name string, limit int64) ([]byte, error) {
	return m.C.FetchRelease(name, limit)
}

// githubHosts are the only hosts a release download may be served from: github.com itself, and the
// hosts GitHub redirects release assets to.
var githubHosts = map[string]bool{
	"github.com":                            true,
	"objects.githubusercontent.com":         true,
	"release-assets.githubusercontent.com":  true,
	"github-releases.githubusercontent.com": true,
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// GitHubSource reads a release from the public GitHub release of Repo over HTTPS: no gh command, no
// login, no credential of any kind is sent. GitHub only transports the files; nothing is trusted
// until the signature and hashes have been checked, so a hostile mirror or a swapped asset fails
// verification. The manifest comes from the latest release; the signature and the binary are then
// taken from that same release's tag, so a release published while an update runs cannot mix files.
type GitHubSource struct {
	Repo string // owner/name

	// base and client are for tests; the zero values are https://github.com and a client that follows
	// redirects only over https and only to GitHub's own hosts.
	base   string
	client *http.Client
	tag    string
}

func (g *GitHubSource) Describe() string { return "the GitHub release of " + g.Repo }

func (g *GitHubSource) httpClient() *http.Client {
	if g.client != nil {
		return g.client
	}
	return &http.Client{
		Timeout: 3 * time.Minute, // a binary is several MB, and the timeout covers the whole transfer
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" || !githubHosts[req.URL.Hostname()] {
				return fmt.Errorf("refusing a redirect to %s: release files come from GitHub over https only", req.URL.Redacted())
			}
			return nil
		},
	}
}

func (g *GitHubSource) Fetch(name string, limit int64) ([]byte, error) {
	if !repoPattern.MatchString(g.Repo) {
		return nil, fmt.Errorf("%q is not a GitHub repository (owner/name)", g.Repo)
	}
	if !release.ValidName(name) {
		return nil, fmt.Errorf("%q is not a release file name", name)
	}
	base := g.base
	if base == "" {
		base = "https://github.com"
	}
	path := "/" + g.Repo + "/releases/latest/download/" + url.PathEscape(name)
	if name != release.ManifestName && g.tag != "" {
		path = "/" + g.Repo + "/releases/download/" + url.PathEscape(g.tag) + "/" + url.PathEscape(name)
	}
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "aivoicebus-update")
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s from GitHub: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		hint := ""
		if resp.StatusCode == http.StatusNotFound {
			hint = fmt.Sprintf("\nhint: %s has no published release with that file, or the repository is private or does not exist. Releases are read from https://github.com/%s/releases without logging in.", g.Repo, g.Repo)
		}
		return nil, fmt.Errorf("GitHub answered %s for %s%s", resp.Status, name, hint)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s from GitHub: %w", name, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	if name == release.ManifestName {
		if v, err := release.ManifestVersion(b); err == nil {
			g.tag = "v" + v
		}
	}
	return b, nil
}
