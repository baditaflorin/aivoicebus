package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
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

func TestReleaseNodesRetiresTheNodesRemoveNodeTookOut(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := release.SavePrivateKey(keyFile, priv); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(t.TempDir(), "nodes.conf")
	os.WriteFile(conf, []byte(strings.Join([]string{
		"node0:alias0:100.64.0.1",
		"# removed 2026-10-21T21:36:32Z database:bastion:100.64.0.9",                              // remove-node.sh --apply writes this in the node's place
		"# removed 2026-09-01T10:00:00Z old:old-ssh:100.64.0.8 gone stopped 2026-09-01T11:00:00Z", // --gone, then --stop --apply
		"# removed 2026-08-01T10:00:00Z old:old-ssh:100.64.0.8",                                   // an earlier removal of the same node: the later one counts
		"# removed 2026-07-01T10:00:00Z back:back-ssh:100.64.0.2",                                 // taken out, then added again: a node
		"back:back-ssh:100.64.0.2",
		"# removed the test box by hand, see the wiki", // a note, not a removal
		"",
	}, "\n")), 0o644)
	outDir := t.TempDir()
	code, out, e := run(t, "", "release", "nodes", "--conf", conf, "--dir", outDir, "--key", keyFile, "--serial", "50")
	if code != 0 || !strings.Contains(out, "serial 50, 2 node(s)") || !strings.Contains(out, "it retires 2 node(s)") || !strings.Contains(out, ": database, old;") {
		t.Fatalf("%d\n%s%s", code, out, e)
	}
	body, _ := os.ReadFile(filepath.Join(outDir, release.NodesName))
	sig, _ := os.ReadFile(filepath.Join(outDir, release.NodesSigName))
	n, err := release.VerifyNodes(body, sig, []ed25519.PublicKey{pub})
	want := []release.RetiredEntry{
		{Name: "database", URL: "http://100.64.0.9:7777", Since: "2026-10-21T21:36:32Z"},
		{Name: "old", URL: "http://100.64.0.8:7777", Since: "2026-09-01T10:00:00Z"},
	}
	if err != nil || !reflect.DeepEqual(n.Retired, want) || len(n.Nodes) != 2 {
		t.Fatalf("%v %+v", err, n)
	}

	// more removals than a list may carry: the newest are kept, and it says so
	var lines []string
	for i := 0; i < release.MaxRetired+3; i++ {
		lines = append(lines, fmt.Sprintf("# removed %s r%d:r%d:100.64.1.%d", time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC).Format(time.RFC3339), i, i, i))
	}
	os.WriteFile(conf, []byte("node0:alias0:100.64.0.1\n"+strings.Join(lines, "\n")+"\n"), 0o644)
	code, out, e = run(t, "", "release", "nodes", "--conf", conf, "--dir", outDir, "--key", keyFile)
	if code != 0 || !strings.Contains(out, "3 older removed node(s) left out") {
		t.Fatalf("%d\n%s%s", code, out, e)
	}
	body, _ = os.ReadFile(filepath.Join(outDir, release.NodesName))
	sig, _ = os.ReadFile(filepath.Join(outDir, release.NodesSigName))
	if n, err := release.VerifyNodes(body, sig, []ed25519.PublicKey{pub}); err != nil || len(n.Retired) != release.MaxRetired || n.Retired[0].Name != fmt.Sprintf("r%d", release.MaxRetired+2) {
		t.Errorf("%v %+v", err, n)
	}
}

func TestNodesAndMeshSayWhichConfiguredNodeTheListRetired(t *testing.T) {
	a := meshNode(t, "node-a", "0.6.0", 0, server.NewPeerBoard(nil))
	b := meshNode(t, "build", "0.6.0", 0, server.NewPeerBoard(nil))
	gone := "http://127.0.0.1:1" // nothing listens there: a node that was stopped
	pointAt(t, a, gone)          // a machine set up before the node was taken out
	if err := config.SaveDiscovered(config.Discovered{Serial: 4, Nodes: []string{a, b}, FetchedAt: time.Now(),
		Retired: []config.RetiredNode{{Name: "database", URL: gone, Since: time.Date(2026, 10, 21, 21, 36, 32, 0, time.UTC)}}}); err != nil {
		t.Fatal(err)
	}
	code, out, e := run(t, "", "mesh")
	if code != 0 || !strings.Contains(out, "mesh healthy: 2 node(s)") || !strings.Contains(out, "not checked: "+gone+" is configured here, but retired from the mesh (database, ") {
		t.Fatalf("the retired node is neither asked nor a problem: %d\n%s%s", code, out, e)
	}
	code, out, _ = run(t, "", "mesh", "--json")
	var mv meshView
	if err := json.Unmarshal([]byte(out), &mv); err != nil || code != 0 || len(mv.Nodes) != 2 || len(mv.Retired) != 1 || mv.Retired[0].URL != gone || mv.Retired[0].Used || mv.Retired[0].Since == nil {
		t.Errorf("%d %v %+v", code, err, mv)
	}
	code, out, _ = run(t, "", "nodes")
	if code != 0 || !strings.Contains(out, gone) || !strings.Contains(out, "configured, retired from the mesh (database, ") || !strings.Contains(out, ": not used") || !strings.Contains(out, b) {
		t.Errorf("%d\n%s", code, out)
	}
	code, out, _ = run(t, "", "nodes", "--json")
	var nv struct {
		Nodes   []struct{ URL string }
		Retired []retiredView
	}
	if err := json.Unmarshal([]byte(out), &nv); err != nil || code != 0 || len(nv.Nodes) != 2 || len(nv.Retired) != 1 || nv.Retired[0].Name != "database" || nv.Retired[0].Used {
		t.Errorf("%d %v %+v", code, err, nv)
	}

	// a machine configured for the retired node alone: the list does not apply, so it is still
	// used, and both commands say so
	t.Setenv("AIVOICEBUS_URL", gone)
	if _, out, _ := run(t, "", "nodes"); !strings.Contains(out, "but still used: AIVOICEBUS_URL names no node of the list") {
		t.Errorf("%s", out)
	}
	if code, out, _ := run(t, "", "mesh"); code != 1 || !strings.Contains(out, "but still used here") || !strings.Contains(out, "is unreachable") {
		t.Errorf("%d\n%s", code, out)
	}
}
