package release

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Source supplies the files of one release. It only fetches bytes; nothing it
// returns is trusted until the signature and hashes have been checked.
type Source interface {
	// Fetch returns the named release file, failing if it is longer than limit.
	Fetch(name string, limit int64) ([]byte, error)
	// Describe says where the files come from, for messages.
	Describe() string
}

// DirSource reads a release from a local directory.
type DirSource struct{ Dir string }

func (d DirSource) Describe() string { return "directory " + d.Dir }

func (d DirSource) Fetch(name string, limit int64) ([]byte, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("%q is not a release file name", name)
	}
	f, err := os.Open(filepath.Join(d.Dir, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return b, nil
}
