package updater

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// A fake binary is a file whose content is "aivoicebus <version>"; the fake
// runner answers `version` from that content and `setup` with "setup ok".
type harness struct {
	t     *testing.T
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
	dir   string // release directory
	bin   string // installed binary
	calls []string
	setup error // what a `setup` run returns
}

func newHarness(t *testing.T, installed, released string) *harness {
	t.Helper()
	priv, pub, err := release.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, pub: pub, priv: priv, dir: t.TempDir(), bin: filepath.Join(t.TempDir(), "aivoicebus")}
	if installed != "" {
		os.WriteFile(h.bin, []byte("aivoicebus "+installed), 0o755)
	}
	h.publish(released, "aivoicebus "+released)
	return h
}

// publish (re)packs the release directory with the given version and binary content.
func (h *harness) publish(version, content string) {
	h.t.Helper()
	os.WriteFile(filepath.Join(h.dir, "aivoicebus-linux-amd64"), []byte(content), 0o755)
	if _, err := release.Pack(release.PackOptions{Dir: h.dir, Version: version, Key: h.priv}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) runner(bin string, args ...string) (string, error) {
	h.calls = append(h.calls, filepath.Base(bin)+" "+strings.Join(args, " "))
	switch args[0] {
	case "version":
		b, err := os.ReadFile(bin)
		if err != nil {
			return "", err
		}
		return string(b) + " (commit x, linux/amd64)\n", nil
	case "setup":
		return "setup ok\n", h.setup
	}
	return "", errors.New("unexpected " + args[0])
}

func (h *harness) options(current string) (Options, *bytes.Buffer) {
	var out bytes.Buffer
	return Options{Source: release.DirSource{Dir: h.dir}, Trusted: []ed25519.PublicKey{h.pub}, Current: current,
		Bin: h.bin, GOOS: "linux", GOARCH: "amd64", Run: h.runner, Out: &out}, &out
}

func (h *harness) installed() string {
	b, _ := os.ReadFile(h.bin)
	return string(b)
}

func noStaging(t *testing.T, bin string) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(bin), ".aivoicebus-update-*"))
	if len(left) != 0 {
		t.Errorf("staging files left behind: %v", left)
	}
}

func TestUpdateInstallsKeepsThePreviousAndRunsSetup(t *testing.T) {
	h := newHarness(t, "0.2.0", "0.3.0")
	o, out := h.options("0.2.0")
	res, err := Update(o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.Latest != "0.3.0" || h.installed() != "aivoicebus 0.3.0" {
		t.Fatalf("result %+v, installed %q", res, h.installed())
	}
	if b, _ := os.ReadFile(h.bin + ".prev"); string(b) != "aivoicebus 0.2.0" {
		t.Errorf(".prev = %q", b)
	}
	if fi, _ := os.Stat(h.bin); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %04o", fi.Mode().Perm())
	}
	noStaging(t, h.bin)
	// the staged binary was proven to run BEFORE the swap; setup ran on the installed one AFTER
	want := []string{"version", "setup"}
	if len(h.calls) != 2 || !strings.HasSuffix(h.calls[0], " "+want[0]) || h.calls[1] != "aivoicebus setup" {
		t.Errorf("calls: %v", h.calls)
	}
	if !strings.Contains(out.String(), "installed aivoicebus 0.3.0") || !strings.Contains(out.String(), "setup ok") {
		t.Errorf("output: %s", out)
	}
}

func TestNothingToDoWhenCurrentOrNewer(t *testing.T) {
	h := newHarness(t, "0.3.0", "0.3.0")
	o, out := h.options("0.3.0")
	res, err := Update(o)
	if err != nil || res.Updated || res.Available || !strings.Contains(out.String(), "up to date") {
		t.Fatalf("same version: %+v %v %s", res, err, out)
	}
	// an older signed release is never installed by default (no downgrade by replay)
	h.publish("0.2.0", "aivoicebus 0.2.0")
	o, out = h.options("0.3.0")
	if res, err = Update(o); err != nil || res.Updated || h.installed() != "aivoicebus 0.3.0" {
		t.Fatalf("older release must not install: %+v %v", res, err)
	}
	// ...unless forced, which is explicit
	o, out = h.options("0.3.0")
	o.Force = true
	if res, err = Update(o); err != nil || !res.Updated || h.installed() != "aivoicebus 0.2.0" || !strings.Contains(out.String(), "--force") {
		t.Fatalf("forced downgrade: %+v %v %s", res, err, out)
	}
}

func TestCheckChangesNothing(t *testing.T) {
	h := newHarness(t, "0.2.0", "0.3.0")
	o, out := h.options("0.2.0")
	o.Check = true
	res, err := Update(o)
	if err != nil || !res.Available || res.Updated || h.installed() != "aivoicebus 0.2.0" || len(h.calls) != 0 {
		t.Fatalf("%+v %v calls=%v", res, err, h.calls)
	}
	if !strings.Contains(out.String(), "0.3.0 is available") {
		t.Errorf("output: %s", out)
	}
	noStaging(t, h.bin)
}

func TestADevBuildCanInstallARelease(t *testing.T) {
	h := newHarness(t, "", "0.3.0")
	o, _ := h.options("dev")
	if res, err := Update(o); err != nil || !res.Updated {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(h.bin + ".prev"); err == nil {
		t.Error("a fresh install has no previous binary to keep")
	}
}

func TestRefusesWithoutChangingAnythingWhenTheSignatureIsNotTrusted(t *testing.T) {
	h := newHarness(t, "0.2.0", "0.3.0")
	_, other, _ := release.GenerateKey()
	o, _ := h.options("0.2.0")
	o.Trusted = []ed25519.PublicKey{other}
	if _, err := Update(o); !errors.Is(err, release.ErrBadSignature) {
		t.Fatalf("wrong key: %v", err)
	}
	o.Trusted = nil
	if _, err := Update(o); !errors.Is(err, release.ErrNoTrustedKeys) {
		t.Fatalf("no keys: %v", err)
	}
	if h.installed() != "aivoicebus 0.2.0" || len(h.calls) != 0 {
		t.Error("an untrusted release must not touch the installed binary or run anything")
	}
	noStaging(t, h.bin)
}

func TestRefusesATamperedManifestOrBinary(t *testing.T) {
	// manifest edited after signing (a replayed signature on different content)
	h := newHarness(t, "0.2.0", "0.3.0")
	mp := filepath.Join(h.dir, release.ManifestName)
	mb, _ := os.ReadFile(mp)
	os.WriteFile(mp, bytes.Replace(mb, []byte("0.3.0"), []byte("9.9.9"), 1), 0o644)
	o, _ := h.options("0.2.0")
	if _, err := Update(o); !errors.Is(err, release.ErrBadSignature) {
		t.Fatalf("tampered manifest: %v", err)
	}

	// binary swapped after signing: the signed hash catches it
	h = newHarness(t, "0.2.0", "0.3.0")
	os.WriteFile(filepath.Join(h.dir, "aivoicebus-linux-amd64"), []byte("aivoicebus 0.3.0 plus malware"), 0o755)
	o, _ = h.options("0.2.0")
	_, err := Update(o)
	if err == nil || !strings.Contains(err.Error(), "does not match the signed manifest") {
		t.Fatalf("tampered binary: %v", err)
	}
	if h.installed() != "aivoicebus 0.2.0" || len(h.calls) != 0 {
		t.Error("nothing may run or change when the hash is wrong")
	}
	noStaging(t, h.bin)
}

func TestAbortsBeforeTheSwapIfTheNewBinaryCannotRunOrLiesAboutItsVersion(t *testing.T) {
	// reports a different version than the signed manifest says
	h := newHarness(t, "0.2.0", "0.3.0")
	h.publish("0.3.0", "aivoicebus 9.9.9")
	o, _ := h.options("0.2.0")
	if _, err := Update(o); err == nil || !strings.Contains(err.Error(), "different version") {
		t.Fatalf("version mismatch: %v", err)
	}
	if h.installed() != "aivoicebus 0.2.0" {
		t.Error("the installed binary must be untouched")
	}
	noStaging(t, h.bin)

	// cannot run on this machine at all
	h = newHarness(t, "0.2.0", "0.3.0")
	o, _ = h.options("0.2.0")
	o.Run = func(string, ...string) (string, error) { return "", errors.New("exec format error") }
	if _, err := Update(o); err == nil || !strings.Contains(err.Error(), "does not run here") {
		t.Fatalf("unrunnable binary: %v", err)
	}
	if h.installed() != "aivoicebus 0.2.0" {
		t.Error("the installed binary must be untouched")
	}
	noStaging(t, h.bin)
}

func TestReleaseWithoutABuildForThisPlatform(t *testing.T) {
	h := newHarness(t, "0.2.0", "0.3.0")
	o, _ := h.options("0.2.0")
	o.GOOS = "plan9"
	if _, err := Update(o); err == nil || !strings.Contains(err.Error(), "no build for plan9/amd64") {
		t.Fatalf("%v", err)
	}
}

func TestSetupFailureAfterTheSwapIsReportedNotFatal(t *testing.T) {
	h := newHarness(t, "0.2.0", "0.3.0")
	h.setup = errors.New("boom")
	o, out := h.options("0.2.0")
	res, err := Update(o)
	if err != nil || !res.Updated || h.installed() != "aivoicebus 0.3.0" {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(out.String(), "doctor --fix") {
		t.Errorf("the user must be told how to repair: %s", out)
	}
}

func TestRollbackRestoresThePreviousBinary(t *testing.T) {
	h := newHarness(t, "0.2.0", "0.3.0")
	o, _ := h.options("0.2.0")
	if _, err := Update(o); err != nil {
		t.Fatal(err)
	}
	h.calls = nil
	var out bytes.Buffer
	if err := Rollback(h.bin, h.runner, &out); err != nil {
		t.Fatal(err)
	}
	if h.installed() != "aivoicebus 0.2.0" || !strings.Contains(out.String(), "rolled back to aivoicebus 0.2.0") {
		t.Errorf("installed %q, output %s", h.installed(), out.String())
	}
	noStaging(t, h.bin)
	// no .prev: nothing to roll back to
	os.Remove(h.bin + ".prev")
	if err := Rollback(h.bin, h.runner, &out); err == nil {
		t.Error("rollback without a previous binary must fail")
	}
}

func TestMeshSourceEndToEndWithFailoverAcrossNodes(t *testing.T) {
	h := newHarness(t, "0.2.0", "0.3.0")
	// node A has no release yet (mid-rollout), node B serves it
	sa, _ := store.Open(t.TempDir(), "a")
	sb, _ := store.Open(t.TempDir(), "b")
	nodeA := httptest.NewServer(server.NewHandler(sa, "tok"))
	nodeB := httptest.NewServer(server.NewHandler(sb, "tok", server.WithReleaseDir(h.dir)))
	defer nodeA.Close()
	defer nodeB.Close()
	c := &client.Client{URLs: []string{nodeA.URL, nodeB.URL}, Token: "tok"}

	o, _ := h.options("0.2.0")
	o.Source = MeshSource{C: c}
	res, err := Update(o)
	if err != nil || !res.Updated || h.installed() != "aivoicebus 0.3.0" {
		t.Fatalf("%+v %v", res, err)
	}
	if c.Latest != "0.3.0" {
		t.Errorf("the client should have learned the latest release from the node, got %q", c.Latest)
	}
}

func TestGitHubSourceTakesTheSignatureAndBinaryFromTheManifestsRelease(t *testing.T) {
	h := newHarness(t, "0.2.0", "0.3.0")
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		parts := strings.Split(r.URL.Path, "/")
		b, err := os.ReadFile(filepath.Join(h.dir, parts[len(parts)-1]))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer srv.Close()
	o, _ := h.options("0.2.0")
	o.Source = &GitHubSource{Repo: "o/r", base: srv.URL, client: srv.Client()}
	if res, err := Update(o); err != nil || !res.Updated {
		t.Fatalf("%+v %v", res, err)
	}
	has := func(path, s string) bool { return strings.Contains(path, s) }
	if len(seen) != 3 || !has(seen[0], "/releases/latest/download/manifest.json") || has(seen[0], "v0.3.0") ||
		!has(seen[1], "/releases/download/v0.3.0/") || !has(seen[2], "/releases/download/v0.3.0/") {
		t.Errorf("the manifest comes from the latest release, then everything from its tag: %v", seen)
	}
	// a missing release and an oversize answer are both errors
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	o.Source = &GitHubSource{Repo: "o/r", base: missing.URL, client: missing.Client()}
	if _, err := Update(o); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a missing release: %v", err)
	}
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 100)) }))
	defer big.Close()
	if _, err := (&GitHubSource{Repo: "o/r", base: big.URL, client: big.Client()}).Fetch("manifest.json", 10); err == nil {
		t.Error("oversize answer accepted")
	}
}
