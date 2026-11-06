//go:build unix

package store

import (
	"errors"
	"os"
	"syscall"
)

func historyOwned(fi os.FileInfo) bool {
	s, ok := fi.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Getuid()) && (!fi.Mode().IsRegular() || s.Nlink == 1)
}

func lockLeaseHistory(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || !historyOwned(fi) {
		f.Close()
		return nil, errors.New("lease history: unsafe writer-lock file")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("lease history: data directory already has an indexed writer")
	}
	return f, nil
}
