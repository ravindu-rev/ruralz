// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultHeaderTimeout is how long a connection on a proxyProtocol
// listener has, from its first Read, to deliver the complete PROXY v2
// header: ReadHeaderTimeout, 10 s (target; 04 reqs 10 and 16, 06 req 61).
const DefaultHeaderTimeout = 10 * time.Second

// TimeSource returns the current time; internal/clock.Clock satisfies it.
type TimeSource interface {
	Now() time.Time
}

// ListenerOptions configure NewListener.
type ListenerOptions struct {
	// Clock computes the header deadline; required. It must track wall
	// time: the deadline Clock.Now() + Timeout goes to the connection's
	// SetReadDeadline, which the operating system enforces against the
	// real clock, so a clock far behind refuses every connection at once
	// and one far ahead never times out. Tests pass clock.Real() or a
	// clocktest fake seeded from clock.Real().Now().
	Clock TimeSource
	// Timeout bounds the header read from the first Read or Write; 0
	// means DefaultHeaderTimeout.
	Timeout time.Duration
	// OnRefused, when set, is called once for every connection closed
	// because its header was missing, malformed, truncated or late
	// (ruralz_listener_connections_total{result="refused"}), with the TCP
	// peer and an error wrapping ErrMalformed. It is not called when the
	// connection was closed locally first. It runs on the goroutine that
	// first read the connection and must not block.
	OnRefused func(peer net.Addr, err error)
}

// ErrOptions is wrapped by NewListener for unusable options.
var ErrOptions = errors.New("clientaddr: invalid listener options")

// Listener requires a PROXY v2 header on every accepted connection
// (listeners[].proxyProtocol: true). Accept never reads from the
// connection: each Conn reads its header on first use, on the goroutine
// that serves it.
type Listener struct {
	net.Listener

	opts ListenerOptions
}

// NewListener wraps l. The wrapper owns no goroutine; closing it closes l.
func NewListener(l net.Listener, o ListenerOptions) (*Listener, error) {
	switch {
	case l == nil:
		return nil, fmt.Errorf("%w: nil listener", ErrOptions)
	case o.Clock == nil:
		return nil, fmt.Errorf("%w: Clock is required", ErrOptions)
	case o.Timeout < 0:
		return nil, fmt.Errorf("%w: negative Timeout %v", ErrOptions, o.Timeout)
	case o.Timeout == 0:
		o.Timeout = DefaultHeaderTimeout
	}
	return &Listener{Listener: l, opts: o}, nil
}

// Accept waits for the next connection and returns it as a *Conn.
func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &Conn{Conn: c, opts: &l.opts}, nil
}

// Conn is a connection that must begin with a PROXY v2 header. The first
// Read or Write reads the header (concurrent callers wait for it) under a
// deadline of the header timeout or any earlier read deadline the caller
// set, then restores the caller's read deadline. A missing, malformed or
// late header closes the connection, and every later Read and Write fails
// with a *net.OpError (Op "read" or "write") wrapping the cause, which
// net/http treats as a common network read error and answers with nothing.
type Conn struct {
	net.Conn

	opts *ListenerOptions

	once  sync.Once
	ready atomic.Bool // set after hdr is written, on success only
	hdr   Header
	err   error

	dlMu    sync.Mutex
	readDL  time.Time // the caller's read deadline
	parseDL time.Time // the header deadline while parsing, else zero

	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

// Read reads the header first, then passes through.
func (c *Conn) Read(b []byte) (int, error) {
	if err := c.handshake(); err != nil {
		return 0, c.opError("read", err)
	}
	return c.Conn.Read(b)
}

// Write reads the header first, so nothing reaches a client that never
// sent one, then passes through.
func (c *Conn) Write(b []byte) (int, error) {
	if err := c.handshake(); err != nil {
		return 0, c.opError("write", err)
	}
	return c.Conn.Write(b)
}

// Close closes the connection; it is idempotent.
func (c *Conn) Close() error {
	c.closed.Store(true)
	return c.closeInner()
}

// closeInner closes the wrapped connection once.
func (c *Conn) closeInner() error {
	c.closeOnce.Do(func() { c.closeErr = c.Conn.Close() })
	return c.closeErr
}

// CloseWrite half-closes the wrapped connection when it supports it
// (net/http uses it to avoid a TCP reset after an error response).
func (c *Conn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

// SetDeadline sets the read and write deadlines.
func (c *Conn) SetDeadline(t time.Time) error {
	if err := c.SetWriteDeadline(t); err != nil {
		return err
	}
	return c.SetReadDeadline(t)
}

// SetReadDeadline records t as the caller's read deadline; while the
// header is being read the earlier of t and the header deadline applies.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.dlMu.Lock()
	defer c.dlMu.Unlock()
	c.readDL = t
	return c.Conn.SetReadDeadline(c.effectiveReadDL())
}

// effectiveReadDL is the deadline to apply; dlMu must be held.
func (c *Conn) effectiveReadDL() time.Time {
	if !c.parseDL.IsZero() && (c.readDL.IsZero() || c.parseDL.Before(c.readDL)) {
		return c.parseDL
	}
	return c.readDL
}

// RemoteAddr returns the PROXY v2 source once a PROXY header was read,
// else the TCP peer. It never blocks: net/http calls it before the first
// Read, so Request.RemoteAddr is the TCP peer; use PeerOf in handlers.
func (c *Conn) RemoteAddr() net.Addr {
	if c.ready.Load() && c.hdr.Command == CommandProxy {
		return net.TCPAddrFromAddrPort(normalizeAddrPort(c.hdr.Source))
	}
	return c.Conn.RemoteAddr()
}

// NetConn returns the wrapped connection.
func (c *Conn) NetConn() net.Conn { return c.Conn }

// Header returns the PROXY v2 header once it was read successfully.
func (c *Conn) Header() (Header, bool) {
	if !c.ready.Load() {
		return Header{}, false
	}
	return c.hdr, true
}

// Peer returns the connection peer for the client-address rule (04 req
// 17): the PROXY v2 source after a PROXY header, else the TCP peer (LOCAL,
// or before the header was read), unmapped and without zone.
func (c *Conn) Peer() netip.AddrPort {
	if c.ready.Load() && c.hdr.Command == CommandProxy {
		return normalizeAddrPort(c.hdr.Source)
	}
	return addrPortOf(c.Conn.RemoteAddr())
}

// handshake reads the header once; every caller gets its outcome.
func (c *Conn) handshake() error {
	c.once.Do(c.readHeader)
	return c.err
}

// readHeader reads the header under the header deadline.
func (c *Conn) readHeader() {
	c.dlMu.Lock()
	c.parseDL = c.opts.Clock.Now().Add(c.opts.Timeout)
	err := c.Conn.SetReadDeadline(c.effectiveReadDL())
	c.dlMu.Unlock()

	var h Header
	if err == nil {
		h, err = ReadHeader(c.Conn)
	} else {
		err = readErr(err, false)
	}

	c.dlMu.Lock()
	c.parseDL = time.Time{}
	derr := c.Conn.SetReadDeadline(c.readDL)
	c.dlMu.Unlock()
	if err == nil && derr != nil && !c.closed.Load() {
		err = readErr(derr, true)
	}

	if err != nil {
		c.err = err
		if !c.closed.Load() && c.opts.OnRefused != nil {
			c.opts.OnRefused(c.Conn.RemoteAddr(), err)
		}
		_ = c.closeInner()
		return
	}
	c.hdr = h
	c.ready.Store(true)
}

// opError wraps err the way net reports read and write failures.
func (c *Conn) opError(op string, err error) error {
	oe := &net.OpError{Op: op, Source: c.LocalAddr(), Addr: c.Conn.RemoteAddr(), Err: err}
	if oe.Source != nil {
		oe.Net = oe.Source.Network()
	}
	return oe
}

// maxUnwrap bounds the NetConn chain PeerOf follows.
const maxUnwrap = 8

// PeerOf returns the connection peer of c for the client-address rule: it
// follows NetConn() wrappers (a *tls.Conn, a counting wrapper) down to a
// *Conn and returns its Peer; without one it returns c's remote TCP
// address, unmapped and without zone, or the zero AddrPort when c is not a
// TCP connection.
func PeerOf(c net.Conn) netip.AddrPort {
	if c == nil {
		return netip.AddrPort{}
	}
	cur := c
	for range maxUnwrap {
		if pc, ok := cur.(*Conn); ok {
			return pc.Peer()
		}
		u, ok := cur.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		if cur = u.NetConn(); cur == nil {
			break
		}
	}
	return addrPortOf(c.RemoteAddr())
}

// addrPortOf converts a TCP address, unmapped and without zone.
func addrPortOf(a net.Addr) netip.AddrPort {
	switch v := a.(type) {
	case nil:
		return netip.AddrPort{}
	case *net.TCPAddr:
		return normalizeAddrPort(v.AddrPort())
	}
	if a.Network() != "tcp" && a.Network() != "tcp4" && a.Network() != "tcp6" {
		return netip.AddrPort{}
	}
	p, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return netip.AddrPort{}
	}
	return normalizeAddrPort(p)
}
