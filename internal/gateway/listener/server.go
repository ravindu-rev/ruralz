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

// newHTTPServer returns the Server of a listener of protocol p: HTTP/1.1
// and HTTP/2 over TLS for https, HTTP/1.1 and h2c by prior knowledge for
// http; the Bounded resources limits; ReadTimeout and WriteTimeout unset,
// since the handler sets body and write deadlines through
// http.ResponseController.
func newHTTPServer(p v1alpha1.ListenerProtocol, h http.Handler, errorLog slog.Handler) *http.Server {
	var protos http.Protocols
	protos.SetHTTP1(true)
	if p == v1alpha1.ListenerProtocolHTTPS {
		protos.SetHTTP2(true)
	} else {
		protos.SetUnencryptedHTTP2(true)
	}
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: ReadHeaderTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
		Protocols:         &protos,
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

// server is one Server of an endpoint and the listener feeding it. A
// Server is never changed once serving; a new one replaces it.
type server struct {
	ep   *endpoint
	http *http.Server
	ln   *serverListener
	// The fields below are guarded by Set.mu.
	shutting bool
	cancel   context.CancelFunc
	// done is closed when the Server stopped after shutting down.
	done chan struct{}
}

// serverListener hands one Server the connections its endpoint accepted.
type serverListener struct {
	ep    *endpoint
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newServerListener(ep *endpoint) *serverListener {
	return &serverListener{ep: ep, conns: make(chan net.Conn), done: make(chan struct{})}
}

// Accept returns the next connection the endpoint hands over, starting
// the handshake observer of a TLS connection; after Close it returns
// net.ErrClosed.
func (l *serverListener) Accept() (net.Conn, error) {
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

func (l *serverListener) closedErr() error {
	return &net.OpError{Op: "accept", Net: "tcp", Addr: l.ep.local, Err: net.ErrClosed}
}

// Close stops handing connections over; the socket stays open.
func (l *serverListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

// Addr returns the socket's address.
func (l *serverListener) Addr() net.Addr { return l.ep.local }
