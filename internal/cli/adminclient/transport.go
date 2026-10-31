// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/ravindu-rev/ruralz/internal/adminauth"
)

// errConnectTimeout is the cause a connection attempt is canceled with
// when ConnectTimeout passes.
var errConnectTimeout = errors.New("connect deadline expired")

// newTransport returns the client's transport. Proxy is nil: connections
// are direct and proxy variables are ignored, so the loopback rule tests
// the real peer and no proxy sees a token. HTTP/1.1 only, without
// keep-alives: every call dials a fresh connection, and net/http retries a
// request only on a reused one, so no call is retried. No compression is
// requested, so bodies are bounded and kept exactly as the Node sent them.
// Both dialers run under the connection deadline.
func (c *Client) newTransport() *http.Transport {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	return &http.Transport{
		Proxy:              nil,
		DialContext:        c.dialPlain,
		DialTLSContext:     c.dialTLS,
		DisableKeepAlives:  true,
		DisableCompression: true,
		Protocols:          &protocols,
	}
}

// dialPlain connects to an http Node.
func (c *Client) dialPlain(ctx context.Context, network, addr string) (net.Conn, error) {
	return c.connect(ctx, network, addr, false)
}

// dialTLS connects to an https Node and finishes the TLS handshake.
func (c *Client) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	return c.connect(ctx, network, addr, true)
}

// connect opens a connection to addr, with TLS when secure, within
// ConnectTimeout on the client's clock. ctx is the transport's dial
// context, which the transport cancels once no call waits for the dial.
func (c *Client) connect(ctx context.Context, network, addr string, secure bool) (net.Conn, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := c.clock.AfterFunc(ConnectTimeout, func() { cancel(errConnectTimeout) })
	defer timer.Stop()
	conn, err := c.open(ctx, network, addr, secure)
	if err != nil {
		if errors.Is(context.Cause(ctx), errConnectTimeout) {
			return nil, &DeadlineError{Kind: DeadlineConnect, Op: "connect to " + addr, Limit: ConnectTimeout, TLS: secure}
		}
		return nil, err
	}
	return conn, nil
}

// open dials addr and, when secure, finishes TLS under ctx. Over http
// with a token it applies the cleartext rule to the connected peer before
// anything is written.
func (c *Client) open(ctx context.Context, network, addr string, secure bool) (net.Conn, error) {
	conn, err := c.dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if !secure {
		if c.tokenFile != "" {
			if err := c.cleartextPeer(conn); err != nil {
				_ = conn.Close()
				return nil, err
			}
		}
		return conn, nil
	}
	tc := tls.Client(conn, c.tls)
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tc, nil
}

// cleartextPeer applies the rule of docs/reference/01-cli-and-api-surface.md
// "Admin API": a token never goes in cleartext beyond loopback, so over
// http the connected peer must be a loopback address by the Node's rule
// (127.0.0.0/8, ::1 or an IPv4-mapped loopback address), which a name
// such as localhost resolving to loopback satisfies.
func (c *Client) cleartextPeer(conn net.Conn) error {
	ra := conn.RemoteAddr()
	if ra == nil {
		return fmt.Errorf("%w: the connection has no peer address", ErrCleartext)
	}
	if peer := ra.String(); !adminauth.Loopback(peer) {
		return fmt.Errorf("%w to %s, which is not a loopback address; use https, or drop %s",
			ErrCleartext, peer, c.names.TokenFile)
	}
	return nil
}
