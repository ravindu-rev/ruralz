// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package mockup is the mock Upstream of the process harness
// (docs/engineering/03-testing-and-quality-strategy.md, "Process
// harness"): an HTTP server on loopback that speaks HTTP/1.1 and h2c
// (HTTP/2 over cleartext with prior knowledge) or, with a TLS
// configuration passed in (from testkit/pki), HTTP/1.1 and HTTP/2 over TLS
// negotiated by ALPN.
//
// Its answers are programmable per Server and per path with a Behavior:
// status, headers, a literal or generated body of any size, a delay
// before the headers, and echo (a JSON description of the request).
//
// Every request is counted and, unless disabled, kept in a bounded
// request log (method, URI, protocol, headers, trailers and a body
// prefix), which secret leak tests scan for credentials a Node must
// strip; Stats.LogEvicted and Request.BodyTruncated, with the
// conditions on Request, tell such a scan whether the log dropped
// anything.
//
// Goroutines: the http.Server's serve loop and its per-connection
// goroutines, owned by the Server; Close, or the end of the Start
// context, closes the listener and every connection, ends delayed
// responses, and waits for the serve loop and every handler.
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
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults and bounds.
const (
	// DefaultLogSize is the request log capacity.
	DefaultLogSize = 1024
	// DefaultMaxLoggedBody bounds the body bytes kept per logged request.
	DefaultMaxLoggedBody = 64 << 10
	// maxEchoBody bounds the request body bytes an Echo returns.
	maxEchoBody = 1 << 20
	// readHeaderTimeout bounds reading request headers.
	readHeaderTimeout = 10 * time.Second
)

// ErrClosed is returned by SetRoute on a closed Server, and by
// WaitRequests when the Server closed before enough requests arrived.
var ErrClosed = errors.New("mockup: closed")

// Config configures a Server.
type Config struct {
	// Name identifies the mock, which Echo reports.
	Name string
	// Listen is the listen address; default "127.0.0.1:0".
	Listen string
	// TLS, when set, serves HTTP over TLS with this configuration (ALPN
	// h2 and http/1.1). When nil the Server serves cleartext HTTP/1.1
	// and h2c with prior knowledge on the same port.
	TLS *tls.Config
	// Behavior answers every path without a route.
	Behavior Behavior
	// LogSize is the request log capacity (a ring keeping the newest,
	// counting the dropped ones in Stats.LogEvicted); 0 means
	// DefaultLogSize and a negative value disables the log (the request
	// count still runs).
	LogSize int
	// MaxLoggedBody bounds the body bytes kept per logged request
	// (Request.BodyTruncated marks a cut body); 0 means
	// DefaultMaxLoggedBody, negative keeps none.
	MaxLoggedBody int

	// listen, when set, makes the Server's listener in place of a TCP
	// listener on Listen. Tests only.
	listen func(ctx context.Context, addr string) (net.Listener, error)
}

// Stats counts requests and connections.
type Stats struct {
	// Requests counts requests received (logged or not).
	Requests int64
	// Connections counts accepted connections.
	Connections int64
	// LogEvicted counts logged requests dropped from the full request
	// log to make room for newer ones. A leak scan over Requests is
	// incomplete unless it is 0 (see Request for the other conditions).
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

	mu        sync.Mutex // guards closed, stopAfter and handler admission
	closed    bool
	stopAfter func() bool
	handlers  sync.WaitGroup
	serveDone chan struct{}
	closeOnce sync.Once

	requests, conns atomic.Int64
}

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
	if err := c.Behavior.validate(); err != nil {
		return nil, fmt.Errorf("mockup: %w", err)
	}
	if c.TLS != nil {
		c.TLS = c.TLS.Clone()
	}
	var ln net.Listener
	var err error
	if c.listen != nil {
		ln, err = c.listen(ctx, c.Listen)
	} else {
		ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", c.Listen)
	}
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
		serveDone: make(chan struct{}),
	}
	s.table.Store(&routes{def: c.Behavior.clone(), paths: map[string]Behavior{}})
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	if c.TLS != nil {
		protocols.SetHTTP2(true)
	} else {
		protocols.SetUnencryptedHTTP2(true)
		// net/http then reads the h2c preface's first bytes alone, under the
		// header deadline: each connection's first read takes more.
		ln = firstReadListener{ln}
	}
	s.srv = &http.Server{
		Handler:           s,
		TLSConfig:         c.TLS,
		ReadHeaderTimeout: readHeaderTimeout,
		Protocols:         protocols,
		ErrorLog:          slog.NewLogLogger(slog.DiscardHandler, slog.LevelError),
		// Handlers outlive neither Close nor ctx (the AfterFunc below
		// closes the Server), so they carry ctx's values, not its end.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
		ConnState:   s.connState,
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
	if st == http.StateNew {
		s.conns.Add(1)
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
	for {
		old := s.table.Load()
		paths := make(map[string]Behavior, len(old.paths)+1)
		for k, v := range old.paths {
			paths[k] = v
		}
		paths[path] = b
		if s.table.CompareAndSwap(old, &routes{def: old.def, paths: paths}) {
			return nil
		}
	}
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

// Stats returns the counters.
func (s *Server) Stats() Stats {
	_, evicted := s.log.counts()
	return Stats{
		Requests:    s.requests.Load(),
		Connections: s.conns.Load(),
		LogEvicted:  evicted,
	}
}

// Close closes the listener and every connection, ends delayed
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

// ServeHTTP answers one request with its Behavior.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.enter() {
		panic(http.ErrAbortHandler)
	}
	defer s.handlers.Done()
	s.requests.Add(1)

	t := s.table.Load()
	b, ok := t.paths[r.URL.Path]
	if !ok {
		b = t.def
	}

	keep := max(s.cfg.MaxLoggedBody, 0)
	if s.cfg.LogSize < 0 {
		keep = 0
	}
	if b.Echo {
		keep = max(keep, maxEchoBody)
	}
	body, n := readBody(r.Body, keep)
	s.log.add(r, body, n, s.cfg.MaxLoggedBody, s.cfg.LogSize > 0)

	if !s.wait(r.Context(), b.Delay) {
		panic(http.ErrAbortHandler)
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
		lit = s.echo(r, body[:min(len(body), maxEchoBody)], n)
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
		if status != http.StatusNoContent && status != http.StatusNotModified {
			h.Set("Content-Length", strconv.FormatInt(size, 10))
		}
	}
	for k, v := range b.Header {
		h[k] = append([]string(nil), v...)
	}
	w.WriteHeader(status)
	if bodyless {
		return
	}
	if err := s.writeBody(w, lit, size); err != nil {
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

// writeBody writes the first n bytes of the body: lit, or the generated
// pattern when lit is nil.
func (s *Server) writeBody(w io.Writer, lit []byte, n int64) error {
	for off := int64(0); off < n; {
		k, err := w.Write(s.piece(lit, off, n-off))
		off += int64(k)
		if err != nil {
			return err
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
