// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/controlstore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// ioTimeout is the I/O deadline of a handshake and of raft's RPCs, the
// one hashicorp/raft's own TCP transport applies. Socket deadlines run on
// real time.
const ioTimeout = 10 * time.Second

// TLSSource supplies this replica's 8092 credentials, read at every
// handshake so a renewed peer certificate applies to new connections.
type TLSSource interface {
	// Certificate returns the peer certificate, issued by the peer CA
	// with this replica's urn:ruralz:replica identity and its advertised
	// host, and its key.
	Certificate() (*tls.Certificate, error)
	// PeerCAs returns the peer CA pool. It never holds the Node CA, so a
	// Node key cannot join Raft (TB-11).
	PeerCAs() *x509.CertPool
}

// StaticTLS is a TLSSource with fixed credentials.
type StaticTLS struct {
	Cert tls.Certificate
	Pool *x509.CertPool
}

// Certificate returns the certificate.
func (s StaticTLS) Certificate() (*tls.Certificate, error) { return &s.Cert, nil }

// PeerCAs returns the pool.
func (s StaticTLS) PeerCAs() *x509.CertPool { return s.Pool }

// PeerConn is one authorized 8092 connection handed to a class handler.
type PeerConn struct {
	net.Conn
	// Class is the negotiated class.
	Class controlstore.Class
	// Peer is the verified peer identity; zero for a Join without a
	// certificate.
	Peer controlstore.PeerIdentity
	// Certificate is the verified client certificate, nil without one.
	Certificate *x509.Certificate
}

// Handler serves one connection of a class; the connection closes when
// it returns.
type Handler func(ctx context.Context, c *PeerConn)

// errTransportClosed reports a closed transport.
var errTransportClosed = errors.New("controlstore: 8092 transport closed")

// transport owns the 8092 listener: TLS 1.3 only, one class per
// connection named by ALPN, every class but Join behind a verified peer
// certificate, and each class authorized by the certificate's role and,
// once this replica knows the membership, its server ID
// (docs/architecture/04-control-plane-and-gitops.md "Replica roles",
// ADR-0006 "Transport").
type transport struct {
	ln        net.Listener
	advertise string
	src       TLSSource
	clk       clock.Clock
	rclk      clock.Clock
	log       *slog.Logger
	// authz refuses a revoked server ID, and one neither a member nor
	// pending once membership is known.
	authz    func(controlstore.PeerIdentity) error
	handlers map[controlstore.Class]Handler
	lim      *limiters

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	raftCh chan net.Conn

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	// heard is when each peer was last heard from on a Raft connection,
	// on the real clock raft's timing runs on.
	heard map[controlstore.ServerID]time.Time
}

// newTransport returns the transport of ln; it accepts nothing until
// start, so no handler runs before the replica it serves exists.
func newTransport(ln net.Listener, advertise string, src TLSSource, clk, rclk clock.Clock, log *slog.Logger,
	authz func(controlstore.PeerIdentity) error, handlers map[controlstore.Class]Handler,
) *transport {
	ctx, cancel := context.WithCancel(context.Background())
	return &transport{
		ln: ln, advertise: advertise, src: src, clk: clk, rclk: rclk, log: log, authz: authz, handlers: handlers,
		lim: newLimiters(rclk, PeerRate), ctx: ctx, cancel: cancel, raftCh: make(chan net.Conn),
		conns: map[net.Conn]struct{}{}, heard: map[controlstore.ServerID]time.Time{},
	}
}

// start begins accepting connections.
func (t *transport) start() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.wg.Go(t.serve)
	}
}

// serverConfig is the per-connection TLS configuration: exactly one
// class, the current certificate and peer CA pool, a required client
// certificate except for Join.
func (t *transport) serverConfig(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	if len(hello.SupportedProtos) != 1 {
		return nil, fmt.Errorf("controlstore: an 8092 connection names exactly one class, not %d", len(hello.SupportedProtos))
	}
	class := controlstore.Class(hello.SupportedProtos[0])
	if !slices.Contains(controlstore.Classes(), class) {
		return nil, fmt.Errorf("controlstore: unknown 8092 class %q", class)
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		NextProtos: []string{string(class)},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return t.src.Certificate()
		},
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  t.src.PeerCAs(),
		Time:       t.clk.Now,
	}
	if class == controlstore.ClassJoin {
		// Only Join takes a token without a client certificate; a given
		// one still verifies against the peer CA.
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
	}
	return cfg, nil
}

// serve accepts connections until the transport closes.
func (t *transport) serve() {
	base := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, GetConfigForClient: t.serverConfig}
	backoff := time.Duration(0)
	for {
		c, err := t.ln.Accept()
		if err != nil {
			if t.ctx.Err() != nil {
				return
			}
			// Accept errors such as a full file table pass; back off as
			// net/http does, from 5 ms to 1 s.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			t.log.Warn("8092 accept failed", slog.String(catalog.KeyError, err.Error()))
			if t.rclk.Sleep(t.ctx, backoff) != nil {
				return
			}
			continue
		}
		backoff = 0
		if !t.track(c) {
			_ = c.Close()
			return
		}
		t.wg.Go(func() { t.handle(tls.Server(c, base)) })
	}
}

// track registers an open connection; false once closed.
func (t *transport) track(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.conns[c] = struct{}{}
	return true
}

// untrack forgets a closed connection.
func (t *transport) untrack(c net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.conns, c)
}

// handle completes the handshake, authorizes the class and dispatches.
func (t *transport) handle(tc *tls.Conn) {
	raw := tc.NetConn()
	handed := false
	defer func() {
		if !handed {
			_ = tc.Close()
			t.untrack(raw)
		}
	}()
	_ = tc.SetDeadline(t.rclk.Now().Add(ioTimeout))
	if err := tc.HandshakeContext(t.ctx); err != nil {
		t.log.Debug("8092 handshake refused", slog.String(catalog.KeyError, err.Error()))
		return
	}
	_ = tc.SetDeadline(time.Time{})
	cs := tc.ConnectionState()
	class := controlstore.Class(cs.NegotiatedProtocol)
	pc := &PeerConn{Conn: tc, Class: class}
	if len(cs.PeerCertificates) > 0 {
		pc.Certificate = cs.PeerCertificates[0]
		id, err := controlstore.PeerIdentityOf(pc.Certificate)
		if err != nil {
			t.log.Warn("8092 connection refused: certificate names no peer", slog.String(catalog.KeyError, err.Error()))
			return
		}
		pc.Peer = id
	}
	if err := t.authorize(class, pc); err != nil {
		t.log.Warn("8092 connection refused", slog.String(catalog.KeyReason, err.Error()))
		return
	}
	if class == controlstore.ClassRaft {
		rc := &raftConn{Conn: tc, t: t, peer: pc.Peer.ServerID, lim: t.lim.of(pc.Peer.ServerID)}
		select {
		case t.raftCh <- rc:
			handed = true
		case <-t.ctx.Done():
		}
		return
	}
	h := t.handlers[class]
	if h == nil {
		return
	}
	if class == controlstore.ClassForward {
		pc.Conn = &limitedConn{Conn: tc, ctx: t.ctx, lim: t.lim.of(pc.Peer.ServerID)}
	}
	h(t.ctx, pc)
}

// authorize applies the class's role rule and the membership rule.
func (t *transport) authorize(class controlstore.Class, pc *PeerConn) error {
	if !class.Admits(pc.Peer.Role) {
		if pc.Peer.Role == "" {
			return fmt.Errorf("class %s needs a peer certificate", class)
		}
		return fmt.Errorf("class %s does not admit role %s", class, pc.Peer.Role)
	}
	if pc.Peer.ServerID == "" {
		return nil
	}
	return t.authz(pc.Peer)
}

// dial opens a connection of one class to a peer, verifying its
// certificate against the peer CA pool, its role and its server ID.
func (t *transport) dial(ctx context.Context, address string, class controlstore.Class) (*tls.Conn, controlstore.PeerIdentity, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, controlstore.PeerIdentity{}, fmt.Errorf("controlstore: peer address %q: %w", address, err)
	}
	d := &tls.Dialer{Config: &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		RootCAs:    t.src.PeerCAs(),
		ServerName: host,
		NextProtos: []string{string(class)},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return t.src.Certificate()
		},
		Time: t.clk.Now,
	}}
	c, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, controlstore.PeerIdentity{}, fmt.Errorf("controlstore: dial %s: %w", address, err)
	}
	tc, ok := c.(*tls.Conn)
	if !ok {
		_ = c.Close()
		return nil, controlstore.PeerIdentity{}, fmt.Errorf("controlstore: dial %s: not a TLS connection", address)
	}
	cs := tc.ConnectionState()
	fail := func(format string, args ...any) (*tls.Conn, controlstore.PeerIdentity, error) {
		_ = tc.Close()
		return nil, controlstore.PeerIdentity{}, fmt.Errorf("controlstore: dial %s: "+format, append([]any{address}, args...)...)
	}
	if controlstore.Class(cs.NegotiatedProtocol) != class || len(cs.PeerCertificates) == 0 {
		return fail("the peer did not accept class %s", class)
	}
	id, err := controlstore.PeerIdentityOf(cs.PeerCertificates[0])
	if err != nil {
		return fail("%w", err)
	}
	if id.Role != controlstore.RoleReplica {
		return fail("the peer has role %s, not replica", id.Role)
	}
	if err := t.authz(id); err != nil {
		return fail("%w", err)
	}
	if !t.track(tc.NetConn()) {
		return fail("%w", errTransportClosed)
	}
	return tc, id, nil
}

// release closes a dialed connection and forgets it.
func (t *transport) release(tc *tls.Conn) {
	_ = tc.Close()
	t.untrack(tc.NetConn())
}

// heardFrom records a read from a peer.
func (t *transport) heardFrom(id controlstore.ServerID) {
	now := t.rclk.Now()
	t.mu.Lock()
	t.heard[id] = now
	t.mu.Unlock()
}

// lastHeard returns when a peer was last heard from on a Raft connection.
func (t *transport) lastHeard(id controlstore.ServerID) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.heard[id]
	return at, ok
}

// close stops accepting, closes every connection and waits for the
// handlers.
func (t *transport) close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	conns := make([]net.Conn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	t.mu.Unlock()
	t.cancel()
	_ = t.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	t.wg.Wait()
}

// dialedConn is a dialed connection of an embedder class: its writes go
// through the peer's budget behind Raft, and Close forgets it.
type dialedConn struct {
	*tls.Conn
	t    *transport
	lim  *limiter
	once sync.Once
}

// Write waits for the peer's budget.
func (c *dialedConn) Write(p []byte) (int, error) {
	return (&limitedWriter{ctx: c.t.ctx, w: c.Conn, lim: c.lim}).Write(p)
}

// Close closes and forgets the connection once.
func (c *dialedConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		c.t.untrack(c.NetConn())
	})
	return err
}

// limitedConn is an accepted connection of an embedder class whose writes
// go through the peer's budget behind Raft.
type limitedConn struct {
	net.Conn
	ctx context.Context
	lim *limiter
}

// Write waits for the peer's budget.
func (c *limitedConn) Write(p []byte) (int, error) {
	return (&limitedWriter{ctx: c.ctx, w: c.Conn, lim: c.lim}).Write(p)
}

// raftConn is a Raft-class connection: its writes go through the peer's
// budget ahead of content, and its reads mark the peer as heard from.
type raftConn struct {
	net.Conn
	t    *transport
	peer controlstore.ServerID
	lim  *limiter
	once sync.Once
}

// Read reads and records contact.
func (c *raftConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.t.heardFrom(c.peer)
	}
	return n, err
}

// Write waits for the peer's budget, Raft first.
func (c *raftConn) Write(p []byte) (int, error) {
	if err := c.lim.wait(c.t.ctx, len(p), true); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

// Close closes the connection once.
func (c *raftConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		if tc, ok := c.Conn.(*tls.Conn); ok {
			c.t.untrack(tc.NetConn())
		}
	})
	return err
}

// streamLayer is the raft.StreamLayer over the transport: Accept yields
// the authorized Raft-class connections, Dial opens one, and Addr is the
// advertised peer address.
type streamLayer struct {
	t    *transport
	done chan struct{}
	once sync.Once
}

// Compile-time check of the contract.
var _ hraft.StreamLayer = (*streamLayer)(nil)

// newStreamLayer returns the stream layer.
func newStreamLayer(t *transport) *streamLayer { return &streamLayer{t: t, done: make(chan struct{})} }

// Accept returns the next Raft-class connection.
func (s *streamLayer) Accept() (net.Conn, error) {
	select {
	case c := <-s.t.raftCh:
		return c, nil
	case <-s.done:
		return nil, errTransportClosed
	case <-s.t.ctx.Done():
		return nil, errTransportClosed
	}
}

// Close stops Accept; the transport owns the listener.
func (s *streamLayer) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

// Addr returns the advertised peer address.
func (s *streamLayer) Addr() net.Addr { return peerAddr(s.t.advertise) }

// Dial opens a Raft-class connection within timeout.
func (s *streamLayer) Dial(address hraft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(s.t.ctx, timeout)
	defer cancel()
	tc, id, err := s.t.dial(ctx, string(address), controlstore.ClassRaft)
	if err != nil {
		return nil, err
	}
	return &raftConn{Conn: tc, t: s.t, peer: id.ServerID, lim: s.t.lim.of(id.ServerID)}, nil
}

// peerAddr is an advertised address as a net.Addr.
type peerAddr string

// Network returns "tcp".
func (peerAddr) Network() string { return "tcp" }

// String returns the address.
func (a peerAddr) String() string { return string(a) }
