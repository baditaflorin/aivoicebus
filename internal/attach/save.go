package attach

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Why a fetch did not happen, for the CLI and the MCP tool to say the same thing.
var (
	ErrNoAttachment = errors.New("the message has no attachment")
	ErrNotSetUp     = errors.New("the attachment is a link to a service this machine is not set up to use, so it was not fetched")
	ErrNotAFile     = errors.New("the attachment is a live log, not a file: `aivoicebus get ID --follow` reads it")
	ErrChecksum     = errors.New("the decrypted file does not match the checksum the sender gave; not saved")
)

// FetchFile fetches, decrypts and checks the file a message body points at, from a service this
// machine is configured with (never a host named in the message). token is the mesh token, which a
// file held by a node needs. The returned sha256 is of what was fetched.
func FetchFile(ctx context.Context, body, token string) ([]byte, Link, string, error) {
	l, found, err := Find(body)
	switch {
	case !found:
		return nil, l, "", ErrNoAttachment
	case err != nil:
		return nil, l, "", err
	}
	p := ProviderFor(l)
	if n, ok := p.(*Node); ok {
		p = &Node{URLs: n.URLs, Token: token}
	}
	if p == nil {
		return nil, l, "", ErrNotSetUp
	}
	fs, ok := p.(FileStore)
	if !ok {
		return nil, l, "", ErrNotAFile
	}
	open, err := Unseal(l)
	if err != nil {
		return nil, l, "", err
	}
	data, err := fs.Get(ctx, open)
	if err != nil {
		return nil, l, "", err
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if l.SHA256 != "" && got != l.SHA256 {
		return nil, l, "", ErrChecksum
	}
	return data, l, got, nil
}

// SavePath picks where a received file goes. A sender-chosen name is only ever a file name.
func SavePath(msgID, name, out string) (string, error) {
	safe := SafeName(name)
	if out != "" {
		if fi, err := os.Stat(out); err == nil && fi.IsDir() {
			return filepath.Join(out, safe), nil
		}
		return out, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".local", "share", "aivoicebus", "received", msgID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, safe), nil
}

// WriteNew writes data to path with mode 0600. It refuses to replace an existing file unless
// force, and never writes through a symbolic link.
func WriteNew(path string, data []byte, force bool) error {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link; not writing through it", path)
		}
		if !force {
			return fmt.Errorf("%s exists; use --out to pick another path or --force to replace it", path)
		}
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
