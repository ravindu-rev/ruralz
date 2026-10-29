// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/adminauth"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/readiness"
	"github.com/ravindu-rev/ruralz/internal/gateway/tap"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// DefaultPort is the admin port before any Revision names one
// (Gateway.spec.admin.port's default).
const DefaultPort = 9901

// http.Server bounds (target values of docs/architecture/03-data-plane.md
// "Bounded resources").
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 256 << 10
	// HTTP/2 over TLS.
	maxConcurrentStreams          = 250
	maxReceiveBufferPerConnection = 256 << 10
	maxReceiveBufferPerStream     = 64 << 10
	maxReadFrameSize              = 16 << 10
)

// Configuration errors; New refuses to build a server on any of them.
var (
	// ErrNoAuthenticator: Config.Auth is nil.
	ErrNoAuthenticator = errors.New("admin: no authenticator")
	// ErrNoAudit: the authenticator has no logger, so /tap and
	// /config/dump uses would go unlogged.
	ErrNoAudit = errors.New("admin: the authenticator does not log /tap and /config/dump uses")
	// ErrNoReadiness: Config.Readiness is nil.
	ErrNoReadiness = errors.New("admin: no readiness tracker")
	// ErrNoRequestID: Config.RequestID is nil, so problem documents would
	// carry no requestId.
	ErrNoRequestID = errors.New("admin: no requestId function")
	// ErrTLSMismatch: Config.TLS is set without RURALZ_ADMIN_TLS_DIR, or
	// missing with it.
	ErrTLSMismatch = errors.New("admin: TLS configuration does not match " + adminauth.EnvTLSDir)
)

// Config holds the dependencies of a Server.
type Config struct {
	// Auth authenticates every request; required, with its Logger set.
	Auth *adminauth.Authenticator
	// TLS is the admin TLS configuration, tlsconf.Admin of
	// RURALZ_ADMIN_TLS_DIR. It is set exactly when Auth's settings name a
	// TLS directory; nil serves cleartext.
	TLS *tls.Config
	// Readiness answers /readyz; required.
	Readiness *readiness.Tracker
	// Tap serves /tap; nil leaves /tap unserved.
	Tap *tap.Hub
	// Metrics serves /metrics (telemetry.Runtime.MetricsHandler).
	Metrics http.Handler
	// ConfigDump serves /config/dump.
	ConfigDump http.Handler
	// Upstreams serves /debug/upstreams.
	Upstreams http.Handler
	// Snapshots returns the /debug/snapshots body: retire.Holder.View with
	// the loader's pending digest and the Last-Known-Good digest.
	Snapshots func() adminapi.Snapshots
	// RequestID returns the requestId of the server's problem documents:
	// RequestID(tracer), the function the Authenticator and the Hub take
	// too; required.
	RequestID func(*http.Request) string
	// Status reports the admin hop of ruralz_security_cleartext_hops; nil
	// reports nothing.
	Status emit.NodeStatus
	// ErrorLog receives net/http's own errors
	// (telemetry.Runtime.ErrorLog); nil discards them.
	ErrorLog slog.Handler
}

// Server is the admin server. It is safe for concurrent use.
type Server struct {
	srv       *http.Server
	handler   http.Handler
	limiter   *admission.ConnLimiter
	tls       bool
	status    emit.NodeStatus
	requestID func(*http.Request) string
	// endpoints maps each served exact path to its handler.
	endpoints map[string]http.Handler
	// mutex and block are the profiling windows of /debug/pprof/.
	mutex, block *profileWindow
	// stop cancels the base context of every request.
	stop context.CancelFunc
}

// New builds the server and reports the admin hop: cleartext without
// Config.TLS. It starts nothing; Serve does.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Auth == nil:
		return nil, ErrNoAuthenticator
	case !cfg.Auth.AuditEnabled():
		return nil, ErrNoAudit
	case cfg.Readiness == nil:
		return nil, ErrNoReadiness
	case cfg.RequestID == nil:
		return nil, ErrNoRequestID
	case (cfg.TLS != nil) != cfg.Auth.Settings().TLS():
		return nil, ErrTLSMismatch
	}
	base, stop := context.WithCancel(context.Background())
	s := &Server{
		limiter:   admission.NewConnLimiter(admission.AdminConnections),
		tls:       cfg.TLS != nil,
		status:    cfg.Status,
		requestID: cfg.RequestID,
		mutex:     newProfileWindow(setMutexProfileFraction, MutexProfileFraction),
		block:     newProfileWindow(setBlockProfileRate, BlockProfileRate),
		stop:      stop,
	}
	s.endpoints = s.buildEndpoints(cfg)
	s.handler = cfg.Auth.Middleware(http.HandlerFunc(s.route))
	errorLog := cfg.ErrorLog
	if errorLog == nil {
		errorLog = slog.DiscardHandler
	}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	s.srv = &http.Server{
		Handler:           s.handler,
		TLSConfig:         cfg.TLS,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		Protocols:         &protocols,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams:          maxConcurrentStreams,
			MaxReceiveBufferPerConnection: maxReceiveBufferPerConnection,
			MaxReceiveBufferPerStream:     maxReceiveBufferPerStream,
			MaxReadFrameSize:              maxReadFrameSize,
		},
		// "OPTIONS *" reaches the handler, so it is authenticated and
		// answered like every other unknown path.
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     slog.NewLogLogger(errorLog, slog.LevelWarn),
		BaseContext:                  func(net.Listener) context.Context { return base },
	}
	s.reportHop(!s.tls)
	return s, nil
}

// Handler returns the server's handler: authentication, then routing.
func (s *Server) Handler() http.Handler { return s.handler }

// Serve accepts admin connections on ln, the raw TCP listener. It returns
// an error matching http.ErrServerClosed once Shutdown runs, or the accept
// error when ln fails or is closed. ln's connections count against the
// admin ceiling shared by every listener the server serves, and TLS wraps
// them when configured. Serve may run on several listeners at once, as
// when a Hot Reload moves the port: the wiring serves the new listener,
// then closes the old one.
func (s *Server) Serve(ln net.Listener) error {
	lim := s.limiter.Wrap(ln)
	var err error
	if s.tls {
		// The certificate is in TLSConfig; ServeTLS adds ALPN h2.
		err = s.srv.ServeTLS(lim, "", "")
	} else {
		err = s.srv.Serve(lim)
	}
	return fmt.Errorf("admin: serve %s: %w", ln.Addr(), err)
}

// DisableKeepAlives closes idle admin connections and answers every later
// HTTP/1.1 request with Connection: close. During a handover's linger it
// moves keep-alive health checkers to the new process, so no balancer
// sees the Drain's 503 (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "In-place handover", step 6).
func (s *Server) DisableKeepAlives() { s.srv.SetKeepAlivesEnabled(false) }

// Shutdown ends every /tap stream and profile window at once, stops
// accepting, waits within ctx for the remaining requests and closes the
// connections still open when ctx ends. The admin hop stops counting.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stop()
	err := s.srv.Shutdown(ctx)
	if err != nil {
		err = errors.Join(err, s.srv.Close())
	}
	s.reportHop(false)
	if err != nil {
		return fmt.Errorf("admin: shutdown: %w", err)
	}
	return nil
}

// reportHop sets the admin cleartext hop count.
func (s *Server) reportHop(cleartext bool) {
	if s.status == nil {
		return
	}
	n := 0
	if cleartext {
		n = 1
	}
	s.status.SetCleartextHops(catalog.HopAdmin, n)
}
