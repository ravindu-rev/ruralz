// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package otlpsink is the in-process OTLP/gRPC collector of the test kit
// (11 section 3, F 32, req 39; 09 tests 33, 34 and 36 to 38): a gRPC
// server implementing the OTLP trace, metrics and logs collector services
// over TLS or h2c (cleartext HTTP/2), which records every export request
// for assertions.
//
// It records, per request, the decoded message, the wire bytes as the
// client sent them (after gRPC decompression, for canary scans), the
// incoming metadata (headers), the peer address, the connection's
// sequence number and the TLS state; Stats counts connections opened and
// open now, so a test sees an exporter close its old connection (09 test
// 40). Spans,
// log records and metrics are also kept flattened with their resource and
// scope, and the span queries order a trace by start time (ties in
// arrival order): Trace, SpanNames, Children, Roots and FormatTree answer
// "which spans, in which order, under which parent" (09 test 34, 11 req
// 34).
//
// Modes script collector failures for exporter tests (09 tests 37, 38):
// Accept records and answers OK; Reject answers every export with a gRPC
// status; Stall holds every export until the mode changes, the client's
// deadline passes or the sink closes; Down stops the server (dials are
// refused) and leaving Down listens on the same address again.
//
// The package is self-contained on go.opentelemetry.io/proto/otlp and
// google.golang.org/grpc and imports nothing from internal/telemetry
// (R-53), so the telemetry runtime tests against it. Every goroutine
// belongs to the Sink (one gRPC server with its serve loop, and the
// handlers gRPC starts per export); Close, or the end of the Start
// context, stops the server and waits for the serve loop and every
// handler. Recording is bounded by MaxRequests and MaxBytes.
package otlpsink

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Defaults.
const (
	// DefaultMaxRecvMsgSize bounds one export request (the OTLP exporters
	// batch at most 512 spans; 16 MiB leaves ample room).
	DefaultMaxRecvMsgSize = 16 << 20
	// DefaultMaxRequests bounds the recorded export requests.
	DefaultMaxRequests = 100_000
	// DefaultMaxBytes bounds the recorded wire bytes.
	DefaultMaxBytes = 256 << 20
)

// ErrClosed is returned by operations on a closed Sink.
var ErrClosed = errors.New("otlpsink: closed")

// Signal is an OTLP signal.
type Signal int

// Signals.
const (
	// Traces is the trace signal (TraceService/Export).
	Traces Signal = iota + 1
	// Metrics is the metrics signal (MetricsService/Export).
	Metrics
	// Logs is the logs signal (LogsService/Export).
	Logs
)

// String returns "traces", "metrics" or "logs".
func (s Signal) String() string {
	switch s {
	case Traces:
		return "traces"
	case Metrics:
		return "metrics"
	case Logs:
		return "logs"
	}
	return fmt.Sprintf("Signal(%d)", int(s))
}

// Mode is the sink's answer to export requests: Accept, Reject, Stall or
// Down.
type Mode interface{ mode() }

// Accept records every export and answers OK.
type Accept struct{}

// Reject answers every export with the gRPC status Code (not OK) and
// Message, recording nothing. codes.Unavailable is retryable for the
// OTLP exporters; codes.InvalidArgument is not.
type Reject struct {
	Code    codes.Code
	Message string
}

// Stall holds every export until the mode changes (the held exports then
// get the new mode's answer), the client's deadline passes or the sink
// closes.
type Stall struct{}

// Down stops the gRPC server: the listener closes, so dials are refused,
// and live connections and in-flight exports end. Leaving Down listens on
// the same address again.
type Down struct{}

func (Accept) mode() {}
func (Reject) mode() {}
func (Stall) mode()  {}
func (Down) mode()   {}

// String names the mode.
func (Accept) String() string { return "accept" }

// String names the mode and its status code.
func (r Reject) String() string { return "reject(" + r.Code.String() + ")" }

// String names the mode.
func (Stall) String() string { return "stall" }

// String names the mode.
func (Down) String() string { return "down" }

func validMode(m Mode) error {
	switch m := m.(type) {
	case Accept, Stall, Down:
		return nil
	case Reject:
		if m.Code == codes.OK {
			return errors.New("otlpsink: Reject needs a code other than OK")
		}
		return nil
	case nil:
		return errors.New("otlpsink: nil mode")
	}
	return fmt.Errorf("otlpsink: unknown mode %T", m)
}

// Config configures a Sink.
type Config struct {
	// Listen is the listen address; default "127.0.0.1:0".
	Listen string
	// TLS, when set, serves gRPC over TLS with this configuration (from
	// testkit/pki; set ClientAuth and ClientCAs for mutual TLS). When nil
	// the sink serves h2c, gRPC over cleartext HTTP/2.
	TLS *tls.Config
	// MaxRecvMsgSize bounds one export request; default
	// DefaultMaxRecvMsgSize.
	MaxRecvMsgSize int
	// MaxRequests bounds the recorded export requests; default
	// DefaultMaxRequests. Exports past the bound are answered OK and
	// counted in Stats.Dropped, not recorded.
	MaxRequests int
	// MaxBytes bounds the recorded wire bytes; default DefaultMaxBytes.
	// Exports past the bound are answered OK and counted in
	// Stats.Dropped.
	MaxBytes int64
	// Mode is the initial mode; default Accept.
	Mode Mode
}

// Stats counts export requests.
type Stats struct {
	// TraceRequests, MetricRequests and LogRequests count exports answered
	// OK per signal, recorded or dropped.
	TraceRequests, MetricRequests, LogRequests int64
	// Rejected counts exports answered with an error (Reject, a Stall the
	// client abandoned, a Down or Close that ended the export).
	Rejected int64
	// Dropped counts exports answered OK but not recorded (MaxRequests or
	// MaxBytes reached).
	Dropped int64
	// Stalled is the number of exports Stall holds now.
	Stalled int64
	// Connections counts the client connections opened (HTTP/2
	// transports up, after the TLS handshake); OpenConnections is the
	// number open now. Down and Close end every connection before they
	// return.
	Connections, OpenConnections int64
}

// Sink is a running in-process OTLP/gRPC collector.
type Sink struct {
	cfg    Config
	addr   string
	codec  rawCodec
	conns  *connStats // shared by the servers across Down
	rec    *recorder
	closed chan struct{} // closed by Close

	// modeMu serializes SetMode and Close, which start and stop servers;
	// it is never taken by request handlers.
	modeMu sync.Mutex

	mu        sync.Mutex // guards the fields below
	srv       *grpc.Server
	mode      Mode
	release   chan struct{} // closed when the mode leaves Stall
	isClosed  bool
	stopAfter func() bool

	wg        sync.WaitGroup // serve loops
	closeOnce sync.Once

	rejected, stalled atomic.Int64
}

// Start listens on c.Listen and serves the OTLP collector services until
// Close is called or ctx ends.
func Start(ctx context.Context, c Config) (*Sink, error) {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:0"
	}
	if c.MaxRecvMsgSize <= 0 {
		c.MaxRecvMsgSize = DefaultMaxRecvMsgSize
	}
	if c.MaxRequests <= 0 {
		c.MaxRequests = DefaultMaxRequests
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if c.Mode == nil {
		c.Mode = Accept{}
	}
	if err := validMode(c.Mode); err != nil {
		return nil, err
	}
	if c.TLS != nil {
		c.TLS = c.TLS.Clone()
	}
	codec, err := newRawCodec()
	if err != nil {
		return nil, err
	}
	ln, err := listen(ctx, c.Listen)
	if err != nil {
		return nil, err
	}
	s := &Sink{
		cfg:     c,
		addr:    ln.Addr().String(),
		codec:   codec,
		conns:   &connStats{},
		rec:     newRecorder(c.MaxRequests, c.MaxBytes),
		closed:  make(chan struct{}),
		mode:    c.Mode,
		release: make(chan struct{}),
	}
	if _, down := c.Mode.(Down); down {
		_ = ln.Close()
	} else {
		s.serve(ln)
	}
	// The callback may run at once (ctx already done); Close then waits
	// for s.mu, so stopAfter is assigned before it is read.
	s.mu.Lock()
	s.stopAfter = context.AfterFunc(ctx, func() { _ = s.Close() })
	s.mu.Unlock()
	return s, nil
}

func listen(ctx context.Context, addr string) (net.Listener, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("otlpsink: listen %s: %w", addr, err)
	}
	return ln, nil
}

// serve starts a gRPC server on ln; the caller holds s.mu or owns s
// exclusively.
func (s *Sink) serve(ln net.Listener) {
	opts := []grpc.ServerOption{
		grpc.ForceServerCodecV2(s.codec),
		grpc.MaxRecvMsgSize(s.cfg.MaxRecvMsgSize),
		// Stop returns only after every handler has; handlers end
		// promptly because hold watches the export context and Close.
		grpc.WaitForHandlers(true),
		grpc.StatsHandler(s.conns),
	}
	if s.cfg.TLS != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(s.cfg.TLS)))
	}
	srv := grpc.NewServer(opts...)
	s.register(srv)
	s.srv = srv
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = srv.Serve(ln)
	}()
}

// Addr returns the listen address (host:port); it stays the same across
// Down.
func (s *Sink) Addr() string { return s.addr }

// URL returns the endpoint URL for Gateway.spec.telemetry.otlp.endpoint:
// "https://<addr>" when serving TLS, else "http://<addr>".
func (s *Sink) URL() string {
	if s.cfg.TLS != nil {
		return "https://" + s.addr
	}
	return "http://" + s.addr
}

// Mode returns the current mode.
func (s *Sink) Mode() Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// SetMode switches the mode for held and new exports. Entering Down stops
// the server; leaving Down listens on Addr again, which fails when the
// port was taken meanwhile (the sink then stays Down).
func (s *Sink) SetMode(m Mode) error {
	if err := validMode(m); err != nil {
		return err
	}
	s.modeMu.Lock()
	defer s.modeMu.Unlock()
	s.mu.Lock()
	if s.isClosed {
		s.mu.Unlock()
		return ErrClosed
	}
	old := s.mode
	_, down := m.(Down)
	_, wasDown := old.(Down)
	if !down && wasDown {
		ln, err := listen(context.Background(), s.addr)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		s.serve(ln)
	}
	_, stall := m.(Stall)
	_, wasStall := old.(Stall)
	switch {
	case wasStall && !stall:
		close(s.release)
	case stall && !wasStall:
		s.release = make(chan struct{})
	}
	s.mode = m
	var stop *grpc.Server
	if down && !wasDown {
		stop, s.srv = s.srv, nil
	}
	s.mu.Unlock()
	if stop != nil {
		// Outside s.mu: Stop ends in-flight exports, whose handlers take
		// s.mu to read the mode.
		stop.Stop()
	}
	return nil
}

// Stats returns the export counters.
func (s *Sink) Stats() Stats {
	st := s.rec.stats()
	st.Rejected = s.rejected.Load()
	st.Stalled = s.stalled.Load()
	st.Connections = s.conns.opened.Load()
	st.OpenConnections = s.conns.open.Load()
	return st
}

// Close stops the server, ends held exports and waits for the serve loop.
// Recorded data stays readable. It is idempotent.
func (s *Sink) Close() error {
	s.closeOnce.Do(func() {
		s.modeMu.Lock()
		defer s.modeMu.Unlock()
		s.mu.Lock()
		s.isClosed = true
		if _, stall := s.mode.(Stall); stall {
			close(s.release)
		}
		srv := s.srv
		s.srv = nil
		stopAfter := s.stopAfter
		s.mu.Unlock()
		close(s.closed)
		if srv != nil {
			srv.Stop()
		}
		s.wg.Wait()
		if stopAfter != nil {
			stopAfter()
		}
	})
	return nil
}

// admit applies the mode to one export; nil records it.
func (s *Sink) admit(ctx context.Context) error {
	for {
		s.mu.Lock()
		m, release, closed := s.mode, s.release, s.isClosed
		s.mu.Unlock()
		if closed {
			return status.Error(codes.Unavailable, "otlpsink: closed")
		}
		if err := ctx.Err(); err != nil {
			// The client gave up (a Stall outlived its deadline).
			return status.FromContextError(err).Err()
		}
		switch m := m.(type) {
		case Accept:
			return nil
		case Reject:
			msg := m.Message
			if msg == "" {
				msg = "otlpsink: rejected"
			}
			return status.Error(m.Code, msg)
		case Down:
			return status.Error(codes.Unavailable, "otlpsink: down")
		case Stall:
			if err := s.hold(ctx, release); err != nil {
				return err
			}
		}
	}
}

// hold waits for release (the mode left Stall), the export's context or
// Close.
func (s *Sink) hold(ctx context.Context, release <-chan struct{}) error {
	s.stalled.Add(1)
	defer s.stalled.Add(-1)
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-s.closed:
		return status.Error(codes.Unavailable, "otlpsink: closed")
	}
}

// handle runs one decoded export with its wire bytes: apply the mode,
// record.
func (s *Sink) handle(ctx context.Context, sig Signal, msg any, raw []byte) error {
	if err := s.admit(ctx); err != nil {
		s.rejected.Add(1)
		return err
	}
	req := Request{Signal: sig, Received: time.Now(), Raw: raw, Conn: connSeq(ctx)}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		req.Metadata = map[string][]string(md.Copy())
	}
	if p, ok := peer.FromContext(ctx); ok {
		if p.Addr != nil {
			req.Peer = p.Addr.String()
		}
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok {
			state := info.State
			req.TLS = &state
		}
	}
	switch m := msg.(type) {
	case *coltrace.ExportTraceServiceRequest:
		req.Traces = m
	case *colmetrics.ExportMetricsServiceRequest:
		req.Metrics = m
	case *collogs.ExportLogsServiceRequest:
		req.Logs = m
	}
	s.rec.add(req)
	return nil
}
