package cli

import (
	"crypto/ed25519"
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
		return errors.New("usage: release keygen|pubkey|pack|verify|nodes")
	}
	switch args[0] {
	case "nodes":
		return releaseNodes(args[1:], s)
	case "keygen":
		return releaseKeygen(args[1:], s)
	case "pubkey":
		return releasePubkey(args[1:], s)
	case "pack":
		return releasePack(args[1:], s)
	case "verify":
		return releaseVerify(args[1:], s)
	}
	return fmt.Errorf("unknown release command %q (keygen, pubkey, pack, verify or nodes): `aivoicebus release -h` lists them", args[0])
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
	out := fs.String("out", defaultKeyPath(), "where to write the private signing key (mode 600; never overwritten); the public key goes beside it as .pub")
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
	fmt.Fprintf(s.Out, "private key written to %s (mode 600): it never leaves this machine; keep it off the repo, the nodes and every message, and back it up\n", *out)
	return printPublicKey(s, pub, *out)
}

// releasePubkey prints the public half of this machine's signing key and writes it beside the key as
// .pub, which is what lets the machine advertise the release-key fact. The private key is read to
// derive it and is never printed.
func releasePubkey(args []string, s Streams) error {
	fs := newFlagSet("release pubkey")
	key := fs.String("key", defaultKeyPath(), "private signing key (read to derive its public half, never printed)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	priv, err := release.LoadPrivateKey(*key)
	if err != nil {
		return err
	}
	return printPublicKey(s, priv.Public().(ed25519.PublicKey), *key)
}

// printPublicKey writes pub beside the private key at keyPath (release.PublicKeyPath) and prints it with
// its fingerprint and whether this binary already trusts it.
func printPublicKey(s Streams, pub ed25519.PublicKey, keyPath string) error {
	pubPath := release.PublicKeyPath(keyPath)
	if err := release.SavePublicKey(pubPath, pub); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "public key (written to %s): %s\n", pubPath, release.EncodeKey(pub))
	fmt.Fprintf(s.Out, "fingerprint: %s\n", release.Fingerprint(pub))
	if k, ok := release.Builtin(pub); ok {
		fmt.Fprintf(s.Out, "this binary trusts it: built in for %s since release %s\n", k.Label, k.Since)
		return nil
	}
	fmt.Fprintln(s.Out, "this binary does not trust it yet: send the public key and the fingerprint (by two different routes) to whoever adds signers in internal/release/keys.go (docs/releasing.md, \"Adding a signer\")")
	return nil
}

func releasePack(args []string, s Streams) error {
	fs := newFlagSet("release pack")
	dir := fs.String("dir", "", "directory holding the aivoicebus-<os>-<arch> binaries; manifest and signature are written there")
	key := fs.String("key", defaultKeyPath(), "private signing key")
	ver := fs.String("version", version.Release, "release number (MAJOR.MINOR.PATCH)")
	commit := fs.String("commit", version.CommitOrBuildInfo(), "git commit the binaries were built from")
	notes := fs.String("notes", "", "one-line release notes")
	anySigner := fs.Bool("any-signer", false, "sign even with a built-in key that older releases do not carry yet (only for a rehearsal or a test: machines on those releases will refuse it)")
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
	note, err := release.CheckSigner(priv.Public().(ed25519.PublicKey), *ver)
	if err != nil && !*anySigner {
		return err
	} else if err != nil {
		note = err.Error() + " (signed anyway: --any-signer)"
	}
	if note != "" {
		fmt.Fprintln(s.Err, "note:", note)
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
	if who := signedBy(*dir, *pub); who != "" {
		fmt.Fprintf(s.Out, "signed by %s\n", who)
	}
	return nil
}

// signedBy names the key that signed the release in dir ("mac14-m1pro-mbp SHA256:..."), or "" when it
// cannot tell. It is only said after VerifyDir passed, so it is information, not a check.
func signedBy(dir, extra string) string {
	keys, _ := release.Keys()
	if extra != "" {
		if k, err := release.ParseKey(extra); err == nil {
			keys = append(keys, release.Key{Public: k, Source: "--pubkey"})
		}
	}
	mb, err1 := os.ReadFile(filepath.Join(dir, release.ManifestName))
	sb, err2 := os.ReadFile(filepath.Join(dir, release.SignatureName))
	if err1 != nil || err2 != nil {
		return ""
	}
	k, err := release.Signer(mb, sb, keys)
	if err != nil {
		return ""
	}
	label := k.Label
	if label == "" {
		label = k.Source
	}
	return label + " " + release.Fingerprint(k.Public)
}
