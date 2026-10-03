// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// ConnLimiter is a connection ceiling shared by every listener it wraps
// (spec 04 req 11): the Node-wide client ceiling across all client
// listeners, or the admin listener's own. A wrapped listener takes a slot
// before calling the inner Accept and the accepted connection returns it on
// its first Close; at the ceiling the listener stops calling Accept, so
// nothing is refused and the kernel's accept queue holds new connections
// until a slot frees. ConnLimiter is safe for concurrent use.
type ConnLimiter struct {
	mu sync.Mutex
	// slots counts the slots held against the ceiling: accepted
	// connections plus accept loops waiting in the inner Accept.
	slots int64
	// conns counts accepted connections not yet closed (Open).
	conns    int64
	limit    int64
	external int64
	waiters  int
	// wake is closed and replaced when a slot may have freed while
	// waiters > 0.
	wake chan struct{}
}

// NewConnLimiter returns a ceiling of limit open connections
// (DefaultConnections for client listeners, AdminConnections for admin).
func NewConnLimiter(limit int64) *ConnLimiter {
	return &ConnLimiter{limit: max(0, limit), wake: make(chan struct{})}
}

// Wrap returns ln counting its connections against the ceiling. Closing the
// returned listener unblocks an Accept waiting for a slot, which then
// returns an error matching net.ErrClosed.
//
// Wrap the raw listener: the TCP listener, or the PROXY v2 listener over it,
// below tls.NewListener, so TLS wraps the limited connections. Accepted
// connections are not *tls.Conn values, so a limited listener placed above
// TLS would hide the TLS connection from http.Server, which type-asserts
// *tls.Conn to negotiate ALPN h2 and to set Request.TLS: HTTP/2 over TLS
// would silently stop working.
func (c *ConnLimiter) Wrap(ln net.Listener) net.Listener {
	return &limitedListener{Listener: ln, c: c, done: make(chan struct{})}
}

// SetExternal lowers the ceiling by n connections, the connections the
// other process reported during a handover (spec 04 req 67); 0 restores
// the full ceiling. Open connections are kept: the lowered ceiling applies
// to new accepts, and a raised one wakes waiting accept loops.
func (c *ConnLimiter) SetExternal(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.external = max(0, n)
	c.broadcastLocked()
}

// Open returns the accepted connections open now (the handover usage
// report's connections, spec 04 req 67). It does not count the slot each
// accept loop holds while it waits in the inner Accept: that slot counts
// against this ceiling but is no connection, so the other process does not
// lower its own ceiling for it.
func (c *ConnLimiter) Open() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conns
}

// Ceiling returns the effective ceiling: the limit minus the external
// usage.
func (c *ConnLimiter) Ceiling() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return effective(c.limit, c.external)
}

// acquire takes a slot, waiting while the ceiling is reached, until done is
// closed.
func (c *ConnLimiter) acquire(done <-chan struct{}) bool {
	c.mu.Lock()
	for c.slots >= effective(c.limit, c.external) {
		wake := c.wake
		c.waiters++
		c.mu.Unlock()
		select {
		case <-wake:
		case <-done:
			c.mu.Lock()
			c.waiters--
			c.mu.Unlock()
			return false
		}
		c.mu.Lock()
		c.waiters--
	}
	c.slots++
	c.mu.Unlock()
	return true
}

// accepted counts a connection accepted on a slot acquire took.
func (c *ConnLimiter) accepted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conns++
}

// release returns a slot, and the connection it held when conn is set, and
// wakes waiting accept loops.
func (c *ConnLimiter) release(conn bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slots--
	if conn {
		c.conns--
	}
	c.broadcastLocked()
}

func (c *ConnLimiter) broadcastLocked() {
	if c.waiters > 0 {
		close(c.wake)
		c.wake = make(chan struct{})
	}
}

// limitedListener is a net.Listener counting its connections.
type limitedListener struct {
	net.Listener
	c         *ConnLimiter
	done      chan struct{}
	closeOnce sync.Once
}

// Accept waits for a slot, then accepts from the inner listener.
func (l *limitedListener) Accept() (net.Conn, error) {
	if !l.c.acquire(l.done) {
		return nil, &net.OpError{Op: "accept", Net: l.Addr().Network(), Addr: l.Addr(), Err: net.ErrClosed}
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		l.c.release(false)
		return nil, err
	}
	l.c.accepted()
	return &limitedConn{Conn: conn, c: l.c}, nil
}

// Close closes the inner listener and unblocks a waiting Accept.
func (l *limitedListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// limitedConn returns its slot on the first Close.
type limitedConn struct {
	net.Conn
	c      *ConnLimiter
	closed atomic.Bool
}

// Close closes the connection and returns its slot once.
func (lc *limitedConn) Close() error {
	err := lc.Conn.Close()
	if lc.closed.CompareAndSwap(false, true) {
		lc.c.release(true)
	}
	return err
}

// NetConn returns the wrapped connection (the TCP or PROXY v2 connection).
func (lc *limitedConn) NetConn() net.Conn { return lc.Conn }

// CloseWrite half-closes the connection when the inner one supports it;
// net/http uses it to finish an HTTP/1.1 response before closing.
func (lc *limitedConn) CloseWrite() error {
	if cw, ok := lc.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// ReadFrom keeps the inner connection's io.ReaderFrom fast path (splice
// and sendfile on a TCP connection).
func (lc *limitedConn) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := lc.Conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(writerOnly{lc.Conn}, r)
}

// writerOnly hides a writer's ReadFrom so io.Copy does not recurse.
type writerOnly struct{ io.Writer }
