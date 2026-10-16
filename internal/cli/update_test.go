package cli

import (
	"crypto/ed25519"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
	"github.com/baditaflorin/aivoicebus/internal/version"
)

// fakeBinary is a tiny executable that answers like aivoicebus does to `version` and `setup`.
func fakeBinary(ver string) []byte {
	return []byte("#!/bin/sh\ncase \"$1\" in\n  version) echo \"aivoicebus " + ver + " (commit test, test/test)\" ;;\n  setup) echo \"setup-ran-" + ver + "\" ;;\nesac\n")
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Release
	version.Release = v
	t.Cleanup(func() { version.Release = old })
}

func trust(t *testing.T, keys ...ed25519.PublicKey) {
	t.Helper()
	old := trustedKeys
	trustedKeys = func() []ed25519.PublicKey { return keys }
	t.Cleanup(func() { trustedKeys = old })
}

// signedRelease makes a release directory for this platform holding a fake binary.
func signedRelease(t *testing.T, ver string) (dir string, pub ed25519.PublicKey) {
	t.Helper()
	priv, pub, err := release.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	os.WriteFile(filepath.Join(dir, release.AssetName(runtime.GOOS, runtime.GOARCH)), fakeBinary(ver), 0o755)
	if _, err := release.Pack(release.PackOptions{Dir: dir, Version: ver, Key: priv}); err != nil {
		t.Fatal(err)
	}
	return dir, pub
}

func installedFake(t *testing.T, ver string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "aivoicebus")
	os.WriteFile(bin, fakeBinary(ver), 0o755)
	return bin
}

func TestUpdateFromADirectoryInstallsRunsSetupAndRollsBack(t *testing.T) {
	withVersion(t, "0.2.0")
	dir, pub := signedRelease(t, "0.3.0")
	trust(t, pub)
	bin := installedFake(t, "0.2.0")

	code, out, e := run(t, "", "update", "--from", "dir:"+dir, "--bin", bin)
	if code != 0 || !strings.Contains(out, "installed aivoicebus 0.3.0") || !strings.Contains(out, "setup-ran-0.3.0") {
		t.Fatalf("update: %d\n%s\n%s", code, out, e)
	}
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "0.3.0") {
		t.Error("the installed binary was not replaced")
	}
	if b, _ := os.ReadFile(bin + ".prev"); !strings.Contains(string(b), "0.2.0") {
		t.Error("the previous binary was not kept")
	}

	code, out, e = run(t, "", "update", "--rollback", "--bin", bin)
	if code != 0 || !strings.Contains(out, "rolled back to aivoicebus 0.2.0") {
		t.Fatalf("rollback: %d\n%s\n%s", code, out, e)
	}
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "0.2.0") {
		t.Error("rollback did not restore the previous binary")
	}
}

func TestUpdateRefusesAReleaseSignedByAnUntrustedKey(t *testing.T) {
	withVersion(t, "0.2.0")
	dir, _ := signedRelease(t, "0.3.0")
	_, other, _ := release.GenerateKey()
	trust(t, other)
	bin := installedFake(t, "0.2.0")
	code, _, e := run(t, "", "update", "--from", "dir:"+dir, "--bin", bin)
	if code != 1 || !strings.Contains(e, "does not match any trusted key") {
		t.Fatalf("%d %q", code, e)
	}
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "0.2.0") {
		t.Error("an untrusted release changed the installed binary")
	}
	trust(t) // a build that trusts nothing refuses everything
	if code, _, e = run(t, "", "update", "--from", "dir:"+dir, "--bin", bin); code != 1 || !strings.Contains(e, "trusts no release signing key") {
		t.Fatalf("no keys: %d %q", code, e)
	}
}

func TestUpdateCheckAndUpToDateChangeNothing(t *testing.T) {
	dir, pub := signedRelease(t, "0.3.0")
	trust(t, pub)
	bin := installedFake(t, "0.2.0")

	withVersion(t, "0.2.0")
	code, out, _ := run(t, "", "update", "--check", "--from", "dir:"+dir, "--bin", bin)
	if code != 0 || !strings.Contains(out, "release 0.3.0 is available") {
		t.Fatalf("check: %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "0.2.0") {
		t.Error("--check changed the binary")
	}
	withVersion(t, "0.3.0")
	if code, out, _ = run(t, "", "update", "--from", "dir:"+dir, "--bin", bin); code != 0 || !strings.Contains(out, "up to date") {
		t.Fatalf("up to date: %d\n%s", code, out)
	}
}

func TestUpdateFromTheMesh(t *testing.T) {
	withVersion(t, "0.2.0")
	dir, pub := signedRelease(t, "0.3.0")
	trust(t, pub)
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, "tok", server.WithReleaseDir(dir)))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	bin := installedFake(t, "0.2.0")

	code, out, e := run(t, "", "update", "--bin", bin)
	if code != 0 || !strings.Contains(out, "installed aivoicebus 0.3.0") {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	t.Setenv("AIVOICEBUS_TOKEN", "wrong")
	if code, _, e = run(t, "", "update", "--check", "--bin", bin); code != exitRejected || !strings.Contains(e, "401") || !strings.Contains(e, "--from github") {
		t.Fatalf("a refused token should fail and point at the other source: %d %q", code, e)
	}
}

func TestUpdateArgumentErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", "")
	if code, _, e := run(t, "", "update", "--from", "carrier-pigeon"); code != 1 || !strings.Contains(e, `unknown --from "carrier-pigeon"`) {
		t.Errorf("%d %q", code, e)
	}
	if code, _, e := run(t, "", "update", "--check", "--from", "mesh"); code != 2 || !strings.Contains(e, "AIVOICEBUS_URL not set") {
		t.Errorf("mesh source without a URL: %d %q", code, e)
	}
	if code, _, e := run(t, "", "update", "--rollback", "--bin", filepath.Join(t.TempDir(), "none")); code != 1 || !strings.Contains(e, "no previous binary") {
		t.Errorf("rollback without .prev: %d %q", code, e)
	}
}

func TestReleaseKeygenPackVerify(t *testing.T) {
	trust(t) // the build trusts nothing: verification needs an explicit key
	dir := t.TempDir()
	keyPath := filepath.Join(t.TempDir(), "sub", "key")

	code, out, e := run(t, "", "release", "keygen", "--out", keyPath)
	if code != 0 || !strings.Contains(out, "public key") || strings.Contains(out, "private key: ") {
		t.Fatalf("keygen: %d\n%s\n%s", code, out, e)
	}
	if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v", err)
	}
	pub := strings.TrimSpace(out[strings.LastIndex(out, ": ")+2:])
	if code, _, e = run(t, "", "release", "keygen", "--out", keyPath); code != 1 || !strings.Contains(e, "exists") {
		t.Fatalf("keygen must never overwrite a key: %d %q", code, e)
	}

	os.WriteFile(filepath.Join(dir, "aivoicebus-linux-amd64"), fakeBinary("1.0.0"), 0o755)
	if code, out, e = run(t, "", "release", "pack", "--dir", dir, "--key", keyPath, "--version", "1.0.0", "--commit", "abc"); code != 0 || !strings.Contains(out, "packed and signed release 1.0.0 (1 files)") {
		t.Fatalf("pack: %d\n%s\n%s", code, out, e)
	}
	if code, _, e = run(t, "", "release", "verify", "--dir", dir); code != 1 || !strings.Contains(e, "trusts no release signing key") {
		t.Errorf("verify without a key: %d %q", code, e)
	}
	if code, out, e = run(t, "", "release", "verify", "--dir", dir, "--pubkey", pub); code != 0 || !strings.Contains(out, "release 1.0.0 verified") {
		t.Fatalf("verify: %d\n%s\n%s", code, out, e)
	}
	os.WriteFile(filepath.Join(dir, "aivoicebus-linux-amd64"), []byte("tampered"), 0o755)
	if code, _, e = run(t, "", "release", "verify", "--dir", dir, "--pubkey", pub); code != 1 || !strings.Contains(e, "does not match the signed manifest") {
		t.Errorf("tampered binary: %d %q", code, e)
	}
	// a world-readable key is refused for signing
	os.Chmod(keyPath, 0o644)
	if code, _, e = run(t, "", "release", "pack", "--dir", dir, "--key", keyPath, "--version", "1.0.1"); code != 1 || !strings.Contains(e, "readable by other users") {
		t.Errorf("open key file: %d %q", code, e)
	}
}

func TestReleaseCommandErrors(t *testing.T) {
	for args, want := range map[string]string{
		"release":                  "usage: release keygen|pack|verify",
		"release frob":             `unknown release command "frob"`,
		"release pack":             "needs --dir",
		"release verify":           "needs --dir",
		"release pack --dir /nope": "no such file",
	} {
		a := strings.Fields(args)
		if a[len(a)-1] == "/nope" {
			t.Setenv("HOME", t.TempDir()) // no default key either
		}
		if code, _, e := run(t, "", a...); code != 1 || !strings.Contains(e, want) {
			t.Errorf("%q: %d %q, want %q", args, code, e, want)
		}
	}
}

func TestDoctorReportsANewerRelease(t *testing.T) {
	dir, _ := signedRelease(t, "0.9.0")
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, "tok", server.WithReleaseDir(dir)))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_ME", "imac/claude")

	withVersion(t, "0.2.0")
	_, out, _ := run(t, "", "doctor")
	if !strings.Contains(out, "WARN  release") || !strings.Contains(out, "release 0.9.0 is available") {
		t.Errorf("behind:\n%s", out)
	}
	withVersion(t, "0.9.0")
	if _, out, _ = run(t, "", "doctor"); !strings.Contains(out, "ok    release") {
		t.Errorf("current:\n%s", out)
	}
}
