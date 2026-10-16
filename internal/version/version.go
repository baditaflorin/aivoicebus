// Package version says which build this is. Release and Commit are stamped at
// build time by scripts/build.sh (see the VERSION file). A plain `go build` of a checkout
// reports "dev"; a `go install github.com/.../aivoicebus@v0.1.0` build reports "0.1.0", read from
// the module version Go records in the binary.
package version

import (
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
)

var (
	// Release is the release number, from the VERSION file.
	Release = "dev"
	// Commit is the short git revision the binary was built from.
	Commit = ""
)

func init() {
	if Release != "dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := ModuleRelease(bi.Main.Version); v != "" {
			Release = v
		}
	}
}

var moduleTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// ModuleRelease turns the module version Go records in a binary into a release number: "v0.1.0"
// gives "0.1.0". Anything that is not a plain tagged release gives "": "(devel)" (built from a
// checkout), a pseudo-version (an untagged commit), a pre-release or build suffix. Those stay "dev"
// so nothing mistakes an unreleased build for a release.
func ModuleRelease(v string) string {
	if !moduleTag.MatchString(v) {
		return ""
	}
	return v[1:]
}

// CommitOrBuildInfo is Commit, or the revision Go stamped into the binary.
func CommitOrBuildInfo() string {
	if Commit != "" {
		return Commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 9 {
				return s.Value[:9]
			}
		}
	}
	return "unknown"
}

// String is the one-line description printed by `aivoicebus version`.
func String() string {
	return fmt.Sprintf("aivoicebus %s (commit %s, %s/%s)", Release, CommitOrBuildInfo(), runtime.GOOS, runtime.GOARCH)
}
