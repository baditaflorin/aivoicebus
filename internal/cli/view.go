package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/viewer"
)

// viewCmd serves the read-only viewer: session cards and the conversations between sessions.
func viewCmd(args []string, s Streams) error {
	fs := newFlagSet("view")
	nodes := fs.String("node", "", "node URLs, comma-separated (default: AIVOICEBUS_URL)")
	tokenFile := fs.String("observer-token-file", "", "the mesh's read-only observer token (default ~/.config/aivoicebus/observer-token)")
	listen := fs.String("listen", "127.0.0.1:7788", "where the page is served: a loopback address only")
	demo := fs.Bool("demo", false, "show an invented board instead of a mesh (no node needed)")
	archive := fs.String("archive", "", "keep the replica in this directory (it holds every message in clear text); default: a private temporary one, removed on exit")
	allowFull := fs.Bool("allow-full-token", false, "accept a token that can also send and read inboxes (the viewer never uses those rights)")
	every := fs.Duration("every", 3*time.Second, "how often the nodes are asked for news")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if err := loopbackOnly(*listen); err != nil {
		return err
	}

	var c *client.Client
	if !*demo {
		var err error
		if c, err = observerClient(*nodes, *tokenFile, *allowFull, s); err != nil {
			return err
		}
	}

	dir := *archive
	if dir == "" {
		tmp, err := os.MkdirTemp("", "aivoicebus-view-*") // 0700
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	v, err := viewer.Open(viewer.Options{Client: c, Dir: filepath.Join(dir, "replica"), Every: *every})
	if err != nil {
		return err
	}
	if *demo {
		if err := v.Demo(time.Now()); err != nil {
			return err
		}
	}

	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	srv := &http.Server{
		Handler:           v.Handler(viewer.HTTPOptions{Key: hex.EncodeToString(key), Hosts: viewer.LoopbackHosts(addr)}),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go v.Run(ctx)
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	what := "following " + strings.Join(cURLs(c), ", ")
	if *demo {
		what = "showing an invented demo board"
	}
	fmt.Fprintf(s.Out, "aivoicebus viewer (read-only), %s\nopen: http://%s/?key=%s\nCtrl-C stops it.\n", what, addr, hex.EncodeToString(key))
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cURLs(c *client.Client) []string {
	if c == nil {
		return nil
	}
	return c.URLs
}

// loopbackOnly refuses any listen address that is not loopback: the viewer shows every message on
// the mesh, and it has no login of its own beyond the one-time key.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen %q is not host:port: %v", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("--listen %s is not a loopback address: the viewer shows every message on the mesh, so it serves this machine only (reach it from elsewhere through an ssh tunnel)", addr)
}

// observerClient is a client for the nodes with the observer token, after checking with a node that
// the token is the read-only one.
func observerClient(nodes, tokenFile string, allowFull bool, s Streams) (*client.Client, error) {
	urls := splitList(nodes)
	if len(urls) == 0 {
		urls = config.URLs()
	}
	if len(urls) == 0 {
		return nil, errors.New("no node to follow: pass --node http://host:7777 or set AIVOICEBUS_URL (or try --demo)")
	}
	if err := checkNodeURLs(urls); err != nil {
		return nil, err
	}
	if tokenFile == "" {
		tokenFile = filepath.Join(config.Dir(), "observer-token")
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("no observer token: %v (a mesh operator makes one with deploy/install.sh --gen-observer-token; pass --observer-token-file)", err)
	}
	if fi, err := os.Stat(tokenFile); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by other users (mode %04o); chmod 600 it", tokenFile, fi.Mode().Perm())
	}
	tok := strings.TrimSpace(string(b))
	if len(tok) < minTokenLen {
		return nil, fmt.Errorf("%s does not hold a token (at least %d characters)", tokenFile, minTokenLen)
	}
	c := &client.Client{URLs: urls, Token: tok, Timeout: 20 * time.Second, Warn: s.Err}
	info, err := c.TokenInfo()
	switch {
	case err != nil:
		return nil, fmt.Errorf("checking the token with the nodes: %v", err)
	case !info.Observer && !allowFull:
		return nil, errors.New("this token can send and read inboxes, not only observe: give the viewer the mesh's observer token, or pass --allow-full-token")
	case !info.Observer:
		fmt.Fprintln(s.Err, "warning: following the mesh with a full token (--allow-full-token); the viewer only reads with it")
	}
	return c, nil
}
