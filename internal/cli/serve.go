package cli

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/server"
)

func serve(args []string, s Streams) error {
	fs := newFlagSet("serve")
	addr := fs.String("addr", "127.0.0.1:7777", "listen address (loopback by default; a private-network IP to serve other machines, never 0.0.0.0 on an open network)")
	data := fs.String("data", "./aivoicebus-data", "data dir")
	tf := fs.String("token-file", "", "token file")
	of := fs.String("observer-token-file", "", "file with a second, read-only token for observers such as the dashboard (optional)")
	node := fs.String("node", identity.Host(), "unique node id (stable across restarts)")
	peers := fs.String("peers", "", "comma-separated peer node URLs to replicate with")
	bfile := fs.String("bootstrap-file", "", "serve this client bundle at /bootstrap/<key>.tgz (temporary)")
	bkey := fs.String("bootstrap-key", "", "unguessable path component for the bootstrap download (min 24 chars)")
	bmax := fs.Duration("bootstrap-max-age", 24*time.Hour, "stop serving the bundle when the file is older than this")
	every := fs.Duration("sync-every", 3*time.Second, "peer pull interval")
	rdir := fs.String("release-dir", "", "serve this release directory (manifest, signature, binaries) at /v1/release/")
	requireSigned := fs.Bool("require-signed", os.Getenv("AIVOICEBUS_REQUIRE_SIGNED") == "1", "refuse messages that carry no valid signature (every client of 0.7.0 or later signs); also AIVOICEBUS_REQUIRE_SIGNED=1")
	allowPublic := fs.Bool("allow-public", false, "listen on all interfaces, a public address or a host name (plain HTTP: put it behind TLS you control)")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	warn, err := checkListenAddr(*addr, *allowPublic)
	if err != nil {
		return err
	}
	if warn != "" {
		fmt.Fprintln(s.Err, "warning:", warn)
	}
	return server.Run(server.Options{
		Addr:            *addr,
		DataDir:         *data,
		Node:            *node,
		TokenFile:       *tf,
		ObserverFile:    *of,
		Peers:           strings.Split(*peers, ","),
		SyncEvery:       *every,
		BootstrapFile:   *bfile,
		BootstrapKey:    *bkey,
		BootstrapMaxAge: *bmax,
		ReleaseDir:      *rdir,
		RequireSigned:   *requireSigned,
		Out:             s.Out,
	})
}

// checkListenAddr judges where a node would listen. Loopback is always fine. A private, link-local or
// tailnet address is fine with a reminder that the transport is plain HTTP. All interfaces, a public
// address or a host name (which could resolve to either) needs --allow-public, because a node on
// the open internet carries its token and every message in clear text.
func checkListenAddr(addr string, allowPublic bool) (warn string, err error) {
	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		return "", fmt.Errorf("--addr %q is not host:port: %v", addr, splitErr)
	}
	if host == "localhost" {
		return "", nil
	}
	ip, parseErr := netip.ParseAddr(strings.Trim(host, "[]"))
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	switch {
	case parseErr == nil && ip.IsLoopback():
		return "", nil
	case parseErr == nil && !ip.IsUnspecified() && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)):
		return "this node listens on " + addr + " over plain HTTP: the token and every message cross that network unencrypted, so keep it a network you trust", nil
	}
	what := "a host name"
	switch {
	case host == "" || (parseErr == nil && ip.IsUnspecified()):
		what = "all interfaces"
	case parseErr == nil:
		what = "a public address"
	}
	if !allowPublic {
		return "", errors.New("--addr " + addr + " is " + what + ": this node speaks plain HTTP with a shared token, so anyone who can reach it can read the traffic. " +
			"Listen on loopback or a private-network address, or run it again with --allow-public if something else (a TLS proxy, a firewall) protects it")
	}
	return "this node listens on " + what + " (" + addr + ") over plain HTTP: the token and every message are readable by anyone on the path; terminate TLS in front of it", nil
}
