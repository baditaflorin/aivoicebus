package server

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/release"
)

// latestRelease reports the version in the release directory's manifest,
// re-reading it only when the file changes.
type latestRelease struct {
	dir string

	mu    sync.Mutex
	mtime time.Time
	size  int64
	ver   string
}

func (l *latestRelease) version() string {
	if l.dir == "" {
		return ""
	}
	path := filepath.Join(l.dir, release.ManifestName)
	fi, err := os.Stat(path)
	if err != nil || fi.Size() > release.MaxManifest {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if fi.ModTime().Equal(l.mtime) && fi.Size() == l.size {
		return l.ver
	}
	l.mtime, l.size, l.ver = fi.ModTime(), fi.Size(), ""
	if b, err := os.ReadFile(path); err == nil {
		if v, err := release.ManifestVersion(b); err == nil {
			l.ver = v
		}
	}
	return l.ver
}
