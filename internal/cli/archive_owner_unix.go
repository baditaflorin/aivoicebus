//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"syscall"
)

// matchOwner gives the files an import wrote the owner of the data directory. A node's
// service runs as a dynamic user that owns /var/lib/aivoicebus, so files created by root
// (`aivoicebus import` run with sudo) would be unreadable to it, and the node would start empty
// as if the import had never happened. It does nothing unless running as root.
func matchOwner(dir string) {
	if os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(dir) // follows the /var/lib/aivoicebus symlink to the real directory
	if err != nil {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	for _, name := range []string{"log.jsonl", "log.jsonl.torn", "presence.json"} {
		_ = os.Chown(filepath.Join(dir, name), int(st.Uid), int(st.Gid))
	}
}
