// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/tlsconf"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Server limits of every client listener, set once at start: target
// values of docs/architecture/03-data-plane.md "Bounded resources"
// (configurability: OQ-data-plane-1).
const (
	// ReadHeaderTimeout bounds reading a request's header block; net/http
	// applies it to the TLS handshake too (tlsconf.HandshakeTimeout).
	ReadHeaderTimeout = tlsconf.HandshakeTimeout
	// IdleTimeout closes a keep-alive connection that sends no request.
	IdleTimeout = 120 * time.Second
	// MaxHeaderBytes is the process header-block ceiling: a larger block
	// gets net/http's plain 431 before any handler.
	MaxHeaderBytes = 256 << 10
	// MaxConcurrentStreams is HTTP2Config.MaxConcurrentStreams.
	MaxConcurrentStreams = 250
	// MaxReceiveBufferPerConnection is the HTTP/2 unread request data per
	// connection.
	MaxReceiveBufferPerConnection = 256 << 10
	// MaxReceiveBufferPerStream is the HTTP/2 unread request data per
	// stream.
	MaxReceiveBufferPerStream = 64 << 10
	// MaxReadFrameSize is the HTTP/2 frame read buffer, the protocol
	// minimum.
	MaxReadFrameSize = 16 << 10
)

// Shards is K, the number of http.Server shards that serve each client
// listener, sharing the handler (target): Server.Shutdown sends all of a
// Server's GOAWAYs at once, so the Drain shuts the shards down one at a
// time across its jitter window and clients reconnect in K waves
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "Drain
// timeline defaults").
const Shards = 8

// newHTTPServer returns the Server of a listener of protocol p: HTTP/1.1
// and HTTP/2 over TLS for https, HTTP/1.1 and h2c by prior knowledge for
// http; the Bounded resources limits; ReadTimeout and WriteTimeout unset,
// since the handler sets body and write deadlines through
// http.ResponseController; and DisableGeneralOptionsHandler, so net/http
// hands "OPTIONS *" to the handler, which answers it 404 RZ-RT-001 like
// any request that matches no Route, instead of answering 200 itself
// (docs/architecture/07-multi-protocol.md "HTTP intermediary behavior").
func newHTTPServer(p v1alpha1.ListenerProtocol, h http.Handler, errorLog slog.Handler) *http.Server {
	var protos http.Protocols
	protos.SetHTTP1(true)
	if p == v1alpha1.ListenerProtocolHTTPS {
		protos.SetHTTP2(true)
	} else {
		protos.SetUnencryptedHTTP2(true)
	}
	return &http.Server{
		Handler:                      h,
		DisableGeneralOptionsHandler: true,
		ReadHeaderTimeout:            ReadHeaderTimeout,
		IdleTimeout:                  IdleTimeout,
		MaxHeaderBytes:               MaxHeaderBytes,
		Protocols:                    &protos,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams:          MaxConcurrentStreams,
			MaxReceiveBufferPerConnection: MaxReceiveBufferPerConnection,
			MaxReceiveBufferPerStream:     MaxReceiveBufferPerStream,
			MaxReadFrameSize:              MaxReadFrameSize,
		},
		ErrorLog:    slog.NewLogLogger(errorLog, slog.LevelWarn),
		ConnContext: connContext,
	}
}

// shardGroup is the Shards servers an endpoint feeds at one time. A
// changed client-certificate request mode replaces the whole group; a
// group is never changed once serving.
type shardGroup struct {
	shards [Shards]*server
}

// offer hands nc to the shard at position first (modulo Shards) or, when
// that shard has stopped taking connections, to the next shard still
// taking them; false means every shard of the group stopped first.
func (g *shardGroup) offer(nc net.Conn, first uint64) bool {
	for k := range uint64(Shards) {
		sv := g.shards[(first+k)%Shards]
		select {
		case sv.ln.conns <- nc:
			return true
		case <-sv.ln.done:
		}
	}
	return false
}

// server is one Server shard of an endpoint and the channel-fed listener
// through which the endpoint's dispatching listener hands it connections.
// A Server is never changed once serving; a new group replaces it.
type server struct {
	ep    *endpoint
	shard int // its position in the group, from 0 to Shards-1
	http  *http.Server
	ln    *shardListener
	// The fields below are guarded by Set.mu.
	shutting bool
	cancel   context.CancelFunc
	// done is closed when the Server stopped after shutting down.
	done chan struct{}
}

// shardListener is the channel-fed listener of one Server shard: it hands
// the Server the connections the dispatching listener gives it.
type shardListener struct {
	ep    *endpoint
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newShardListener(ep *endpoint) *shardListener {
	return &shardListener{ep: ep, conns: make(chan net.Conn), done: make(chan struct{})}
}

// Accept returns the next connection the endpoint hands over, starting
// the handshake observer of a TLS connection; after Close it returns
// net.ErrClosed.
func (l *shardListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, l.closedErr()
	default:
	}
	select {
	case c := <-l.conns:
		l.ep.observe(c)
		return c, nil
	case <-l.done:
		return nil, l.closedErr()
	}
}

func (l *shardListener) closedErr() error {
	return &net.OpError{Op: "accept", Net: "tcp", Addr: l.ep.local, Err: net.ErrClosed}
}

// Close stops handing connections over; the socket stays open.
func (l *shardListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

// Addr returns the socket's address.
func (l *shardListener) Addr() net.Addr { return l.ep.local }
