package release

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	priv, pub, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

// packed makes a release directory with two binaries, signed by the returned key.
func packed(t *testing.T) (dir string, pub ed25519.PublicKey, priv ed25519.PrivateKey) {
	t.Helper()
	priv, pub = newKey(t)
	dir = t.TempDir()
	os.WriteFile(filepath.Join(dir, "aivoicebus-darwin-arm64"), []byte("arm64 binary"), 0o755)
	os.WriteFile(filepath.Join(dir, "aivoicebus-linux-amd64"), []byte("linux binary"), 0o755)
	os.WriteFile(filepath.Join(dir, "README.txt"), []byte("not a release file"), 0o644)
	if _, err := Pack(PackOptions{Dir: dir, Version: "1.2.3", Commit: "abc123", Notes: "n", Key: priv,
		Parts: map[string]string{"hook": "h1", "snippet": "s1"}, Now: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	return dir, pub, priv
}

func TestANodeListCarriesAnExpiryThatIsSignedWithIt(t *testing.T) {
	priv, pub := newKey(t)
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	list := []NodeEntry{{Name: "a", URL: "http://10.0.0.1:7777"}}
	dir := t.TempDir()
	n, err := PackNodes(PackNodesOptions{Dir: dir, Serial: 1, Nodes: list, Key: priv, Now: now})
	if err != nil || n.ValidUntil != "2027-01-12T09:00:00Z" {
		t.Fatalf("the default is 90 days: %+v %v", n, err)
	}
	if n.Expired(now.Add(89*24*time.Hour)) || !n.Expired(now.Add(91*24*time.Hour)) {
		t.Error("expiry boundaries")
	}
	body, _ := os.ReadFile(filepath.Join(dir, NodesName))
	sig, _ := os.ReadFile(filepath.Join(dir, NodesSigName))
	tampered := []byte(strings.Replace(string(body), "2027-01-12", "2099-01-12", 1))
	if _, err := VerifyNodes(tampered, sig, []ed25519.PublicKey{pub}); !errors.Is(err, ErrBadSignature) {
		t.Errorf("extending the expiry must break the signature: %v", err)
	}
	never, _ := PackNodes(PackNodesOptions{Dir: t.TempDir(), Serial: 2, Nodes: list, Key: priv, Now: now, ValidFor: -1})
	if never.ValidUntil != "" || never.Expired(now.Add(10000*24*time.Hour)) {
		t.Errorf("a negative validity means none: %+v", never)
	}
	if _, err := ParseNodes([]byte(`{"schema":1,"serial":3,"valid_until":"next tuesday","nodes":[{"name":"a","url":"http://10.0.0.1:7777"}]}`)); err == nil {
		t.Error("a garbled expiry is refused")
	}
}

func TestTrustedKeysCompiledInAllParse(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no trust-key file of the person running the tests
	if len(builtinKeys) == 0 {
		t.Fatal("no trusted key is compiled in: every update would be refused")
	}
	for _, s := range builtinKeys {
		if _, err := ParseKey(s); err != nil {
			t.Errorf("compiled-in key %q is invalid: %v", s, err)
		}
	}
	if len(TrustedKeys()) != len(builtinKeys) {
		t.Error("TrustedKeys dropped an entry")
	}
}

func TestPackThenVerifyDir(t *testing.T) {
	dir, pub, _ := packed(t)
	m, err := VerifyDir(dir, []ed25519.PublicKey{pub})
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "1.2.3" || m.Commit != "abc123" || m.Date != "2026-10-14T09:00:00Z" || m.Parts["hook"] != "h1" {
		t.Errorf("manifest: %+v", m)
	}
	if len(m.Files) != 2 {
		t.Errorf("only the two binaries belong in the manifest, got %v", m.Files)
	}
	if f := m.Files["aivoicebus-linux-amd64"]; f.SHA256 != Hash([]byte("linux binary")) || f.Size != int64(len("linux binary")) {
		t.Errorf("recorded file: %+v", f)
	}
}

func TestPackOutputIsDeterministic(t *testing.T) {
	dir, _, priv := packed(t)
	first, _ := os.ReadFile(filepath.Join(dir, ManifestName))
	if _, err := Pack(PackOptions{Dir: dir, Version: "1.2.3", Commit: "abc123", Notes: "n", Key: priv,
		Parts: map[string]string{"hook": "h1", "snippet": "s1"}, Now: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if second, _ := os.ReadFile(filepath.Join(dir, ManifestName)); string(first) != string(second) {
		t.Error("packing the same inputs twice must give the same manifest")
	}
}

func TestVerifyRejectsWrongKeyTamperedManifestAndGarbage(t *testing.T) {
	dir, pub, _ := packed(t)
	mb, _ := os.ReadFile(filepath.Join(dir, ManifestName))
	sb, _ := os.ReadFile(filepath.Join(dir, SignatureName))
	_, other := newKey(t)

	if err := Verify(mb, sb, []ed25519.PublicKey{pub}); err != nil {
		t.Fatalf("good signature rejected: %v", err)
	}
	if err := Verify(mb, sb, []ed25519.PublicKey{other}); !errors.Is(err, ErrBadSignature) {
		t.Errorf("wrong key: %v", err)
	}
	if err := Verify(mb, sb, []ed25519.PublicKey{other, pub}); err != nil {
		t.Errorf("rotation: a manifest signed by any trusted key must pass, got %v", err)
	}
	tampered := []byte(strings.Replace(string(mb), `"version": "1.2.3"`, `"version": "9.9.9"`, 1))
	if err := Verify(tampered, sb, []ed25519.PublicKey{pub}); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered manifest accepted: %v", err)
	}
	for _, junk := range []string{"", "not base64!!", "AAAA"} {
		if err := Verify(mb, []byte(junk), []ed25519.PublicKey{pub}); !errors.Is(err, ErrBadSignature) {
			t.Errorf("signature %q accepted: %v", junk, err)
		}
	}
	if err := Verify(mb, sb, nil); !errors.Is(err, ErrNoTrustedKeys) {
		t.Errorf("no keys: %v", err)
	}
}

func TestVerifyDirCatchesATamperedBinaryAndAMissingOne(t *testing.T) {
	dir, pub, _ := packed(t)
	os.WriteFile(filepath.Join(dir, "aivoicebus-linux-amd64"), []byte("evil binary"), 0o755)
	if _, err := VerifyDir(dir, []ed25519.PublicKey{pub}); err == nil || !strings.Contains(err.Error(), "does not match the signed manifest") {
		t.Errorf("tampered binary: %v", err)
	}
	os.Remove(filepath.Join(dir, "aivoicebus-linux-amd64"))
	if _, err := VerifyDir(dir, []ed25519.PublicKey{pub}); err == nil {
		t.Error("a missing binary must fail verification")
	}
}

func TestPackRefusesBadInput(t *testing.T) {
	priv, _ := newKey(t)
	dir := t.TempDir()
	if _, err := Pack(PackOptions{Dir: dir, Version: "1.0.0", Key: priv}); err == nil {
		t.Error("an empty directory has nothing to release")
	}
	os.WriteFile(filepath.Join(dir, "aivoicebus-linux-amd64"), []byte("x"), 0o755)
	for _, v := range []string{"", "dev", "1.0", "v1.0.0", "1.0.0-rc1", "01.0.0"} {
		if _, err := Pack(PackOptions{Dir: dir, Version: v, Key: priv}); err == nil {
			t.Errorf("version %q accepted", v)
		}
	}
	if _, err := Pack(PackOptions{Dir: dir, Version: "1.0.0"}); err == nil {
		t.Error("packing without a key must fail")
	}
}

func TestParseValidatesTheManifest(t *testing.T) {
	good := `{"schema":1,"version":"1.0.0","files":{"aivoicebus-linux-amd64":{"sha256":"` + strings.Repeat("a", 64) + `","size":10}}}`
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	bad := map[string]string{
		"not json":         `{`,
		"future schema":    strings.Replace(good, `"schema":1`, `"schema":2`, 1),
		"bad version":      strings.Replace(good, `1.0.0`, `1.0`, 1),
		"no files":         `{"schema":1,"version":"1.0.0","files":{}}`,
		"path in name":     strings.Replace(good, `aivoicebus-linux-amd64`, `../aivoicebus-linux-amd64`, 1),
		"name off pattern": strings.Replace(good, `aivoicebus-linux-amd64`, `evil`, 1),
		"short hash":       strings.Replace(good, strings.Repeat("a", 64), `abc`, 1),
		"upper-case hash":  strings.Replace(good, strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
		"zero size":        strings.Replace(good, `"size":10`, `"size":0`, 1),
		"huge size":        strings.Replace(good, `"size":10`, `"size":999999999999`, 1),
	}
	for name, in := range bad {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCompareAndNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.2.0", "0.2.0", 0},
		{"0.10.0", "0.9.0", 1}, // numeric, not lexical
		{"0.2.1", "0.2.0", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.2.0", "0.3.0", -1},
		{"dev", "0.0.1", -1}, // a dev build is older than any release
		{"0.0.1", "dev", 1},
		{"dev", "dev", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if !Newer("0.3.0", "0.2.0") || !Newer("0.3.0", "dev") {
		t.Error("0.3.0 is newer than 0.2.0 and than a dev build")
	}
	if Newer("0.2.0", "0.2.0") || Newer("0.1.0", "0.2.0") || Newer("dev", "0.2.0") || Newer("garbage", "0.2.0") {
		t.Error("equal, older and invalid candidates are never newer")
	}
}

func TestNamesAreStrict(t *testing.T) {
	for _, ok := range []string{"manifest.json", "manifest.sig", "aivoicebus-darwin-arm64", "aivoicebus-linux-amd64"} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "../manifest.json", "a/b", "manifest.json/", "aivoicebus", "aivoicebus-darwin", "aivoicebus-Darwin-arm64",
		"aivoicebus-darwin-arm64.exe", "/etc/passwd", "aivoicebus-darwin-arm64\x00", "MANIFEST.JSON"} {
		if ValidName(bad) {
			t.Errorf("%q must not be a valid release file name", bad)
		}
	}
}

func TestPrivateKeyFileIsPrivateAndNeverOverwritten(t *testing.T) {
	priv, pub := newKey(t)
	path := filepath.Join(t.TempDir(), "key")
	if err := SavePrivateKey(path, priv); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %04o", fi.Mode().Perm())
	}
	if err := SavePrivateKey(path, priv); err == nil {
		t.Error("an existing key file must not be overwritten")
	}
	back, err := LoadPrivateKey(path)
	if err != nil || !back.Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatalf("round trip: %v", err)
	}
	os.Chmod(path, 0o644)
	if _, err := LoadPrivateKey(path); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Errorf("a world-readable key must be refused, got %v", err)
	}
}

func TestDirSourceRefusesOtherNamesAndOversizeFiles(t *testing.T) {
	dir, _, _ := packed(t)
	src := DirSource{Dir: dir}
	if _, err := src.Fetch("README.txt", 100); err == nil {
		t.Error("README.txt is not a release file")
	}
	if _, err := src.Fetch("../x", 100); err == nil {
		t.Error("path traversal must be refused")
	}
	if _, err := src.Fetch("aivoicebus-linux-amd64", 3); err == nil {
		t.Error("a file over the limit must be refused")
	}
	if b, err := src.Fetch("aivoicebus-linux-amd64", 100); err != nil || string(b) != "linux binary" {
		t.Errorf("fetch: %q %v", b, err)
	}
}

func TestManifestVersion(t *testing.T) {
	if v, err := ManifestVersion([]byte(`{"version":"0.4.2","files":{}}`)); err != nil || v != "0.4.2" {
		t.Errorf("%q %v", v, err)
	}
	for _, bad := range []string{`{}`, `{"version":"x"}`, `nope`} {
		if _, err := ManifestVersion([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNodeListSignVerifyAndRefuseTampering(t *testing.T) {
	priv, pub := newKey(t)
	dir := t.TempDir()
	list := []NodeEntry{{Name: "node-a", URL: "http://100.64.0.14:7777"}, {Name: "build", URL: "http://100.64.0.13:7777"}}
	n, err := PackNodes(PackNodesOptions{Dir: dir, Serial: 7, Nodes: list, Key: priv, Now: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)})
	if err != nil || n.Serial != 7 {
		t.Fatalf("%v %+v", err, n)
	}
	body, _ := os.ReadFile(filepath.Join(dir, NodesName))
	sig, _ := os.ReadFile(filepath.Join(dir, NodesSigName))
	got, err := VerifyNodes(body, sig, []ed25519.PublicKey{pub})
	if err != nil || len(got.URLs()) != 2 || got.URLs()[1] != "http://100.64.0.13:7777" {
		t.Fatalf("%v %+v", err, got)
	}

	tampered := []byte(strings.Replace(string(body), "100.64.0.13", "203.0.113.9", 1))
	if _, err := VerifyNodes(tampered, sig, []ed25519.PublicKey{pub}); !errors.Is(err, ErrBadSignature) {
		t.Errorf("a changed address must fail the signature: %v", err)
	}
	_, otherPub := newKey(t)
	if _, err := VerifyNodes(body, sig, []ed25519.PublicKey{otherPub}); !errors.Is(err, ErrBadSignature) {
		t.Errorf("a rogue key: %v", err)
	}
	if _, err := VerifyNodes(body, sig, nil); !errors.Is(err, ErrNoTrustedKeys) {
		t.Errorf("no trusted keys: %v", err)
	}
	// a release signature is never a node-list signature
	manifestSig := Sign(body, priv)
	if _, err := VerifyNodes(body, []byte(manifestSig), []ed25519.PublicKey{pub}); !errors.Is(err, ErrBadSignature) {
		t.Errorf("a release signature over the same bytes must not verify as a node list: %v", err)
	}
	// and the reverse: a node-list signature does not verify as a release
	if err := Verify(body, []byte(SignNodes(body, priv)), []ed25519.PublicKey{pub}); !errors.Is(err, ErrBadSignature) {
		t.Errorf("a node-list signature must not verify as a release: %v", err)
	}
}

func TestNodeListValidationRefusesAnythingThatCouldHarvestTheToken(t *testing.T) {
	priv, _ := newKey(t)
	for name, nodes := range map[string][]NodeEntry{
		"credentials":    {{Name: "a", URL: "http://user:pw@100.1.1.1:7777"}},
		"a path":         {{Name: "a", URL: "http://100.1.1.1:7777/steal"}},
		"a query":        {{Name: "a", URL: "http://100.1.1.1:7777?x=1"}},
		"no port":        {{Name: "a", URL: "http://100.1.1.1"}},
		"not http":       {{Name: "a", URL: "ftp://100.1.1.1:7777"}},
		"no host":        {{Name: "a", URL: "http://:7777"}},
		"duplicate":      {{Name: "a", URL: "http://100.1.1.1:7777"}, {Name: "b", URL: "http://100.1.1.1:7777"}},
		"empty":          nil,
		"a bad name":     {{Name: "a b", URL: "http://100.1.1.1:7777"}},
		"too many nodes": manyNodes(MaxNodes + 1),
	} {
		if _, err := PackNodes(PackNodesOptions{Dir: t.TempDir(), Serial: 1, Nodes: nodes, Key: priv}); err == nil {
			t.Errorf("%s: PackNodes must refuse to sign it", name)
		}
	}
	for _, bad := range []string{`{"schema":2,"serial":1,"nodes":[{"name":"a","url":"http://1.1.1.1:1"}]}`, `{"schema":1,"serial":0,"nodes":[{"name":"a","url":"http://1.1.1.1:1"}]}`, `nope`} {
		if _, err := ParseNodes([]byte(bad)); err == nil {
			t.Errorf("ParseNodes accepted %q", bad)
		}
	}
	if !ValidName(NodesName) || !ValidName(NodesSigName) || ValidName("nodes.json/../x") {
		t.Error("the nodes serve the list and its signature, and nothing else with that name")
	}
	// the serial defaults to the clock, which only increases
	n, err := PackNodes(PackNodesOptions{Dir: t.TempDir(), Nodes: []NodeEntry{{Name: "a", URL: "http://100.1.1.1:7777"}}, Key: priv, Now: time.Unix(1_800_000_000, 0)})
	if err != nil || n.Serial != 1_800_000_000 {
		t.Errorf("%v %+v", err, n)
	}
}

func manyNodes(n int) []NodeEntry {
	var out []NodeEntry
	for i := 0; i < n; i++ {
		out = append(out, NodeEntry{Name: fmt.Sprintf("n%d", i), URL: fmt.Sprintf("http://100.1.1.%d:7777", i)})
	}
	return out
}
