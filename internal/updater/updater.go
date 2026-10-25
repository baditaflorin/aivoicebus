// Package updater installs a newer aivoicebus release on this machine.
//
// The order is the safety argument. Nothing is trusted until the manifest's
// ed25519 signature checks out against a compiled-in key; the binary is then
// checked against the hash the signed manifest records; the verified bytes are
// staged next to the installed binary and run once (`version`) to prove they
// work on this machine; only then is the installed binary replaced, by an
// atomic rename that keeps the old one as <bin>.prev. The new binary then runs
// `setup`, so the hook, snippet and config come from the release, not from
// anything fetched separately.
package updater

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/release"
)

// Runner runs a binary with arguments and returns its combined output.
type Runner func(bin string, args ...string) (string, error)

// ExecRunner is the Runner that really runs the binary (30s limit).
func ExecRunner(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return string(out), err
}

// Options configure Update.
type Options struct {
	Source       release.Source
	Trusted      []ed25519.PublicKey
	Current      string // the release number of the running binary ("dev" is fine)
	Bin          string // the installed binary to replace
	GOOS, GOARCH string // which build to install
	Check        bool   // report only; change nothing
	Force        bool   // also reinstall the same release, or an older signed one
	Skip         string // install nothing at or below this release (see Auto), unless Force
	Run          Runner // nil means ExecRunner
	Out          io.Writer
}

// Result says what Update found and did.
type Result struct {
	Current, Latest string
	Available       bool // a strictly newer release exists
	Updated         bool
	Skipped         bool  // the newest release is at or below Options.Skip: nothing was installed
	SetupErr        error // the new binary is installed but its setup failed
	RolledBack      bool  // Auto put the previous binary back after SetupErr
}

// Update fetches, verifies and installs the newest release from o.Source.
func Update(o Options) (Result, error) {
	run := o.Run
	if run == nil {
		run = ExecRunner
	}
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	res := Result{Current: o.Current}
	where := o.Source.Describe()

	mb, err := o.Source.Fetch(release.ManifestName, release.MaxManifest)
	if err != nil {
		return res, fmt.Errorf("fetching the manifest from %s: %w", where, err)
	}
	sb, err := o.Source.Fetch(release.SignatureName, 1<<10)
	if err != nil {
		return res, fmt.Errorf("fetching the signature from %s: %w", where, err)
	}
	if err := release.Verify(mb, sb, o.Trusted); err != nil {
		return res, fmt.Errorf("refusing the release from %s: %w", where, err)
	}
	m, err := release.Parse(mb)
	if err != nil {
		return res, fmt.Errorf("refusing the release from %s: %w", where, err)
	}
	res.Latest = m.Version
	res.Available = release.Newer(m.Version, o.Current)

	if !res.Available && !o.Force {
		fmt.Fprintf(out, "aivoicebus %s is up to date (the latest signed release is %s)\n", o.Current, m.Version)
		return res, nil
	}
	if o.Check {
		fmt.Fprintf(out, "release %s is available (you run %s; signature verified, from %s). Run `aivoicebus update` to install it.\n", m.Version, o.Current, where)
		return res, nil
	}
	if o.Skip != "" && !o.Force && !release.Newer(m.Version, o.Skip) {
		res.Skipped = true
		fmt.Fprintf(out, "release %s was rolled back on this machine, so it is not installed by itself; `aivoicebus update` installs it\n", m.Version)
		return res, nil
	}
	if !res.Available {
		fmt.Fprintf(out, "--force: installing signed release %s over %s\n", m.Version, o.Current)
	}

	asset := release.AssetName(o.GOOS, o.GOARCH)
	if _, ok := m.Files[asset]; !ok {
		return res, fmt.Errorf("release %s has no build for %s/%s", m.Version, o.GOOS, o.GOARCH)
	}
	bin, err := o.Source.Fetch(asset, release.MaxBinary)
	if err != nil {
		return res, fmt.Errorf("fetching %s from %s: %w", asset, where, err)
	}
	if err := release.CheckFile(m, asset, bin); err != nil {
		return res, err
	}

	// Stage the verified bytes next to the installed binary and prove they run.
	tmpName, err := stage(filepath.Dir(o.Bin), bin)
	if err != nil {
		return res, err
	}
	defer os.Remove(tmpName) // gone already after a successful rename
	vout, err := run(tmpName, "version")
	if err != nil || !strings.HasPrefix(strings.TrimSpace(vout), "aivoicebus "+m.Version+" ") {
		return res, fmt.Errorf("the downloaded binary does not run here or reports a different version (%q); nothing was changed", firstLine(vout, err))
	}

	if err := keepPrevious(o.Bin); err != nil {
		return res, err
	}
	if err := os.Rename(tmpName, o.Bin); err != nil {
		return res, fmt.Errorf("replacing %s: %w", o.Bin, err)
	}
	res.Updated = true
	fmt.Fprintf(out, "installed aivoicebus %s at %s (the previous binary is kept as %s.prev)\n", m.Version, o.Bin, filepath.Base(o.Bin))

	// Let the new binary bring the hook, snippet and config in line with itself.
	if sout, err := run(o.Bin, "setup"); err != nil {
		res.SetupErr = fmt.Errorf("`aivoicebus setup` of release %s failed: %s", m.Version, firstLine(sout, err))
		fmt.Fprintf(out, "warning: the binary is updated but `aivoicebus setup` failed (%v); run `aivoicebus doctor --fix`\n%s", err, sout)
	} else {
		fmt.Fprint(out, sout)
	}
	return res, nil
}

// Auto is the update a machine whose person turned on `aivoicebus update --auto` runs by itself.
// It is Update with nothing forced and nothing only checked, from the release the installed binary
// reports (a run started before another one's install may still be the older binary), leaving out
// o.Skip and anything older; and when the new binary's setup fails it puts the previous binary back,
// since nobody is there to repair it. A dev build is left alone.
func Auto(o Options) (Result, error) {
	run := o.Run
	if run == nil {
		run = ExecRunner
	}
	cur, err := InstalledVersion(o.Bin, run)
	if err != nil {
		return Result{}, err
	}
	if !release.ValidVersion(cur) {
		return Result{Current: cur}, fmt.Errorf("%s is a build of its own (%s), not a release: auto-update leaves it alone", o.Bin, cur)
	}
	o.Current, o.Check, o.Force, o.Run = cur, false, false, run
	res, err := Update(o)
	if err != nil || !res.Updated || res.SetupErr == nil {
		return res, err
	}
	if rerr := Rollback(o.Bin, run, o.Out); rerr != nil {
		return res, fmt.Errorf("%v, and putting %s back failed: %v", res.SetupErr, cur, rerr)
	}
	res.RolledBack = true
	return res, res.SetupErr
}

// InstalledVersion is the release the binary at bin says it is (`aivoicebus version`).
func InstalledVersion(bin string, run Runner) (string, error) {
	if run == nil {
		run = ExecRunner
	}
	out, err := run(bin, "version")
	if f := strings.Fields(out); err == nil && len(f) >= 2 && f[0] == "aivoicebus" {
		return f[1], nil
	}
	return "", fmt.Errorf("%s does not run or does not say its release (%s)", bin, firstLine(out, err))
}

// Rollback puts back the binary Update kept as <bin>.prev.
func Rollback(bin string, run Runner, out io.Writer) error {
	if run == nil {
		run = ExecRunner
	}
	if out == nil {
		out = io.Discard
	}
	prev := bin + ".prev"
	b, err := os.ReadFile(prev)
	if err != nil {
		return fmt.Errorf("no previous binary to roll back to (%s): %w", prev, err)
	}
	tmpName, err := stage(filepath.Dir(bin), b)
	if err != nil {
		return err
	}
	defer os.Remove(tmpName)
	vout, err := run(tmpName, "version")
	if err != nil {
		return fmt.Errorf("the previous binary does not run (%v); nothing was changed", err)
	}
	if err := os.Rename(tmpName, bin); err != nil {
		return fmt.Errorf("replacing %s: %w", bin, err)
	}
	fmt.Fprintf(out, "rolled back to %s", vout)
	if sout, err := run(bin, "setup"); err != nil {
		fmt.Fprintf(out, "note: `setup` of the restored binary failed (%v); it may predate that command\n", err)
	} else {
		fmt.Fprint(out, sout)
	}
	return nil
}

// stage writes b to a fresh executable file in dir and returns its path.
func stage(dir string, b []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, ".aivoicebus-update-*")
	if err != nil {
		return "", fmt.Errorf("cannot stage the update in %s: %w", dir, err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, 0o755)
	}
	if werr != nil {
		os.Remove(name)
		return "", werr
	}
	return name, nil
}

// keepPrevious copies the installed binary to <bin>.prev, if there is one.
func keepPrevious(bin string) error {
	b, err := os.ReadFile(bin)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(bin+".prev", b, 0o755)
}

func firstLine(out string, err error) string {
	if s := strings.TrimSpace(out); s != "" {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[:i]
		}
		return s
	}
	if err != nil {
		return err.Error()
	}
	return ""
}
