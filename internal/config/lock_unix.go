//go:build unix

package config

import (
	"os"
	"syscall"
	"time"
)

// Lock takes an exclusive lock on the file at path (made if need be, mode 0600) and returns how to
// let it go. It waits at most wait and then gives up (ok false). The lock goes with the file
// descriptor, so a process that dies does not leave it held.
func Lock(path string, wait time.Duration) (unlock func(), ok bool) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, true
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR || !time.Now().Before(deadline) {
			f.Close()
			return nil, false
		}
		time.Sleep(2 * time.Millisecond)
	}
}
