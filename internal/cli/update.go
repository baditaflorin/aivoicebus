package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/agentcfg"
	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/updater"
	"github.com/baditaflorin/aivoicebus/internal/version"
)

// githubRepo is where official releases are published. A placeholder until the public repository exists.
const githubRepo = "baditaflorin/aivoicebus"

// trustedKeys supplies the release signing keys the update, verify and node-list commands trust:
// the project's built-in key, a key a fork built in, and the keys the user added with
// `aivoicebus trust-key add` (see release.Keys). There is no flag or environment variable that
// adds one. It is a variable so tests can supply their own.
var trustedKeys = release.TrustedKeys

func installedBinary() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return agentcfg.Paths{Home: h}.Binary(), nil
}

// updateCmd installs a newer signed release.
func updateCmd(args []string, s Streams) error {
	fs := newFlagSet("update")
	check := fs.Bool("check", false, "report whether a newer signed release exists; change nothing")
	force := fs.Bool("force", false, "also reinstall the current release, or install an older signed one")
	from := fs.String("from", "auto", "where to read the release: auto (the mesh when a node URL is configured, else GitHub) | mesh | github | dir:<path>")
	bin := fs.String("bin", "", "the binary to replace (default ~/.local/bin/aivoicebus)")
	rollback := fs.Bool("rollback", false, "restore the previous binary kept by the last update")
	auto := fs.String("auto", "", "on | off | status | run: whether this machine installs newer signed releases by itself (on is the person's, typed in their own terminal; run is what the session-start hook starts)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["auto"] {
		if len(set) > 1 || fs.NArg() > 0 {
			return outbox.Usagef(autoUsage)
		}
		return autoUpdateCmd(*auto, s)
	}
	binPath := *bin
	if binPath == "" {
		var err error
		if binPath, err = installedBinary(); err != nil {
			return err
		}
	}
	if *rollback {
		// What it rolls back from is a release the person does not want: auto-update leaves it out.
		from, _ := updater.InstalledVersion(binPath, nil)
		if err := updater.Rollback(binPath, nil, s.Out); err != nil {
			return err
		}
		if release.ValidVersion(from) {
			_ = config.EditAutoState(func(st *config.AutoState) bool { st.Skip = from; return true })
		}
		return nil
	}
	src, err := releaseSource(*from)
	if err != nil {
		return err
	}
	_, err = updater.Update(updater.Options{
		Source: src, Trusted: trustedKeys(), Current: version.Release, Bin: binPath,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Check: *check, Force: *force, Out: s.Out,
	})
	if err != nil && *from != "github" && !strings.HasPrefix(*from, "dir:") {
		fmt.Fprintln(s.Err, "hint: if the nodes do not serve a release, use `aivoicebus update --from github`")
	}
	return err
}

func releaseSource(from string) (release.Source, error) {
	if from == "auto" {
		if _, err := client.FromEnv(); err != nil {
			from = "github" // no mesh configured: the project's public releases
		} else {
			from = "mesh"
		}
	}
	switch {
	case from == "mesh":
		c, err := client.FromEnv()
		if err != nil {
			return nil, err
		}
		c.Timeout = 2 * time.Minute // a binary is several MB, and the timeout covers the whole transfer
		return updater.MeshSource{C: c}, nil
	case from == "github":
		return &updater.GitHubSource{Repo: githubRepo}, nil
	case strings.HasPrefix(from, "dir:"):
		return release.DirSource{Dir: strings.TrimPrefix(from, "dir:")}, nil
	}
	return nil, fmt.Errorf("unknown --from %q: run it again with --from auto, mesh, github or dir:<path>", from)
}

// releaseCmd is for whoever publishes releases: keygen, pack, verify, nodes.
func releaseCmd(args []string, s Streams) error {
	if len(args) == 0 {
		return errors.New("usage: release keygen|pack|verify|nodes")
	}
	switch args[0] {
	case "nodes":
		return releaseNodes(args[1:], s)
	case "keygen":
		return releaseKeygen(args[1:], s)
	case "pack":
		return releasePack(args[1:], s)
	case "verify":
		return releaseVerify(args[1:], s)
	}
	return fmt.Errorf("unknown release command %q (keygen, pack, verify or nodes): `aivoicebus release -h` lists them", args[0])
}

func defaultKeyPath() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "release-signing.key"
	}
	return filepath.Join(h, ".config", "aivoicebus", "release-signing.key")
}

func releaseKeygen(args []string, s Streams) error {
	fs := newFlagSet("release keygen")
	out := fs.String("out", defaultKeyPath(), "where to write the private signing key (mode 600; never overwritten)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	priv, pub, err := release.GenerateKey()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		return err
	}
	if err := release.SavePrivateKey(*out, priv); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "private key written to %s (mode 600): keep it off the repo and the nodes, and back it up\n", *out)
	fmt.Fprintf(s.Out, "public key (put it in internal/release/keys.go): %s\n", release.EncodeKey(pub))
	return nil
}

func releasePack(args []string, s Streams) error {
	fs := newFlagSet("release pack")
	dir := fs.String("dir", "", "directory holding the aivoicebus-<os>-<arch> binaries; manifest and signature are written there")
	key := fs.String("key", defaultKeyPath(), "private signing key")
	ver := fs.String("version", version.Release, "release number (MAJOR.MINOR.PATCH)")
	commit := fs.String("commit", version.CommitOrBuildInfo(), "git commit the binaries were built from")
	notes := fs.String("notes", "", "one-line release notes")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("release pack needs --dir: run it again as `aivoicebus release pack --dir DIR`")
	}
	priv, err := release.LoadPrivateKey(*key)
	if err != nil {
		return err
	}
	cur := agentcfg.Current()
	m, err := release.Pack(release.PackOptions{Dir: *dir, Version: *ver, Commit: *commit, Notes: *notes, Key: priv,
		Parts: map[string]string{"hook": cur.Hook, "snippet": cur.Snippet, "usage": cur.Usage}})
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "packed and signed release %s (%d files) in %s\n", m.Version, len(m.Files), *dir)
	return nil
}

func releaseVerify(args []string, s Streams) error {
	fs := newFlagSet("release verify")
	dir := fs.String("dir", "", "release directory to verify")
	pub := fs.String("pubkey", "", "also trust this base64 public key, for this check only")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("release verify needs --dir: run it again as `aivoicebus release verify --dir DIR`")
	}
	keys := trustedKeys()
	if *pub != "" {
		k, err := release.ParseKey(*pub)
		if err != nil {
			return err
		}
		keys = append(keys, k)
	}
	m, err := release.VerifyDir(*dir, keys)
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "release %s verified: signature valid, %d file(s) match their hashes\n", m.Version, len(m.Files))
	return nil
}
