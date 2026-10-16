package server

import (
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// What one connection, and the requests on it, may cost a node. Anything that can reach the
// port can open a connection without a token, and a bare http.Server keeps every one of them
// for ever: 20,000 half-open connections took a node from 7 MB to 285 MB, past the 256 MB its
// unit allows. None of this is policy about who may talk (the token decides that).
const (
	readHeaderTimeout = 10 * time.Second // a client that has not finished its headers by then is not going to
	readTimeout       = 30 * time.Second // the whole request: a message is at most about 36 KB
	writeTimeout      = 30 * time.Second // the whole response: a sync page is about 1 MB
	idleTimeout       = 2 * time.Minute  // longer than the 90 s a Go client keeps an idle connection, so the client lets go first
	maxHeaderBytes    = 64 << 10
	maxConns          = 512 // open connections in all, not per address: one machine runs many sessions
	longWriteTimeout  = 10 * time.Minute

	// A sync page stops at either: records, or about this many bytes (it always holds one
	// record). A client that asks again until it gets nothing, as every consumer of the feed
	// does, loses nothing, and one page of 500 legal 32 KB messages was 98 MB.
	syncPageRecords = 500
	syncPageBytes   = 1 << 20
)

// limits are the values above, as a struct so a test can shorten them.
type limits struct {
	readHeader, read, write, idle time.Duration
	maxHeader, maxConns           int
}

func defaultLimits() limits {
	return limits{readHeaderTimeout, readTimeout, writeTimeout, idleTimeout, maxHeaderBytes, maxConns}
}

// server is an http.Server on addr with these limits.
func (l limits) server(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: l.readHeader, ReadTimeout: l.read,
		WriteTimeout: l.write, IdleTimeout: l.idle, MaxHeaderBytes: l.maxHeader}
}

// longWrite gives the response being written until longWriteTimeout instead of writeTimeout, for
// a download of a release or a client bundle (about 10 MB, over whatever link a machine has).
func longWrite(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(longWriteTimeout)) // not supported: the server's own timeout stays
}

// limitListener accepts at most n connections at a time. Past that it stops accepting, so the
// excess waits in the kernel's queue, which costs the node nothing, until an open one is closed
// (by the client, or by the timeouts above).
func limitListener(l net.Listener, n int) *capListener {
	return &capListener{Listener: l, sem: make(chan struct{}, n), done: make(chan struct{})}
}

type capListener struct {
	net.Listener
	sem       chan struct{} // a slot is taken before a connection is accepted, so one is held by the Accept that waits
	done      chan struct{}
	closeOnce sync.Once
	served    atomic.Int64
}

// open is how many connections are being served.
func (l *capListener) open() int { return int(l.served.Load()) }

func (l *capListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	l.served.Add(1)
	return &capConn{Conn: c, release: sync.OnceFunc(func() { l.served.Add(-1); <-l.sem })}, nil
}

func (l *capListener) Close() error {
	err := l.Listener.Close()
	l.closeOnce.Do(func() { close(l.done) })
	return err
}

type capConn struct {
	net.Conn
	release func()
}

func (c *capConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}
