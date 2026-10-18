package config

import (
	"io"
	"os"
	"strings"
	"syscall"
)

// secretFiles are the files in Dir() that hold a credential: the shared token and the one a
// rotation is moving to, the per-node token list, the dashboard's observer token, the key that
// guards the bootstrap link and the release signing key (a base64 seed). The tokens of single
// machines are the files in the machine-tokens directory.
var secretFiles = []string{"token", "token.next", "token.nodes", "observer-token", "bootstrap-key", "release-signing.key"}

// maxSecretFile bounds what is read from one file; none of them is larger than a few lines.
const maxSecretFile = 64 << 10

// SecretValues lists the credentials this machine holds, so a message that contains one can
// be refused before it leaves: the token in the environment and every line of the token file in
// use, of the files in secretFiles, of machine-tokens/ (and AIVOICEBUS_MACHINE_TOKENS_DIR) and of the credentials/ directory. A `name:token` line (token.nodes)
// gives both the whole line and the token. Whatever is missing or unreadable is skipped, no
// error is reported and nothing is logged: the values are exactly what must not leak.
//
// A path counts when it is a regular file after following symlinks (a symlinked token is the
// usual way to keep one in a password store's directory); anything else, such as a FIFO that
// would make a read block for ever, is skipped, and every send reads these files.
func SecretValues() []string {
	var out []string
	if t := strings.TrimSpace(os.Getenv("AIVOICEBUS_TOKEN")); t != "" {
		out = append(out, t)
	}
	add := func(path string) {
		for _, line := range splitLines(readSecretFile(path)) {
			out = append(out, line)
			if _, tok, ok := strings.Cut(line, ":"); ok {
				out = append(out, strings.TrimSpace(tok))
			}
		}
	}
	if f := os.Getenv("AIVOICEBUS_TOKEN_FILE"); f != "" {
		add(f)
	}
	dir := Dir()
	if dir == "" {
		return out
	}
	for _, name := range secretFiles {
		add(dir + "/" + name)
	}
	mt := []string{dir + "/machine-tokens"}
	if d := os.Getenv("AIVOICEBUS_MACHINE_TOKENS_DIR"); d != "" && d != mt[0] { // where deploy/machine-token.sh was told to keep them
		mt = append(mt, d)
	}
	for _, d := range mt {
		if entries, err := os.ReadDir(d); err == nil {
			for _, e := range entries {
				add(d + "/" + e.Name())
			}
		}
	}
	// credentials a person handed over with `aivoicebus secret save`: one token file per name
	if entries, err := os.ReadDir(dir + "/credentials"); err == nil {
		for _, e := range entries {
			add(dir + "/credentials/" + e.Name() + "/token")
		}
	}
	return out
}

// readSecretFile returns the start of the file at path, or "" if it is not a regular file or
// cannot be read. It decides from Stat (which follows symlinks) before opening, and opens
// without blocking, so a FIFO swapped in between cannot stall the caller either.
func readSecretFile(path string) string {
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(f, maxSecretFile))
	return strings.TrimPrefix(string(b), "\xef\xbb\xbf") // a file saved by a Windows editor
}

// splitLines splits text into its non-blank lines without comments, accepting \n, \r\n and a
// lone \r as line ends.
func splitLines(text string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// SecretFileLines is the credential(s) in one token file: its non-blank lines, without comments
// (none if it is missing or not a regular file). For matching and masking only, never printing.
func SecretFileLines(path string) []string { return splitLines(readSecretFile(path)) }
