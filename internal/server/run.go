package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/sharestore"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// Options configure one node.
type Options struct {
	Addr            string
	DataDir         string
	Node            string // unique, stable node id
	TokenFile       string
	ObserverFile    string // read-only token for observers such as the dashboard (optional)
	Peers           []string
	SyncEvery       time.Duration
	BootstrapFile   string
	BootstrapKey    string
	BootstrapMaxAge time.Duration
	ReleaseDir      string    // serve this release directory at /v1/release/ (optional)
	RequireSigned   bool      // refuse messages without a valid signature
	Out             io.Writer // progress lines; nil discards them
}

// Run starts the node and blocks until the listener fails.
func Run(o Options) error {
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	tokens, err := loadNodeTokens(o.TokenFile)
	if err != nil {
		return err
	}
	observer, err := loadObserverToken(o.ObserverFile, tokens.Primary())
	if err != nil {
		return err
	}
	if observer != "" && tokens.Has(observer) {
		return errors.New("the observer token must differ from every shared token: make a new one with `deploy/install.sh --gen-observer-token` (from the root of an aivoicebus checkout), then try again")
	}
	st, err := store.Open(o.DataDir, o.Node, store.WithLog(out))
	if err != nil {
		return err
	}
	var peers []string
	for _, p := range o.Peers {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			peers = append(peers, p)
		}
	}
	tok := tokens.Primary() // a node authenticates to its peers with the first unrestricted token
	// Recover our own log from peers before taking writes, so a node that
	// lost its disk never reuses sequence numbers under its node id.
	for _, p := range peers {
		if n, err := PullOnce(st, p, tok); err == nil && n > 0 {
			fmt.Fprintf(out, "recovered %d record(s) from %s\n", n, p)
		}
	}
	board := NewPeerBoard(peers)
	go Replicate(context.Background(), st, peers, tok, o.SyncEvery, board)
	sh, err := sharestore.Open(filepath.Join(o.DataDir, "share"), o.Node, sharestore.DefaultLimits(), ThreadActivity(st), sharestore.WithLog(out))
	if err != nil {
		return fmt.Errorf("shared spaces in %s: %v", filepath.Join(o.DataDir, "share"), err)
	}
	shBoard := &shareBoard{}
	for _, p := range peers { // like the message log: take back what this node wrote before it lost its disk
		if n, _, err := pullShares(sh, p, tok, out); err == nil {
			shBoard.ok()
			if n > 0 {
				fmt.Fprintf(out, "recovered %d shared-space record(s) from %s\n", n, p)
			}
		}
	}
	go ReplicateShares(context.Background(), sh, peers, tok, o.SyncEvery, shBoard, out)
	fmt.Fprintf(out, "node %s listening on %s, %d peer(s)\n", o.Node, o.Addr, len(peers))
	if o.BootstrapFile != "" && len(o.BootstrapKey) < 24 {
		return errors.New("--bootstrap-key must be at least 24 chars: run it again with a longer one (`openssl rand -hex 24` makes one)")
	}
	h := WithBootstrap(NewHandler(st, "", WithTokenSet(tokens), WithObserverToken(observer), WithReleaseDir(o.ReleaseDir),
		WithNode(o.Node), WithRequireSigned(o.RequireSigned), WithPeerBoard(board), WithReady(Ready(st, board, len(peers))),
		WithShares(sh, shareReady(sh, shBoard, len(peers)))), o.BootstrapKey, o.BootstrapFile, o.BootstrapMaxAge)
	srv := defaultLimits().server(o.Addr, h)
	// A stop (systemd sends SIGTERM) writes the roster to disk first, so a restart
	// or an upgrade does not forget who is around.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	drained := make(chan struct{})
	go func() {
		<-stop
		st.FlushPresence()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		close(drained)
	}()
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return err
	}
	if err := srv.Serve(limitListener(ln, maxConns)); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Serve returns as soon as Shutdown starts; wait for it to finish, so requests in
	// flight (a write being acknowledged) are not cut off by the process exiting.
	select {
	case <-drained:
	case <-time.After(4 * time.Second):
	}
	return nil
}

// loadObserverToken reads the observer token from file ("" means none). It must
// be long enough to guess nothing and different from the shared token, or the
// observer would not be limited to anything.
func loadObserverToken(file, shared string) (string, error) {
	if file == "" {
		return "", nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("observer token file: %v", err)
	}
	tok := strings.TrimSpace(string(b))
	switch {
	case len(tok) < 24:
		return "", errors.New("observer token missing or shorter than 24 chars: make one with `deploy/install.sh --gen-observer-token` (from the root of an aivoicebus checkout), then try again")
	case tok == shared:
		return "", errors.New("the observer token must differ from the shared token: make a new one with `deploy/install.sh --gen-observer-token` (from the root of an aivoicebus checkout), then try again")
	}
	return tok, nil
}

// loadNodeTokens reads the node's tokens: a token file (see ParseTokens), or the
// single token from the environment for a node started without one.
func loadNodeTokens(file string) (*TokenSet, error) {
	if file == "" {
		tok := config.Token("")
		if len(tok) < MinTokenLen {
			return nil, errors.New("token missing or shorter than 24 chars: set AIVOICEBUS_TOKEN or pass --token-file, then try again")
		}
		return NewTokenSet(tok), nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("token file: %v", err)
	}
	return ParseTokens(string(b))
}
