package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// MaxSmallFile bounds the settings files read into memory (roles, names): they are a few lines.
const MaxSmallFile = 64 << 10

// ReadSmall reads a small settings file: "" and no error when it does not exist. It refuses a
// symlink or anything but a regular file (the file is checked, opened, and checked again to be the
// same one), and one larger than MaxSmallFile, so a planted link or a huge file cannot steer or
// stall the reader.
func ReadSmall(path string) (string, error) {
	li, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", err
	case !li.Mode().IsRegular():
		return "", fmt.Errorf("%s is not a regular file (a symlink is refused)", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !os.SameFile(li, fi) {
		return "", fmt.Errorf("%s changed while it was opened", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxSmallFile+1))
	if err != nil {
		return "", err
	}
	if len(b) > MaxSmallFile {
		return "", fmt.Errorf("%s is larger than %d KiB", path, MaxSmallFile>>10)
	}
	return string(b), nil
}

// UpdateSmall changes the settings file name in the config directory: under a lock (two commands
// at once do not drop each other's edit), edit gets its text (read as ReadSmall does) and returns
// the new one, which replaces it atomically with mode 0600. Nothing is written when the text is
// unchanged.
func UpdateSmall(name string, edit func(old string) (string, error)) error {
	dir := Dir()
	if dir == "" {
		return errors.New("no home directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The wait is long: a loaded machine can hold each edit (read, fsync, rename) for a while, and
	// a settings command that gives up early is worse than one that waits.
	unlock, ok := Lock(filepath.Join(dir, "."+name+".lock"), 10*time.Second)
	if !ok {
		return fmt.Errorf("another aivoicebus command is changing %s; try again", name)
	}
	defer unlock()
	path := filepath.Join(dir, name)
	old, err := ReadSmall(path)
	if err != nil {
		return err
	}
	text, err := edit(old)
	if err != nil || text == old {
		return err
	}
	tmp, err := os.CreateTemp(dir, name+"-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadRoles is the sender's role pins (~/.config/aivoicebus/roles) as written; "" when there are none.
func ReadRoles() (string, error) {
	if d := Dir(); d != "" {
		return ReadSmall(d + "/roles")
	}
	return "", nil
}

// OwnerTrustFile is the file of the declarers whose owner records this sender trusts.
const OwnerTrustFile = "owner-trust"

// ReadOwnerTrust is the sender's trusted declarers (~/.config/aivoicebus/owner-trust) as written; ""
// when there are none. See msg.ParseOwnerPins.
func ReadOwnerTrust() (string, error) {
	if d := Dir(); d != "" {
		return ReadSmall(d + "/" + OwnerTrustFile)
	}
	return "", nil
}
