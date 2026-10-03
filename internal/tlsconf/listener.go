// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tlsconf

import (
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// ErrNoListenerState fails a handshake when the listener has no current
// TLS state (the State function returned nil).
var ErrNoListenerState = errors.New("tlsconf: listener has no TLS state")

// maxUnwrap bounds the NetConn chain walked to find a ClientCertRecorder.
const maxUnwrap = 8

// ListenerOptions describes one https listener in one snapshot.
type ListenerOptions struct {
	// MinVersion is listeners[].tls.minVersion: "" or "1.3" (default) or
	// "1.2" (spec 06 requirement 74).
	MinVersion string
	// Certs serves the listener's tls.certificates by SNI.
	Certs *CertIndex
	// RequestClientCert is true exactly when some Route bound to the
	// listener attaches auth.mtls in its effective chain (spec 06
	// requirement 46).
	RequestClientCert bool
}

// ListenerState is the immutable TLS state of one listener in one
// snapshot, with its per-state *tls.Config built once. The listener's
// Server configuration reads the current state per handshake, so a Hot
// Reload reaches new handshakes only.
type ListenerState struct {
	minVersion        uint16
	certs             *CertIndex
	requestClientCert bool
	cfg               *tls.Config
}

// NewListenerState validates o and builds the per-state configuration:
// the minimum version, the TLS12Suites, ALPN h2 and http/1.1, certificates
// from o.Certs and, with RequestClientCert, tls.RequestClientCert (an
// unverified client certificate; auth.mtls verifies it against its own
// anchors). With RequestClientCert, session tickets are off so every
// connection's handshake requests the certificate afresh: a resumed
// session would skip the request. An unknown MinVersion or a nil Certs is
// RZ-CFG-005.
func NewListenerState(o ListenerOptions) (*ListenerState, error) {
	minVersion, err := ParseMinVersion(o.MinVersion)
	if err != nil {
		return nil, err
	}
	if o.Certs == nil {
		return nil, errcode.Errorf(codeSchema, "an https listener needs a certificate index")
	}
	cfg := &tls.Config{
		MinVersion:     minVersion,
		MaxVersion:     tls.VersionTLS13,
		CipherSuites:   TLS12Suites(),
		NextProtos:     NextProtos(),
		GetCertificate: o.Certs.GetCertificate,
		ClientAuth:     tls.NoClientCert,
	}
	if o.RequestClientCert {
		cfg.ClientAuth = tls.RequestClientCert
		cfg.SessionTicketsDisabled = true
	}
	return &ListenerState{
		minVersion:        minVersion,
		certs:             o.Certs,
		requestClientCert: o.RequestClientCert,
		cfg:               cfg,
	}, nil
}

// MinVersion returns the minimum TLS version.
func (s *ListenerState) MinVersion() uint16 { return s.minVersion }

// RequestClientCert reports whether handshakes request a client
// certificate.
func (s *ListenerState) RequestClientCert() bool { return s.requestClientCert }

// Certs returns the certificate index.
func (s *ListenerState) Certs() *CertIndex { return s.certs }

// Config returns the per-state configuration returned to handshakes. It is
// shared: callers must not modify it.
func (s *ListenerState) Config() *tls.Config { return s.cfg }

// ClientCertRecorder is implemented by the listener's connection wrapper:
// the Server configuration records on it whether the handshake requested a
// client certificate, so an auth.mtls Route reached over a connection that
// requested none gets 421 RZ-AUTH-008 and one that requested one but got
// none gets 401 RZ-AUTH-001 (spec 06 requirement 47).
type ClientCertRecorder interface {
	// SetClientCertRequested records the handshake's request mode.
	SetClientCertRequested(requested bool)
}

// ClientCertRequest is an embeddable ClientCertRecorder.
type ClientCertRequest struct{ requested atomic.Bool }

// SetClientCertRequested implements ClientCertRecorder.
func (r *ClientCertRequest) SetClientCertRequested(requested bool) { r.requested.Store(requested) }

// ClientCertRequested reports the recorded mode.
func (r *ClientCertRequest) ClientCertRequested() bool { return r.requested.Load() }

// ServerOptions configures the listener Server configuration.
type ServerOptions struct {
	// State returns the listener's state from the currently published
	// snapshot; it is called once per handshake and must not block.
	State func() *ListenerState
}

// Server returns the static configuration of an https listener: every
// handshake reads o.State() in GetConfigForClient, records the request
// mode on the connection (ClientCertRecorder, found on the connection or
// through its NetConn chain) and continues with the state's configuration.
// Its own fields (TLS 1.2 minimum, TLS12Suites, ALPN) satisfy
// http.Server.ServeTLS and the HTTP/2 cipher checks; the state decides the
// negotiated version.
//
// The configuration does not observe handshakes. crypto/tls has no hook
// that runs after a successful handshake: VerifyConnection runs before the
// client's last flight is read (in TLS 1.3 right after the server's own
// flight), so it also fires for handshakes the client then rejects. The
// listener measures spec 06 requirement 77 around its own completed
// tls.Conn.HandshakeContext call instead: a nil error observes
// ruralz_listener_tls_handshake_duration_seconds from accept, an error
// counts ruralz_listener_connections_total{result="tls_failure"}.
func Server(o ServerOptions) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS13,
		CipherSuites: TLS12Suites(),
		NextProtos:   NextProtos(),
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			var st *ListenerState
			if o.State != nil {
				st = o.State()
			}
			if st == nil {
				return nil, ErrNoListenerState
			}
			recordClientCertMode(hello.Conn, st.requestClientCert)
			return st.cfg, nil
		},
	}
}

// recordClientCertMode finds the ClientCertRecorder on c or its NetConn
// chain and records requested.
func recordClientCertMode(c net.Conn, requested bool) {
	for range maxUnwrap {
		if c == nil {
			return
		}
		if r, ok := c.(ClientCertRecorder); ok {
			r.SetClientCertRequested(requested)
			return
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return
		}
		c = u.NetConn()
	}
}
