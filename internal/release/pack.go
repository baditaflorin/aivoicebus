package release

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// PackOptions describe a release to assemble in a directory that already holds
// the platform binaries.
type PackOptions struct {
	Dir     string
	Version string
	Commit  string
	Notes   string
	Parts   map[string]string
	Key     ed25519.PrivateKey
	Now     time.Time // zero means time.Now()
}

// Pack hashes the binaries in o.Dir (files named aivoicebus-<os>-<arch>), writes
// manifest.json, signs it, and writes manifest.sig. It returns the manifest.
func Pack(o PackOptions) (*Manifest, error) {
	if !ValidVersion(o.Version) {
		return nil, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", o.Version)
	}
	if len(o.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("no signing key")
	}
	entries, err := os.ReadDir(o.Dir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Schema: Schema, Version: o.Version, Commit: o.Commit, Notes: o.Notes, Parts: o.Parts, Files: map[string]File{}}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	m.Date = o.Now.UTC().Format(time.RFC3339)
	for _, e := range entries {
		if e.IsDir() || !assetRe.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(o.Dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if len(b) == 0 || len(b) > MaxBinary {
			return nil, fmt.Errorf("%s has an unusable size (%d bytes)", e.Name(), len(b))
		}
		m.Files[e.Name()] = File{SHA256: Hash(b), Size: int64(len(b))}
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("no aivoicebus-<os>-<arch> binaries in %s", o.Dir)
	}
	body, err := json.MarshalIndent(m, "", "  ") // map keys are sorted: stable output
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	if err := os.WriteFile(filepath.Join(o.Dir, ManifestName), body, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(o.Dir, SignatureName), []byte(Sign(body, o.Key)+"\n"), 0o644); err != nil {
		return nil, err
	}
	return m, nil
}

// VerifyDir checks a release directory end to end: the signature on its
// manifest, and every listed file against its recorded hash and size.
func VerifyDir(dir string, keys []ed25519.PublicKey) (*Manifest, error) {
	src := DirSource{Dir: dir}
	mb, err := src.Fetch(ManifestName, MaxManifest)
	if err != nil {
		return nil, err
	}
	sb, err := src.Fetch(SignatureName, maxSignature)
	if err != nil {
		return nil, err
	}
	if err := Verify(mb, sb, keys); err != nil {
		return nil, err
	}
	m, err := Parse(mb)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m.Files))
	for n := range m.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b, err := src.Fetch(n, MaxBinary)
		if err != nil {
			return nil, err
		}
		if err := CheckFile(m, n, b); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// CheckFile verifies fetched bytes against the manifest's record of name.
func CheckFile(m *Manifest, name string, b []byte) error {
	f, ok := m.Files[name]
	if !ok {
		return fmt.Errorf("%s is not part of release %s", name, m.Version)
	}
	if int64(len(b)) != f.Size || Hash(b) != f.SHA256 {
		return fmt.Errorf("%s does not match the signed manifest (size or sha256 differs)", name)
	}
	return nil
}
