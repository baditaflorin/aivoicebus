// Package release defines what a aivoicebus release is and how to trust one: a
// manifest (version, file hashes, part ids) signed with ed25519, and the
// version rules for deciding whether it is newer. It does no networking and
// imports nothing from this module.
//
// A release is a directory (or a GitHub release, or the files a node serves):
//
//	manifest.json            the manifest
//	manifest.sig             base64 ed25519 signature over manifest.json's exact bytes
//	aivoicebus-<os>-<arch>    one binary per platform, named in the manifest
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

const (
	// ManifestName and SignatureName are the two fixed file names of a release.
	ManifestName  = "manifest.json"
	SignatureName = "manifest.sig"

	// Schema is the manifest format this code understands.
	Schema = 1

	// MaxManifest and MaxBinary bound what a client will read.
	MaxManifest  = 1 << 20
	MaxBinary    = 128 << 20
	maxSignature = 1 << 10
)

var (
	semverRe = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	assetRe  = regexp.MustCompile(`^aivoicebus-[a-z0-9]+-[a-z0-9]+$`)
	hexRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// File describes one binary in a release.
type File struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is the signed description of a release.
type Manifest struct {
	Schema  int               `json:"schema"`
	Version string            `json:"version"`
	Commit  string            `json:"commit,omitempty"`
	Date    string            `json:"date,omitempty"`
	Notes   string            `json:"notes,omitempty"`
	Files   map[string]File   `json:"files"`
	Parts   map[string]string `json:"parts,omitempty"` // content ids of the managed parts (hook, snippet, usage)
}

// AssetName is the file name of the binary for a platform.
func AssetName(goos, goarch string) string { return "aivoicebus-" + goos + "-" + goarch }

// ValidName reports whether name may be fetched from, or served as part of, a
// release. Anything else (paths, dots, other names) is refused everywhere.
func ValidName(name string) bool {
	return name == ManifestName || name == SignatureName || name == NodesName || name == NodesSigName || assetRe.MatchString(name)
}

// Hash is the lowercase hex sha256 of b.
func Hash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Parse decodes and validates manifest bytes. Verify the signature first: this
// only checks the manifest is well formed.
func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest is not valid JSON: %w", err)
	}
	if m.Schema != Schema {
		return nil, fmt.Errorf("manifest schema %d is not supported by this aivoicebus (want %d); update it by hand first", m.Schema, Schema)
	}
	if !ValidVersion(m.Version) {
		return nil, fmt.Errorf("manifest version %q is not MAJOR.MINOR.PATCH", m.Version)
	}
	if len(m.Files) == 0 {
		return nil, errors.New("manifest lists no files")
	}
	for name, f := range m.Files {
		switch {
		case !assetRe.MatchString(name):
			return nil, fmt.Errorf("manifest file name %q is not allowed", name)
		case !hexRe.MatchString(f.SHA256):
			return nil, fmt.Errorf("manifest hash for %s is not a sha256", name)
		case f.Size <= 0 || f.Size > MaxBinary:
			return nil, fmt.Errorf("manifest size for %s is out of range", name)
		}
	}
	return &m, nil
}

// ManifestVersion reads just the version out of manifest bytes. Nodes use it
// to announce the newest release; it checks nothing else, because clients
// verify the signature before acting.
func ManifestVersion(b []byte) (string, error) {
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &m); err != nil || !ValidVersion(m.Version) {
		return "", errors.New("no valid version in manifest")
	}
	return m.Version, nil
}

// ValidVersion reports whether v is MAJOR.MINOR.PATCH.
func ValidVersion(v string) bool { return semverRe.MatchString(v) }

func parseVersion(v string) (n [3]int, ok bool) {
	m := semverRe.FindStringSubmatch(v)
	if m == nil {
		return n, false
	}
	for i := 0; i < 3; i++ {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	return n, true
}

// Compare orders two release numbers. A number that is not MAJOR.MINOR.PATCH
// (a "dev" build) sorts below every release.
func Compare(a, b string) int {
	na, oka := parseVersion(a)
	nb, okb := parseVersion(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := 0; i < 3; i++ {
		if na[i] != nb[i] {
			if na[i] < nb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Newer reports whether candidate is a strictly newer release than current.
func Newer(candidate, current string) bool {
	return ValidVersion(candidate) && Compare(candidate, current) > 0
}
