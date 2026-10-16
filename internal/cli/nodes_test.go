package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestReleaseNodesSignsTheListFromNodesConfAndNodesRefreshLearnsIt(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	old := trustedKeys
	trustedKeys = func() []ed25519.PublicKey { return []ed25519.PublicKey{pub} }
	t.Cleanup(func() { trustedKeys = old })

	// two real nodes that serve the signed list
	dir := t.TempDir()
	var urls []string
	for i := 0; i < 2; i++ {
		s, _ := store.Open(t.TempDir(), "n")
		srv := httptest.NewServer(server.NewHandler(s, "tok", server.WithReleaseDir(dir)))
		t.Cleanup(srv.Close)
		urls = append(urls, srv.URL)
	}
	// nodes.conf names nodes the way the deploy scripts do: name:alias:ip, the port added by the signer
	conf := filepath.Join(t.TempDir(), "nodes.conf")
	os.WriteFile(conf, []byte("# nodes\nnode0:alias0:100.64.0.1\nnode1:alias1:100.64.0.2\n"), 0o644)
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := release.SavePrivateKey(keyFile, priv); err != nil {
		t.Fatal(err)
	}
	// the test servers listen on loopback ports, so sign their real addresses for the refresh part
	entries := []release.NodeEntry{{Name: "node0", URL: urls[0]}, {Name: "node1", URL: urls[1]}}
	if _, err := release.PackNodes(release.PackNodesOptions{Dir: dir, Serial: 5, Nodes: entries, Key: priv}); err != nil {
		t.Fatal(err)
	}

	pointAt(t, urls[0])
	code, out, e := run(t, "", "nodes", "--refresh")
	if code != 0 || !strings.Contains(out, "node list updated (serial 5): 2 node(s), added "+urls[0]) || !strings.Contains(out, urls[1]+" ") {
		t.Fatalf("%d\n%s%s", code, out, e)
	}
	if !strings.Contains(out, urls[0]+"  ") || !strings.Contains(out, "configured") || !strings.Contains(out, "learned from the mesh") {
		t.Errorf("each node says where it came from:\n%s", out)
	}
	code, out, _ = run(t, "", "nodes", "--refresh")
	if code != 0 || !strings.Contains(out, "node list is current (serial 5)") {
		t.Errorf("a second refresh: %d %s", code, out)
	}

	// release nodes writes a list that verifies, from nodes.conf
	outDir := t.TempDir()
	code, out, e = run(t, "", "release", "nodes", "--conf", conf, "--dir", outDir, "--key", keyFile, "--port", "7777", "--serial", "42")
	if code != 0 || !strings.Contains(out, "serial 42, 2 node(s)") {
		t.Fatalf("%d %s %s", code, out, e)
	}
	body, _ := os.ReadFile(filepath.Join(outDir, release.NodesName))
	sig, _ := os.ReadFile(filepath.Join(outDir, release.NodesSigName))
	n, err := release.VerifyNodes(body, sig, []ed25519.PublicKey{pub})
	if err != nil || n.Serial != 42 || len(n.Nodes) != 2 || !strings.HasPrefix(n.Nodes[0].URL, "http://") || !strings.HasSuffix(n.Nodes[0].URL, ":7777") {
		t.Errorf("%v %+v", err, n)
	}
	if code, _, e := run(t, "", "release", "nodes", "--conf", conf); code == 0 || !strings.Contains(e, "--dir") {
		t.Errorf("release nodes needs --dir: %d %q", code, e)
	}
	bad := filepath.Join(t.TempDir(), "bad.conf")
	os.WriteFile(bad, []byte("only-two:fields\n"), 0o644)
	if code, _, e := run(t, "", "release", "nodes", "--conf", bad, "--dir", outDir, "--key", keyFile); code == 0 || !strings.Contains(e, "name:alias:ip") {
		t.Errorf("a malformed nodes.conf: %d %q", code, e)
	}
}
