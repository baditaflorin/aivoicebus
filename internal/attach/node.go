package attach

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Node is the mesh's own file store: a node holds the encrypted file in memory for a few
// minutes (see the server's /v1/files). Its links are https://NODE/v1/files/ID#KEY, where NODE
// is one of this machine's configured nodes; a reader fetches only from a node it is already
// configured with, so the token never goes anywhere it does not already go. The envelope is the
// pipe's, so the same seal and open apply.
type Node struct {
	URLs  []string // this machine's nodes, in the order to try them
	Token string   // the shared or machine token; set by the caller before Put, Get or WatchRead
}

var reNodePath = regexp.MustCompile(`^/v1/files/([A-Za-z0-9_-]{22})$`)

// Name implements Provider.
func (*Node) Name() string { return "node" }

// base is the configured node u points at, or "".
func (n *Node) base(u *url.URL) string {
	for _, raw := range n.URLs {
		b, err := url.Parse(strings.TrimRight(raw, "/"))
		if err == nil && b.Host != "" && b.User == nil && b.Scheme == u.Scheme && strings.EqualFold(b.Host, u.Host) && (b.Path == "" || b.Path == "/") {
			return b.Scheme + "://" + b.Host
		}
	}
	return ""
}

// Handles reports whether u is a file link to one of this machine's own nodes.
func (n *Node) Handles(u *url.URL) bool {
	return u.User == nil && u.RawQuery == "" && reNodePath.MatchString(u.Path) &&
		len(u.Fragment) == 64 && isHex(u.Fragment) && n.base(u) != ""
}

// Describe implements Provider.
func (*Node) Describe(l Link, msgID string) string {
	name := "a file"
	if l.Name != "" {
		name = fmt.Sprintf("file %q", DisplayName(l.Name))
	}
	size := ""
	if l.Size > 0 {
		size = ", " + HumanSize(l.Size)
	}
	return fmt.Sprintf("[attachment] %s%s, encrypted, held by a node, readable once for a few minutes, fetch: aivoicebus get %s", name, size, msgID)
}

func (n *Node) do(ctx context.Context, method, u string, body []byte, timeout time.Duration) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+n.Token)
	if body != nil {
		req.Header.Set("Content-Type", "text/plain")
	}
	return httpClient(timeout).Do(req)
}

// Put implements FileStore: the first node that takes it holds it.
func (n *Node) Put(ctx context.Context, data []byte, burn bool) (Link, string, error) {
	if len(n.URLs) == 0 {
		return Link{}, "", errors.New("no node is configured (AIVOICEBUS_URL)")
	}
	key, err := newKey()
	if err != nil {
		return Link{}, "", err
	}
	env, err := seal(key, data)
	if err != nil {
		return Link{}, "", err
	}
	q := "?ttl=" + fmt.Sprint(int(PipeTTL/time.Second))
	if burn {
		q += "&burn=1"
	}
	var errs []string
	for _, raw := range n.URLs {
		b, perr := url.Parse(strings.TrimRight(raw, "/"))
		if perr != nil || b.Host == "" {
			continue
		}
		base := b.Scheme + "://" + b.Host
		resp, err := n.do(ctx, http.MethodPost, base+"/v1/files"+q, env, 2*time.Minute)
		if err != nil {
			errs = append(errs, b.Host+": "+err.Error())
			continue
		}
		msgb, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusCreated:
			var r struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(msgb, &r) != nil || !reNodePath.MatchString("/v1/files/"+r.ID) {
				return Link{}, "", fmt.Errorf("node %s answered with no usable id", b.Host)
			}
			sum := sha256.Sum256(data)
			path := base + "/v1/files/" + r.ID
			return Link{URL: path + "#" + key, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, path + "/read", nil
		case resp.StatusCode == http.StatusNotFound:
			errs = append(errs, b.Host+": does not hold files (it runs an older release)")
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return Link{}, "", fmt.Errorf("node %s refused the upload: %s %s", b.Host, resp.Status, strings.TrimSpace(Sanitize(string(msgb))))
		default:
			errs = append(errs, b.Host+": "+resp.Status+" "+strings.TrimSpace(Sanitize(string(msgb))))
		}
	}
	return Link{}, "", errors.New("no node took the file: " + strings.Join(errs, "; "))
}

// Get implements FileStore. It fetches only from the configured node the link names.
func (n *Node) Get(ctx context.Context, l Link) ([]byte, error) {
	u, err := url.Parse(l.URL)
	if err != nil || !n.Handles(u) {
		return nil, errors.New("not a link to one of this machine's nodes")
	}
	resp, err := n.do(ctx, http.MethodGet, n.base(u)+u.Path, nil, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("fetch from the node: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrGone
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("the node answered %s", resp.Status)
	}
	env, err := io.ReadAll(io.LimitReader(resp.Body, maxEnvelope+1))
	if err != nil {
		return nil, err
	}
	if len(env) > maxEnvelope {
		return nil, errors.New("the node's answer is larger than any file it accepts")
	}
	return open(u.Fragment, env)
}

// WatchRead implements FileStore: it long-polls the node, which answers about every 25s.
func (n *Node) WatchRead(ctx context.Context, watch string, wait time.Duration) (bool, error) {
	u, err := url.Parse(watch)
	if err != nil || n.base(u) == "" || !strings.HasSuffix(u.Path, "/read") || !reNodePath.MatchString(strings.TrimSuffix(u.Path, "/read")) {
		return false, errors.New("watch URL is not on one of this machine's nodes")
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		resp, err := n.do(ctx, http.MethodGet, n.base(u)+u.Path, nil, 40*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return false, nil
			}
			return false, err
		}
		var r struct {
			Read bool `json:"read"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return false, nil
		case resp.StatusCode != http.StatusOK:
			return false, fmt.Errorf("the node answered %s", resp.Status)
		}
		if json.Unmarshal(b, &r) == nil && r.Read {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, nil
		}
	}
}
