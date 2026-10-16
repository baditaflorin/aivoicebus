package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func home(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	return h
}

func freshKey(t *testing.T) string {
	t.Helper()
	_, pub, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return EncodeKey(pub)
}

func TestTheBuiltInKeyIsValidAndTheOnlyOneByDefault(t *testing.T) {
	home(t)
	ks, notes := Keys()
	if len(ks) != len(builtinKeys) || len(notes) != 0 || len(ks) == 0 {
		t.Fatalf("%d keys, notes %v", len(ks), notes)
	}
	for _, k := range ks {
		if k.Source != "built in" || !strings.HasPrefix(Fingerprint(k.Public), "SHA256:") {
			t.Errorf("%+v", k)
		}
	}
}

func TestAKeyTheUserAddsIsTrustedAndCanBeRemoved(t *testing.T) {
	h := home(t)
	k, _ := ParseKey(freshKey(t))
	added, err := AddUserKey(k, "  my   mesh \n")
	if err != nil || !added {
		t.Fatalf("add: %v %v", added, err)
	}
	if again, err := AddUserKey(k, "dup"); err != nil || again {
		t.Errorf("a key already trusted must not be added twice: %v %v", again, err)
	}
	fi, err := os.Stat(filepath.Join(h, ".config", "aivoicebus", UserKeyFileName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the key file must be 0600: %v %v", fi, err)
	}
	ks, _ := Keys()
	if len(ks) != len(builtinKeys)+1 || ks[len(ks)-1].Label != "my mesh" || ks[len(ks)-1].Source != "trust-key" {
		t.Fatalf("%+v", ks)
	}
	if n := len(TrustedKeys()); n != len(ks) {
		t.Errorf("TrustedKeys %d, Keys %d", n, len(ks))
	}
	if ok, err := RemoveUserKey("SHA256:nope"); ok || err != nil {
		t.Errorf("removing an unknown key: %v %v", ok, err)
	}
	if ok, err := RemoveUserKey(Fingerprint(k)); !ok || err != nil {
		t.Errorf("remove: %v %v", ok, err)
	}
	if ks, _ := Keys(); len(ks) != len(builtinKeys) {
		t.Errorf("the key stayed: %+v", ks)
	}
}

// A key file anyone else can write would let them redirect updates: it is not read.
func TestAKeyFileOthersCanReadOrWriteIsIgnored(t *testing.T) {
	home(t)
	k := freshKey(t)
	path := UserKeyFile()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(k+" x\n"), 0o644)
	ks, notes := Keys()
	if len(ks) != len(builtinKeys) || len(notes) != 1 || !strings.Contains(notes[0], "chmod 600") {
		t.Errorf("keys %d notes %v", len(ks), notes)
	}
	if _, err := AddUserKey(mustKey(t), ""); err == nil {
		t.Error("a key was added to a file others can read")
	}
	os.Chmod(path, 0o600)
	if ks, _ := Keys(); len(ks) != len(builtinKeys)+1 {
		t.Errorf("a 0600 file is read: %d", len(ks))
	}
}

func TestAKeyFileThatIsASymlinkIsIgnored(t *testing.T) {
	h := home(t)
	path := UserKeyFile()
	os.MkdirAll(filepath.Dir(path), 0o700)
	target := filepath.Join(h, "elsewhere")
	os.WriteFile(target, []byte(freshKey(t)+"\n"), 0o600)
	os.Symlink(target, path)
	if ks, notes := Keys(); len(ks) != len(builtinKeys) || len(notes) != 1 {
		t.Errorf("keys %d notes %v", len(ks), notes)
	}
}

func TestGarbageLinesInTheKeyFileAreReportedAndSkipped(t *testing.T) {
	home(t)
	path := UserKeyFile()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("# comment\n\nnot-a-key\n"+freshKey(t)+" ok\n"), 0o600)
	ks, notes := Keys()
	if len(ks) != len(builtinKeys)+1 || len(notes) != 1 || !strings.Contains(notes[0], "line 3") {
		t.Errorf("keys %d notes %v", len(ks), notes)
	}
}

func TestAForksBuildTimeKeyIsTrustedAlongsideTheBuiltInOne(t *testing.T) {
	home(t)
	old := extraKey
	t.Cleanup(func() { extraKey = old })
	extraKey = freshKey(t)
	ks, _ := Keys()
	if len(ks) != len(builtinKeys)+1 || ks[len(ks)-1].Source != "built in for this fork" {
		t.Errorf("%+v", ks)
	}
	extraKey = "garbage"
	if ks, notes := Keys(); len(ks) != len(builtinKeys) || len(notes) != 1 {
		t.Errorf("a bad fork key must be reported, not trusted: %d %v", len(ks), notes)
	}
}

func TestFingerprintIsStableAndDistinct(t *testing.T) {
	a, b := mustKey(t), mustKey(t)
	if Fingerprint(a) != Fingerprint(a) || Fingerprint(a) == Fingerprint(b) || strings.ContainsAny(Fingerprint(a), "=") {
		t.Errorf("%s %s", Fingerprint(a), Fingerprint(b))
	}
}

func mustKey(t *testing.T) []byte {
	t.Helper()
	k, err := ParseKey(freshKey(t))
	if err != nil {
		t.Fatal(err)
	}
	return k
}
