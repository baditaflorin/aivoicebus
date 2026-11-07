//go:build !unix

package store

import (
	"errors"
	"os"
)

func historyOwned(fi os.FileInfo) bool { return false }

func lockLeaseHistory(path string) (*os.File, error) {
	return nil, errors.New("lease history: indexed mode is supported on Unix servers only")
}
