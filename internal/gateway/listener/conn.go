// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clientaddr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// h2cPreface is the client connection preface that selects h2c by prior
// knowledge on an http listener (RFC 9113 section 3.4).
const h2cPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// notCounted marks a connection whose outcome is not counted yet.
const notCounted = -1

// Conn is one client connection as its listener accepted it, below TLS:
// what the request handler needs from the connection, and its share of
// the listener metrics. Handlers get it with ConnFrom. tlsconf records on
// it, through the embedded ClientCertRequest, whether the handshake
// requested a client certificate (auth.mtls answers 421 RZ-AUTH-008 on a
// connection that requested none).
type Conn struct {
	net.Conn
	tlsconf.ClientCertRequest

	listener string
	metrics  *emit.ListenerMetrics
	stripe   emit.Stripe
	start    time.Time
	tls      bool
	proxy    bool
	cache    filter.ConnCache
	// requests numbers the requests the connection carried (NextRequest).
	requests atomic.Uint32
	// timedOut holds the error of the write that timed out, which every
	// later Write returns.
	timedOut atomic.Pointer[error]

	// sniffing and sniffed track the h2c preface; only Read, which
	// net/http never calls concurrently, touches them.
	sniffing bool
	sniffed  int

	mu      sync.Mutex
	proto   int // the ruralz_listener_open_connections slot holding it
	outcome int // the ruralz_listener_connections_total result, or notCounted
	closed  bool
}

// newConn records c, accepted at start, as open under http1.
func newConn(c net.Conn, listener string, m *emit.ListenerMetrics, stripe emit.Stripe, start time.Time, isTLS, proxy bool) *Conn {
	rec := &Conn{
		Conn:     c,
		listener: listener,
		metrics:  m,
		stripe:   stripe,
		start:    start,
		tls:      isTLS,
		proxy:    proxy,
		sniffing: !isTLS,
		proto:    emit.ProtoHTTP1,
		outcome:  notCounted,
	}
	m.OpenConns[emit.ProtoHTTP1].Add(stripe, 1)
	return rec
}

// Listener returns the name of the listener that accepted the connection.
func (c *Conn) Listener() string { return c.listener }

// Stripe returns the connection's stripe, taken once at accept
// (retire.Holder.ConnStripe); the handler uses it for its pins and metric
// handles.
func (c *Conn) Stripe() emit.Stripe { return c.stripe }

// Peer returns the connection peer for the client-address rule: the PROXY
// v2 source on a proxyProtocol listener, else the TCP peer
// (clientaddr.PeerOf).
func (c *Conn) Peer() netip.AddrPort { return clientaddr.PeerOf(c) }

// Cache returns the connection's memo (filter.ConnTLS.Cache), safe for
// concurrent HTTP/2 streams.
func (c *Conn) Cache() *filter.ConnCache { return &c.cache }

// Metrics returns the handles of the listener that accepted the
// connection, from the snapshot published at accept: the request handler
// records a request it answers before pinning a snapshot, such as 503
// RZ-RT-005, on them.
func (c *Conn) Metrics() *emit.ListenerMetrics { return c.metrics }

// NextRequest returns the connection's request sequence number, 0 for its
// first request: the per-connection offset of the request's stripe
// (retire.Holder.RequestStripe), so concurrent HTTP/2 streams of one
// connection spread over the stripes. Safe for concurrent streams.
func (c *Conn) NextRequest() uint32 { return c.requests.Add(1) - 1 }

// NetConn returns the connection below the record.
func (c *Conn) NetConn() net.Conn { return c.Conn }

// Read reads from the connection; on an http listener it watches the first
// bytes for the h2c preface, and on a proxyProtocol listener it counts a
// refused PROXY v2 header.
func (c *Conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.sniffing && n > 0 {
		c.sniff(b[:n])
	}
	if err != nil && c.proxy {
		c.checkRefused(err)
	}
	return n, err
}

// Write writes to the connection; on a proxyProtocol listener a write
// before the header also reads it, so a refusal can surface here. Once a
// write has timed out, every later write fails at once with its error
// (docs/architecture/03-data-plane.md "Bounded resources"): the stream
// stops at an unknown point, and the close_notify that the Close of a TLS
// connection writes after net/http's failed response write would wait
// its own 5 s deadline on a peer that reads nothing.
func (c *Conn) Write(b []byte) (int, error) {
	if err := c.timedOut.Load(); err != nil {
		return 0, *err
	}
	n, err := c.Conn.Write(b)
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			c.timedOut.CompareAndSwap(nil, &err)
		}
		if c.proxy {
			c.checkRefused(err)
		}
	}
	return n, err
}

// CloseWrite half-closes the connection when the inner one supports it;
// net/http uses it to finish an HTTP/1.1 response before closing.
func (c *Conn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

// Close closes the connection once: it leaves the open connections, and a
// cleartext connection not counted yet counts as accepted http1.
func (c *Conn) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.metrics.OpenConns[c.proto].Add(c.stripe, -1)
		if c.outcome == notCounted && !c.tls {
			c.outcome = emit.ConnAccepted
			c.metrics.Conns[emit.ProtoHTTP1][emit.ConnAccepted].Add(c.stripe, 1)
		}
	}
	c.mu.Unlock()
	return c.Conn.Close()
}

// sniff matches b against the rest of the h2c preface: a mismatch makes
// the connection http1, the whole preface http2.
func (c *Conn) sniff(b []byte) {
	for _, x := range b {
		if x != h2cPreface[c.sniffed] {
			c.sniffing = false
			c.count(emit.ConnAccepted, emit.ProtoHTTP1)
			return
		}
		c.sniffed++
		if c.sniffed == len(h2cPreface) {
			c.sniffing = false
			c.count(emit.ConnAccepted, emit.ProtoHTTP2)
			return
		}
	}
}

// checkRefused counts a refused PROXY v2 header: err wraps
// clientaddr.ErrMalformed, and the connection was not closed locally
// first (clientaddr.ListenerOptions.OnRefused applies the same rule).
func (c *Conn) checkRefused(err error) {
	if errors.Is(err, clientaddr.ErrMalformed) && !errors.Is(err, net.ErrClosed) {
		c.count(emit.ConnRefused, emit.ProtoHTTP1)
	}
}

// handshakeDone counts the end of the TLS handshake: accepted with the
// protocol ALPN chose and its duration from accept, or tls_failure. A
// handshake that ended because the connection was closed locally counts
// nothing.
func (c *Conn) handshakeDone(err error, alpn string, took time.Duration) {
	switch {
	case err == nil:
		proto := emit.ProtoHTTP1
		if alpn == tlsconf.ProtoH2 {
			proto = emit.ProtoHTTP2
		}
		c.metrics.TLSHandshake.Record(c.stripe, uint64(max(took, 0)))
		c.count(emit.ConnAccepted, proto)
	case errors.Is(err, net.ErrClosed):
	default:
		c.count(emit.ConnTLSFailure, emit.ProtoHTTP1)
	}
}

// count records the connection's one outcome, result under proto, and
// moves an open connection to the proto slot of the open connections.
func (c *Conn) count(result, proto int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outcome != notCounted {
		return
	}
	c.outcome = result
	c.metrics.Conns[proto][result].Add(c.stripe, 1)
	if proto != c.proto && !c.closed {
		c.metrics.OpenConns[c.proto].Add(c.stripe, -1)
		c.metrics.OpenConns[proto].Add(c.stripe, 1)
		c.proto = proto
	}
}

// connKey keys the Conn in a request context.
type connKey struct{}

// ConnFrom returns the connection a request arrived on, from its context
// (http.Request.Context), or nil for a request a Set did not serve.
func ConnFrom(ctx context.Context) *Conn {
	c, _ := ctx.Value(connKey{}).(*Conn)
	return c
}

// connContext is the Servers' ConnContext: it puts the connection's
// record in the context every request on it derives from.
func connContext(ctx context.Context, c net.Conn) context.Context {
	if rec := recordOf(c); rec != nil {
		return context.WithValue(ctx, connKey{}, rec)
	}
	return ctx
}

// recordOf returns the record below c, a record or a TLS connection over
// one.
func recordOf(c net.Conn) *Conn {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	rec, _ := c.(*Conn)
	return rec
}
