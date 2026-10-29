// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/discovery"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// IdleConnTimeout closes a pooled Upstream connection idle this long
// (docs/architecture/03-data-plane.md "Upstream layer").
const IdleConnTimeout = 90 * time.Second

// MaxResponseHeaderBytes caps an Upstream response's header block: the
// process header ceiling of client requests (listener.MaxHeaderBytes). A
// larger block fails the attempt as a protocol error, error kind reset.
const MaxResponseHeaderBytes = listener.MaxHeaderBytes

// ALPN protocols offered to TLS Upstreams: HTTP/2 by ALPN, else HTTP/1.1
// (ADR-0009, Upstream layer row).
const (
	alpnH2     = "h2"
	alpnHTTP11 = "http/1.1"
)

// errHandshakeTimeout is the cancel cause of a TLS handshake that ran past
// resilience.TLSHandshakeTimeout.
var errHandshakeTimeout = errors.New("TLS handshake timeout")

// dialError marks a failure to establish the TCP connection to an
// Endpoint: error kind connect (a dial error or dial timeout, the egress
// guard's refusal, an Endpoint without a resolved address).
type dialError struct{ err error }

func (e *dialError) Error() string { return "upstream dial: " + e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// handshakeError marks a failure during the TLS handshake: error kind tls
// (a handshake or verification failure, or its 2 s timeout). The message
// keeps the "tls: " prefix crypto/tls errors carry.
type handshakeError struct{ err error }

func (e *handshakeError) Error() string { return "tls: upstream handshake: " + e.err.Error() }
func (e *handshakeError) Unwrap() error { return e.err }

// tlsEntry is one TLS setting of an Upstream runtime, keyed by protocol,
// SNI and secret references (settings.poolKey). It holds the client
// configuration that follows rotations and is carried across Hot Reloads
// that keep the key; the live snapshots referencing it count in refs.
type tlsEntry struct {
	key string
	// cfg is nil for a cleartext Upstream.
	cfg *tlsconf.ClientConfig
	// maxConns is circuitBreaker.maxConnections when the entry was made:
	// the idle connection bound of its pools.
	maxConns int
	// refs is guarded by the runtime's mutex.
	refs int
}

// poolList is the immutable list of one Endpoint's pools, one per live TLS
// entry; it is replaced copy-on-write.
type poolList struct{ pools []*pool }

// find returns the pool of entry e.
func (l *poolList) find(e *tlsEntry) *pool {
	if l == nil {
		return nil
	}
	for _, p := range l.pools {
		if p.entry == e {
			return p
		}
	}
	return nil
}

// pool is the connection pool of one (TLS setting, Endpoint): an
// http.Transport whose dialer reaches only that Endpoint, through the
// egress guard, within the 1 s dial budget and 2 s TLS handshake.
type pool struct {
	rt    *runtime
	st    *epState
	entry *tlsEntry
	tr    *http.Transport
	// send sends an attempt: tr's RoundTrip (a test may stand in for it).
	send http.RoundTripper
	// retired is set when the Endpoint left the set or the TLS entry was
	// released: idle connections are closed after every attempt.
	retired atomic.Bool
}

// newPool builds the pool of entry e for Endpoint st. Cleartext pools
// speak HTTP/1.1; TLS pools offer h2 and http/1.1 by ALPN. HTTP/2
// connections ping after health.PingAfter of silence and close after
// health.PingTimeout unanswered, and an HTTP/2 Upstream's stream limit
// opens another connection (StrictMaxConcurrentRequests false); every
// attempt holds a bulkhead slot, so nothing queues unseen in the
// transport.
func newPool(rt *runtime, st *epState, e *tlsEntry) *pool {
	p := &pool{rt: rt, st: st, entry: e}
	var protos http.Protocols
	protos.SetHTTP1(true)
	tr := &http.Transport{
		DialContext:            p.dial,
		DisableCompression:     true,
		MaxIdleConnsPerHost:    max(e.maxConns, 1),
		IdleConnTimeout:        IdleConnTimeout,
		MaxResponseHeaderBytes: MaxResponseHeaderBytes,
		Protocols:              &protos,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout:             health.PingAfter,
			PingTimeout:                 health.PingTimeout,
			StrictMaxConcurrentRequests: false,
		},
	}
	if e.cfg != nil {
		protos.SetHTTP2(true)
		tr.DialTLSContext = p.dialTLS
	}
	p.tr, p.send = tr, tr
	return p
}

// retire marks the pool retired and closes its idle connections; in-flight
// attempts finish on their connections, which close when they go idle.
func (p *pool) retire() {
	p.retired.Store(true)
	p.tr.CloseIdleConnections()
}

// done is called when an attempt on the pool ended: a retired pool closes
// the connection it returned.
func (p *pool) done() {
	if p.retired.Load() {
		p.tr.CloseIdleConnections()
	}
}

// dial connects to the Endpoint's cached addresses in answer order within
// one dial budget (discovery.Dial), each through the egress guard. The
// transport dials with a context detached from the request's
// cancellation, so the budget alone bounds it.
func (p *pool) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	n := p.st.net.Load()
	if n == nil {
		return nil, &dialError{err: discovery.ErrNoAddresses}
	}
	c, err := discovery.Dial(ctx, p.rt.m.clk, p.rt.m.dial, network, n.addrs, resilience.DialTimeout)
	if err != nil {
		return nil, &dialError{err: err}
	}
	return p.rt.track(c), nil
}

// dialTLS dials like dial and runs the TLS handshake within
// resilience.TLSHandshakeTimeout with the TLS entry's current
// configuration: rotated values reach new connections only. The server
// name is the SNI when set, else the Endpoint host (an IP literal verifies
// IP SANs and sends no SNI).
func (p *pool) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	raw, err := p.dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	cfg := p.entry.cfg.ForHost(p.st.net.Load().host)
	cfg.NextProtos = []string{alpnH2, alpnHTTP11}
	tc := tls.Client(raw, cfg)
	hctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	t := p.rt.m.clk.AfterFunc(resilience.TLSHandshakeTimeout, func() { cancel(errHandshakeTimeout) })
	err = tc.HandshakeContext(hctx)
	t.Stop()
	if err != nil {
		_ = raw.Close()
		if errors.Is(context.Cause(hctx), errHandshakeTimeout) {
			err = fmt.Errorf("%w: %w", errHandshakeTimeout, err)
		}
		return nil, &handshakeError{err: err}
	}
	return tc, nil
}

// connClosed is the closed bit of trackedConn.state; the low bits count
// the attempts using the connection.
const connClosed = uint64(1) << 63

// trackedConn counts one Upstream connection in
// ruralz_upstream_pool_connections: idle while no attempt uses it, active
// while at least one does (HTTP/2 streams included).
type trackedConn struct {
	net.Conn
	rt    *runtime
	state atomic.Uint64
}

// track wraps a new connection, counted idle until an attempt gets it.
func (rt *runtime) track(c net.Conn) *trackedConn {
	tc := &trackedConn{Conn: c, rt: rt}
	rt.poolAdd(emit.PoolIdle, 1)
	return tc
}

// begin counts an attempt on the connection; false when it is closed.
func (c *trackedConn) begin() bool {
	for {
		s := c.state.Load()
		if s&connClosed != 0 {
			return false
		}
		if c.state.CompareAndSwap(s, s+1) {
			if s == 0 {
				c.rt.poolMove(emit.PoolIdle, emit.PoolActive)
			}
			return true
		}
	}
}

// end uncounts an attempt begun on the connection.
func (c *trackedConn) end() {
	for {
		s := c.state.Load()
		if s&^connClosed == 0 {
			return
		}
		if c.state.CompareAndSwap(s, s-1) {
			if s == 1 {
				c.rt.poolMove(emit.PoolActive, emit.PoolIdle)
			}
			return
		}
	}
}

// Close closes the connection and uncounts it.
func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	for {
		s := c.state.Load()
		if s&connClosed != 0 {
			return err
		}
		if c.state.CompareAndSwap(s, s|connClosed) {
			if s == 0 {
				c.rt.poolAdd(emit.PoolIdle, -1)
			} else {
				c.rt.poolAdd(emit.PoolActive, -1)
			}
			return err
		}
	}
}

// trackedOf returns the trackedConn under a connection the transport
// reports (httptrace.GotConnInfo.Conn): the connection itself for
// cleartext, the one under the *tls.Conn for TLS.
func trackedOf(c net.Conn) *trackedConn {
	switch x := c.(type) {
	case *trackedConn:
		return x
	case *tls.Conn:
		t, _ := x.NetConn().(*trackedConn)
		return t
	default:
		return nil
	}
}
