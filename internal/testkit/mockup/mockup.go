// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package mockup is the mock Upstream of the test kit (11 section 3, F 32,
// req 39; PBB scenarios S1 and O1a): an HTTP server on loopback that
// speaks HTTP/1.1 and h2c (HTTP/2 over cleartext with prior knowledge)
// or, with a TLS configuration passed in (from testkit/pki), HTTP/1.1 and
// HTTP/2 over TLS negotiated by ALPN.
//
// Its answers are programmable per Server and per path with a Behavior:
// status, headers, a literal or generated body of any size, a delay drawn
// from a distribution (Fixed, Uniform, Normal, Exponential), a slow body
// (chunks paced by an interval), a reset (TCP RST of the connection, or
// an aborted HTTP/2 stream) before or after the headers, and echo (a JSON
// description of the request). With Config.Overrides, X-Mockup-* request
// headers override the Behavior per request, so a test drives the mock
// through a Node.
//
// Every request is counted and, unless disabled, kept in a bounded
// request log (method, URI, headers, framing, body prefix, protocol,
// TLS), which secret leak tests scan for credentials a Node must strip
// (11 req 39); Stats.LogEvicted and Request.BodyTruncated, with the
// conditions on Request, tell such a scan whether the log dropped
// anything.
//
// Goroutines: the http.Server's serve loop and its per-connection
// goroutines, owned by the Server; Close, or the end of the Start
// context, closes the listener and every connection, ends delayed and
// slow responses, and waits for the serve loop and every handler.
package mockup

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults.
const (
	// DefaultLogSize is the request log capacity.
	DefaultLogSize = 1024
	// DefaultMaxLoggedBody bounds the body bytes kept per logged request.
	DefaultMaxLoggedBody = 64 << 10
	// DefaultMaxEchoBody bounds the request body bytes an Echo returns.
	DefaultMaxEchoBody = 1 << 20
	// DefaultReadHeaderTimeout bounds reading request headers.
	DefaultReadHeaderTimeout = 10 * time.Second
	// DefaultChunkSize is a slow body's chunk.
	DefaultChunkSize = 1024
)

// ErrClosed is returned by SetBehavior and SetRoute on a closed Server,
// and by WaitRequests when the Server closed before enough requests
// arrived.
var ErrClosed = errors.New("mockup: closed")

// Config configures a Server.
type Config struct {
	// Name identifies the mock; when set, every response carries it in
	// HeaderServer and Echo reports it.
	Name string
	// Listen is the listen address; default "127.0.0.1:0".
	Listen string
	// TLS, when set, serves HTTP over TLS with this configuration (ALPN
	// h2 and http/1.1). When nil the Server serves cleartext HTTP/1.1
	// and h2c with prior knowledge on the same port.
	TLS *tls.Config
	// DisableHTTP2 serves HTTP/1.1 only (no h2c, no h2 over TLS).
	DisableHTTP2 bool
	// MaxConcurrentStreams bounds streams per HTTP/2 connection; 0 keeps
	// the net/http default (250).
	MaxConcurrentStreams int
	// Behavior answers every path without a route.
	Behavior Behavior
	// Overrides honors the X-Mockup-* request headers.
	Overrides bool
	// LogSize is the request log capacity (a ring keeping the newest,
	// counting the dropped ones in Stats.LogEvicted); 0 means
	// DefaultLogSize and a negative value disables the log (the request
	// count still runs), as benchmarks do.
	LogSize int
	// MaxLoggedBody bounds the body bytes kept per logged request
	// (Request.BodyTruncated marks a cut body); 0 means
	// DefaultMaxLoggedBody, negative keeps none.
	MaxLoggedBody int
	// MaxEchoBody bounds the request body bytes an Echo returns; 0 means
	// DefaultMaxEchoBody.
	MaxEchoBody int
	// ReadHeaderTimeout bounds reading request headers; default
	// DefaultReadHeaderTimeout.
	ReadHeaderTimeout time.Duration
	// IdleTimeout closes idle keep-alive connections; 0 keeps them.
	IdleTimeout time.Duration
	// ErrorLog receives the net/http server's own errors (for example
	// TLS handshake failures); nil discards them.
	ErrorLog slog.Handler
	// Seed seeds the delay distributions; 0 picks a random seed.
	Seed uint64
}

// Stats counts requests and connections.
type Stats struct {
	// Requests counts requests received (logged or not).
	Requests int64
	// Active is the number of requests being answered now.
	Active int64
	// Resets counts responses broken by a reset.
	Resets int64
	// Connections counts accepted connections; OpenConnections is the
	// number open now.
	Connections, OpenConnections int64
	// LogEvicted counts logged requests dropped from the full request
	// log to make room for newer ones (ClearRequests is not counted). A
	// leak scan over Requests is incomplete unless it is 0 (see Request
	// for the other conditions).
	LogEvicted int64
}

// routes is the immutable routing table, replaced on every change.
type routes struct {
	def   Behavior
	paths map[string]Behavior
}

// Server is a running mock Upstream.
type Server struct {
	cfg     Config
	addr    string
	srv     *http.Server
	pattern []byte
	log     *requestLog
	closing chan struct{} // closed when Close starts
	drained chan struct{} // closed when Close has ended every handler

	table atomic.Pointer[routes]

	rngMu sync.Mutex
	rng   *rand.Rand

	mu        sync.Mutex // guards closed, stopAfter and handler admission
	closed    bool
	stopAfter func() bool
	handlers  sync.WaitGroup
	serveDone chan struct{}
	closeOnce sync.Once

	requests, active, resets, conns, openConns atomic.Int64
}

type connKey struct{}

// Start listens on c.Listen and serves until Close is called or ctx ends.
func Start(ctx context.Context, c Config) (*Server, error) {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:0"
	}
	if c.LogSize == 0 {
		c.LogSize = DefaultLogSize
	}
	if c.MaxLoggedBody == 0 {
		c.MaxLoggedBody = DefaultMaxLoggedBody
	}
	if c.MaxEchoBody <= 0 {
		c.MaxEchoBody = DefaultMaxEchoBody
	}
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if c.MaxConcurrentStreams < 0 {
		return nil, errors.New("mockup: negative MaxConcurrentStreams")
	}
	if err := c.Behavior.validate(); err != nil {
		return nil, fmt.Errorf("mockup: %w", err)
	}
	if c.TLS != nil {
		c.TLS = c.TLS.Clone()
	}
	seed := c.Seed
	if seed == 0 {
		seed = rand.Uint64() //nolint:gosec // G404: delay jitter, not security
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.Listen)
	if err != nil {
		return nil, fmt.Errorf("mockup: listen %s: %w", c.Listen, err)
	}
	s := &Server{
		cfg:       c,
		addr:      ln.Addr().String(),
		pattern:   GeneratedBody(patternSize),
		log:       newRequestLog(c.LogSize),
		closing:   make(chan struct{}),
		drained:   make(chan struct{}),
		rng:       rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), //nolint:gosec // G404: delay jitter, not security
		serveDone: make(chan struct{}),
	}
	s.table.Store(&routes{def: c.Behavior.clone(), paths: map[string]Behavior{}})
	errorLog := c.ErrorLog
	if errorLog == nil {
		errorLog = slog.DiscardHandler
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	switch {
	case c.DisableHTTP2:
	case c.TLS != nil:
		protocols.SetHTTP2(true)
	default:
		protocols.SetUnencryptedHTTP2(true)
	}
	s.srv = &http.Server{
		Handler:           s,
		TLSConfig:         c.TLS,
		ReadHeaderTimeout: c.ReadHeaderTimeout,
		IdleTimeout:       c.IdleTimeout,
		Protocols:         protocols,
		HTTP2:             &http.HTTP2Config{MaxConcurrentStreams: c.MaxConcurrentStreams},
		ErrorLog:          slog.NewLogLogger(errorLog, slog.LevelError),
		// Handlers outlive neither Close nor ctx (the AfterFunc below
		// closes the Server), so they carry ctx's values, not its end.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if tc, ok := c.(*tls.Conn); ok {
				c = tc.NetConn()
			}
			return context.WithValue(ctx, connKey{}, c)
		},
		ConnState: s.connState,
	}
	go func() {
		defer close(s.serveDone)
		if c.TLS != nil {
			_ = s.srv.ServeTLS(ln, "", "")
			return
		}
		_ = s.srv.Serve(ln)
	}()
	// The callback may run at once (ctx already done); Close then waits
	// for s.mu, so stopAfter is assigned before it is read.
	s.mu.Lock()
	s.stopAfter = context.AfterFunc(ctx, func() { _ = s.Close() })
	s.mu.Unlock()
	return s, nil
}

func (s *Server) connState(_ net.Conn, st http.ConnState) {
	switch st {
	case http.StateNew:
		s.conns.Add(1)
		s.openConns.Add(1)
	case http.StateClosed, http.StateHijacked:
		s.openConns.Add(-1)
	case http.StateActive, http.StateIdle:
	}
}

// Name returns Config.Name.
func (s *Server) Name() string { return s.cfg.Name }

// Addr returns the listen address (host:port).
func (s *Server) Addr() string { return s.addr }

// URL returns "https://<addr>" when serving TLS, else "http://<addr>".
func (s *Server) URL() string {
	if s.cfg.TLS != nil {
		return "https://" + s.addr
	}
	return "http://" + s.addr
}

// Behavior returns the default Behavior.
func (s *Server) Behavior() Behavior { return s.table.Load().def.clone() }

// SetBehavior replaces the default Behavior for new requests. It returns
// ErrClosed once the Server is closed.
func (s *Server) SetBehavior(b Behavior) error {
	if s.isClosing() {
		return ErrClosed
	}
	if err := b.validate(); err != nil {
		return fmt.Errorf("mockup: %w", err)
	}
	b = b.clone()
	for {
		old := s.table.Load()
		if s.table.CompareAndSwap(old, &routes{def: b, paths: old.paths}) {
			return nil
		}
	}
}

// SetRoute answers requests whose URL path equals path with b instead of
// the default Behavior. It returns ErrClosed once the Server is closed.
func (s *Server) SetRoute(path string, b Behavior) error {
	if s.isClosing() {
		return ErrClosed
	}
	if err := b.validate(); err != nil {
		return fmt.Errorf("mockup: %w", err)
	}
	b = b.clone()
	s.updatePaths(func(m map[string]Behavior) { m[path] = b })
	return nil
}

// isClosing reports whether Close has started.
func (s *Server) isClosing() bool {
	select {
	case <-s.closing:
		return true
	default:
		return false
	}
}

// DeleteRoute returns path to the default Behavior.
func (s *Server) DeleteRoute(path string) {
	s.updatePaths(func(m map[string]Behavior) { delete(m, path) })
}

func (s *Server) updatePaths(edit func(map[string]Behavior)) {
	for {
		old := s.table.Load()
		paths := make(map[string]Behavior, len(old.paths)+1)
		for k, v := range old.paths {
			paths[k] = v
		}
		edit(paths)
		if s.table.CompareAndSwap(old, &routes{def: old.def, paths: paths}) {
			return
		}
	}
}

// Stats returns the counters.
func (s *Server) Stats() Stats {
	_, evicted := s.log.counts()
	return Stats{
		Requests:        s.requests.Load(),
		Active:          s.active.Load(),
		Resets:          s.resets.Load(),
		Connections:     s.conns.Load(),
		OpenConnections: s.openConns.Load(),
		LogEvicted:      evicted,
	}
}

// Close closes the listener and every connection, ends delayed and slow
// responses, and waits for the serve loop and every handler. It is
// idempotent; the request log stays readable.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		stopAfter := s.stopAfter
		s.mu.Unlock()
		close(s.closing)
		err = s.srv.Close()
		<-s.serveDone
		s.handlers.Wait()
		close(s.drained)
		if stopAfter != nil {
			stopAfter()
		}
	})
	return err
}

// enter admits a handler unless the Server is closing; the admission and
// Close's closed flag share s.mu, so handlers.Add never races
// handlers.Wait.
func (s *Server) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.handlers.Add(1)
	return true
}

func (s *Server) sample(d Distribution) time.Duration {
	if d == nil {
		return 0
	}
	s.rngMu.Lock()
	defer s.rngMu.Unlock()
	return d.Sample(s.rng)
}

// ServeHTTP answers one request with its Behavior.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.enter() {
		panic(http.ErrAbortHandler)
	}
	defer s.handlers.Done()
	s.requests.Add(1)
	s.active.Add(1)
	defer s.active.Add(-1)
	if s.cfg.Name != "" {
		w.Header().Set(HeaderServer, s.cfg.Name)
	}

	t := s.table.Load()
	b, ok := t.paths[r.URL.Path]
	if !ok {
		b = t.def
	}
	var overrideErr error
	if s.cfg.Overrides {
		b, overrideErr = applyOverrides(b, r.Header)
	}

	keep := max(s.cfg.MaxLoggedBody, 0)
	if s.cfg.LogSize < 0 {
		keep = 0
	}
	if b.Echo {
		keep = max(keep, s.cfg.MaxEchoBody)
	}
	body, n := readBody(r.Body, keep)
	delay := s.sample(b.Delay)
	s.log.add(r, body, n, s.cfg.MaxLoggedBody, delay, s.cfg.LogSize > 0)

	if overrideErr != nil {
		http.Error(w, "mockup: "+overrideErr.Error(), http.StatusBadRequest)
		return
	}
	if !s.wait(r.Context(), delay) {
		panic(http.ErrAbortHandler)
	}
	if b.Reset != NoReset && !b.ResetAfterHeaders {
		s.reset(r, b.Reset)
	}

	status := b.Status
	if status == 0 {
		status = http.StatusOK
	}
	lit, size := b.Body, b.BodySize
	if lit != nil {
		size = int64(len(lit))
	}
	ctype := b.ContentType
	if b.Echo {
		lit = s.echo(r, body[:min(len(body), s.cfg.MaxEchoBody)], n)
		size = int64(len(lit))
		if ctype == "" {
			ctype = "application/json"
		}
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	bodyless := r.Method == http.MethodHead || status == http.StatusNoContent || status == http.StatusNotModified
	h := w.Header()
	if !bodyless || r.Method == http.MethodHead {
		h.Set("Content-Type", ctype)
		if !b.Chunked && status != http.StatusNoContent && status != http.StatusNotModified {
			h.Set("Content-Length", strconv.FormatInt(size, 10))
		}
	}
	for k, v := range b.Header {
		h[k] = append([]string(nil), v...)
	}
	w.WriteHeader(status)
	rc := http.NewResponseController(w)

	if b.Reset != NoReset {
		_ = rc.Flush()
		if !bodyless {
			_ = s.writeBody(r.Context(), w, rc, lit, min(size, b.ResetAfterBytes), b)
			_ = rc.Flush()
		}
		s.reset(r, b.Reset)
	}
	if bodyless {
		return
	}
	if err := s.writeBody(r.Context(), w, rc, lit, size, b); err != nil {
		panic(http.ErrAbortHandler)
	}
}

// wait sleeps d unless the request or the Server ends first.
func (s *Server) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-s.closing:
		return false
	}
}

// reset breaks the response and never returns.
func (s *Server) reset(r *http.Request, kind ResetKind) {
	s.resets.Add(1)
	if kind == ResetConn {
		if c, ok := r.Context().Value(connKey{}).(net.Conn); ok {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			_ = c.Close()
		}
	}
	panic(http.ErrAbortHandler)
}

// writeBody writes the first n bytes of the body (lit, or the generated
// pattern when lit is nil), paced by b's chunk interval.
func (s *Server) writeBody(ctx context.Context, w io.Writer, rc *http.ResponseController, lit []byte, n int64, b Behavior) error {
	chunk := int64(1 << 30)
	if b.ChunkInterval > 0 {
		chunk = int64(b.ChunkSize)
		if chunk <= 0 {
			chunk = DefaultChunkSize
		}
	}
	for off := int64(0); off < n; {
		if off > 0 && b.ChunkInterval > 0 {
			if !s.wait(ctx, b.ChunkInterval) {
				return context.Canceled
			}
		}
		end := min(off+chunk, n)
		for off < end {
			p := s.piece(lit, off, end-off)
			k, err := w.Write(p)
			off += int64(k)
			if err != nil {
				return err
			}
		}
		if b.ChunkInterval > 0 {
			if err := rc.Flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

// piece returns up to n body bytes starting at off.
func (s *Server) piece(lit []byte, off, n int64) []byte {
	if lit != nil {
		return lit[off : off+n]
	}
	start := off % int64(len(s.pattern))
	return s.pattern[start:min(int64(len(s.pattern)), start+n)]
}

// patternAlphabet is the generated body's repeating unit; patternSize is
// a multiple of its length, so the pattern block wraps seamlessly.
const (
	patternAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	patternSize     = len(patternAlphabet) * 1024
)

// GeneratedBody returns the n bytes a Behavior without a literal Body
// sends: patternAlphabet repeated, so tests can check integrity end to
// end.
func GeneratedBody(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = patternAlphabet[i%len(patternAlphabet)]
	}
	return out
}

// readBody reads the whole request body, keeping at most keep bytes, and
// returns the kept bytes and the total length.
func readBody(body io.Reader, keep int) ([]byte, int64) {
	if body == nil {
		return nil, 0
	}
	var buf bytes.Buffer
	n, _ := io.Copy(&buf, io.LimitReader(body, int64(keep)))
	rest, _ := io.Copy(io.Discard, body)
	return buf.Bytes(), n + rest
}

// Echo is the JSON document an Echo Behavior answers with.
type Echo struct {
	// Server is Config.Name of the answering mock.
	Server string `json:"server,omitempty"`
	// Method, Host, URI (the request target as received), Path and
	// RawQuery describe the request line; Proto is "HTTP/1.1" or
	// "HTTP/2.0".
	Method   string `json:"method"`
	Host     string `json:"host"`
	URI      string `json:"uri"`
	Path     string `json:"path"`
	RawQuery string `json:"rawQuery,omitempty"`
	Proto    string `json:"proto"`
	// Header and Trailer are the request's fields.
	Header  map[string][]string `json:"header"`
	Trailer map[string][]string `json:"trailer,omitempty"`
	// ContentLength, TransferEncoding and Close show the request's
	// framing, as in Request.
	ContentLength    int64    `json:"contentLength"`
	TransferEncoding []string `json:"transferEncoding,omitempty"`
	Close            bool     `json:"close"`
	// Body is the request body (base64 in JSON), at most
	// Config.MaxEchoBody bytes; BodyBytes is its full length.
	Body      []byte `json:"body"`
	BodyBytes int64  `json:"bodyBytes"`
	// RemoteAddr is the client's address.
	RemoteAddr string `json:"remoteAddr"`
	// TLS reports a TLS connection, ServerName its SNI and ALPN the
	// negotiated protocol.
	TLS        bool   `json:"tls"`
	ServerName string `json:"serverName,omitempty"`
	ALPN       string `json:"alpn,omitempty"`
}

func (s *Server) echo(r *http.Request, body []byte, n int64) []byte {
	e := Echo{
		Server:           s.cfg.Name,
		Method:           r.Method,
		Host:             r.Host,
		URI:              r.RequestURI,
		Path:             r.URL.Path,
		RawQuery:         r.URL.RawQuery,
		Proto:            r.Proto,
		Header:           r.Header,
		Trailer:          r.Trailer,
		ContentLength:    r.ContentLength,
		TransferEncoding: r.TransferEncoding,
		Close:            r.Close,
		Body:             body,
		BodyBytes:        n,
		RemoteAddr:       r.RemoteAddr,
	}
	if e.Body == nil {
		e.Body = []byte{}
	}
	if r.TLS != nil {
		e.TLS, e.ServerName, e.ALPN = true, r.TLS.ServerName, r.TLS.NegotiatedProtocol
	}
	out, err := json.Marshal(e)
	if err != nil {
		return []byte(`{"error":"echo encoding failed"}`)
	}
	return out
}
