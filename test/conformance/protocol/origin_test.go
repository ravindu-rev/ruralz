// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package protocol

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// upstream is the transport of one test Upstream: the Node reaches it as
// an http Upstream over HTTP/1.1, HTTP/1.1 over TLS or HTTP/2 over TLS
// (docs/architecture/07-multi-protocol.md "Streams and chunks": HTTP/1.1
// in cleartext, HTTP/2 by ALPN over TLS).
type upstream int

// Upstream transports.
const (
	// upH1 is HTTP/1.1 in cleartext.
	upH1 upstream = iota
	// upH1TLS is HTTP/1.1 over TLS: the server offers only http/1.1 by
	// ALPN.
	upH1TLS
	// upH2 is HTTP/2 over TLS: the server offers only h2 by ALPN.
	upH2
	// upWire is HTTP/1.1 in cleartext to a raw origin that records each
	// request head as its bytes arrived, which net/http's server would
	// normalize; it answers 200 "ok" and closes the connection.
	upWire
	// upBlackhole is TLS to a listener that accepts connections and never
	// answers, so an attempt waits on its TLS handshake, whose timeout
	// runs on the Node clock.
	upBlackhole
	numOrigins
)

// upstreams returns every Upstream transport.
func upstreams() []upstream { return []upstream{upH1, upH1TLS, upH2} }

// String names the transport; it is also the Upstream's metadata.name.
func (u upstream) String() string {
	switch u {
	case upH1:
		return "h1"
	case upH1TLS:
		return "h1tls"
	case upH2:
		return "h2"
	case upWire:
		return "wire"
	case upBlackhole:
		return "blackhole"
	default:
		return "upstream(" + strconv.Itoa(int(u)) + ")"
	}
}

// proto is the protocol the Upstream sees on its side of the hop.
func (u upstream) proto() string {
	if u == upH2 {
		return "HTTP/2.0"
	}
	return "HTTP/1.1"
}

// path returns the request path of behavior on the plain Route to u:
// /up/<transport>/<behavior>.
func (u upstream) path(behavior string) string { return "/up/" + u.String() + "/" + behavior }

// Field names the suite and its origin use.
const (
	// caseHeader carries the ID of one exchange, under which the origin
	// records what reached it.
	caseHeader = "X-Conformance-Case"
	// originHeader names the origin transport on every origin response.
	originHeader = "X-Conformance-Origin"
)

// Origin behaviors, chosen by the last path segment.
const (
	// bOK answers 200 with "ok" after reading the whole request.
	bOK = "ok"
	// bTrailers answers 200 with a declared and an undeclared trailer
	// field.
	bTrailers = "trailers"
	// bRawTrailers writes, on an HTTP/1.1 transport, a chunked response by
	// hand whose trailer section also holds hop-by-hop and framing fields.
	bRawTrailers = "raw-trailers"
	// bConnTrailers answers like bTrailers, plus a declared and an
	// undeclared trailer field that its Connection field names.
	bConnTrailers = "conn-trailers"
	// bHints sends 103 Early Hints with a Link field before 200.
	bHints = "hints"
	// bHopHints sends 103 Early Hints whose Link travels with hop-by-hop
	// fields: Keep-Alive and a field its own Connection field names.
	bHopHints = "hop-hints"
	// bInterim sends an unsolicited 100 Continue, 102 Processing and 103
	// Early Hints before 200.
	bInterim = "interim"
	// bDate answers with a fixed Date and a Server field.
	bDate = "date"
	// bNoDate answers without a Date field.
	bNoDate = "no-date"
	// bNotModified answers 304 with an ETag.
	bNotModified = "not-modified"
	// bSized answers 200 with Content-Length 1234 and its body (none to a
	// HEAD).
	bSized = "sized"
	// bHopByHop answers with hop-by-hop fields and one a Connection
	// option names.
	bHopByHop = "hop-by-hop"
	// bFlaky answers 503 to the first attempt of a case and 200 to the
	// next.
	bFlaky = "flaky"
	// bHold records the request head without reading its body and waits
	// until the case is released or the request ends.
	bHold = "hold"
	// bBig answers 200 with a body of ?size= bytes, written in 32 KiB
	// pieces.
	bBig = "big"
	// bVia answers with a Via field of its own.
	bVia = "via"
)

// seen is what one request carried when it reached the origin.
type seen struct {
	Upstream   upstream
	Proto      string
	ALPN       string
	Method     string
	RequestURI string
	Host       string
	Header     http.Header
	Body       []byte
	// Truncated reports a body whose read failed: Body holds what came
	// before the failure, not a complete request.
	Truncated bool
	Trailer   http.Header
	// Attempt numbers the requests of one case at the origin, from 1.
	Attempt int
	// RawHead is the request head as its bytes arrived, on upWire only.
	RawHead string
}

// origin is the in-process test Upstream of a topology: one server per
// transport, sharing a recorder keyed by caseHeader.
type origin struct {
	t     testing.TB
	addrs [numOrigins]string

	mu      sync.Mutex
	seen    map[string][]seen
	changed chan struct{}
	release map[string]chan struct{}
	held    map[string]int
	peak    map[string]int

	ids atomic.Int64
}

// originReadHeaderTimeout bounds the origin's header reads, a test hang
// guard.
const originReadHeaderTimeout = 30 * time.Second

// startOrigin starts the three transports of the origin on loopback, the
// TLS ones with certificates from the topology's CA, which every Node
// trusts. They end with the test.
func startOrigin(t testing.TB, top *topology.Topology) *origin {
	t.Helper()
	o := &origin{
		t: t, seen: map[string][]seen{}, changed: make(chan struct{}),
		release: map[string]chan struct{}{}, held: map[string]int{}, peak: map[string]int{},
	}
	for _, u := range upstreams() {
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var protos http.Protocols
		srv := &http.Server{
			Handler:           o.handler(u),
			ReadHeaderTimeout: originReadHeaderTimeout,
			ErrorLog:          slog.NewLogLogger(slog.DiscardHandler, slog.LevelError),
			Protocols:         &protos,
		}
		switch u {
		case upH1:
			protos.SetHTTP1(true)
		case upH1TLS, upH2:
			cfg, err := top.ServerTLS()
			if err != nil {
				t.Fatal(err)
			}
			if u == upH1TLS {
				protos.SetHTTP1(true)
				cfg.NextProtos = []string{"http/1.1"}
			} else {
				protos.SetHTTP2(true)
				cfg.NextProtos = []string{"h2"}
			}
			srv.TLSConfig = cfg
			ln = tls.NewListener(ln, cfg)
		default:
			t.Fatalf("unknown upstream %v", u)
		}
		o.addrs[u] = ln.Addr().String()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = srv.Serve(ln)
		}()
		t.Cleanup(func() {
			o.releaseAll()
			_ = srv.Close()
			<-done
		})
	}
	o.startWire(t)
	o.startBlackhole(t)
	return o
}

// addr returns the loopback address of transport u.
func (o *origin) addr(u upstream) string { return o.addrs[u] }

// newCase returns a fresh exchange ID for caseHeader.
func (o *origin) newCase() string { return "case-" + strconv.FormatInt(o.ids.Add(1), 10) }

// record stores what reached the origin for one case.
func (o *origin) record(id string, s seen) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	s.Attempt = len(o.seen[id]) + 1
	o.seen[id] = append(o.seen[id], s)
	o.signalLocked()
	return s.Attempt
}

// requests returns what reached the origin for case id, in arrival order.
func (o *origin) requests(id string) []seen {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]seen(nil), o.seen[id]...)
}

// only returns the one request of case id, failing t when the origin saw
// another number of them.
func (o *origin) only(t testing.TB, id string) seen {
	t.Helper()
	rs := o.requests(id)
	if len(rs) != 1 {
		t.Fatalf("the origin saw %d requests of %s, want 1", len(rs), id)
	}
	return rs[0]
}

// waitRequests waits until the origin saw n requests of case id, or ctx
// ends.
func (o *origin) waitRequests(ctx context.Context, id string, n int) ([]seen, error) {
	for {
		o.mu.Lock()
		rs, ch := append([]seen(nil), o.seen[id]...), o.changed
		o.mu.Unlock()
		if len(rs) >= n {
			return rs, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return rs, fmt.Errorf("the origin saw %d requests of %s, want %d: %w", len(rs), id, n, ctx.Err())
		}
	}
}

// holdChan returns the release channel of case id.
func (o *origin) holdChan(id string) chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	ch, ok := o.release[id]
	if !ok {
		ch = make(chan struct{})
		o.release[id] = ch
	}
	return ch
}

// releaseCase lets every held request of case id answer.
func (o *origin) releaseCase(id string) {
	ch := o.holdChan(id)
	o.mu.Lock()
	defer o.mu.Unlock()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// releaseAll releases every held request, at cleanup.
func (o *origin) releaseAll() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, ch := range o.release {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

// maxHeld returns the most requests of case id held at once.
func (o *origin) maxHeld(id string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.peak[id]
}

// waitHeld waits until exactly n requests of case id are held, or ctx
// ends.
func (o *origin) waitHeld(ctx context.Context, id string, n int) error {
	for {
		o.mu.Lock()
		held, ch := o.held[id], o.changed
		o.mu.Unlock()
		if held == n {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("%d requests of %s held, want %d: %w", held, id, n, ctx.Err())
		}
	}
}

// signalLocked wakes the waiters of the recorder; the caller holds mu.
func (o *origin) signalLocked() {
	close(o.changed)
	o.changed = make(chan struct{})
}

// startWire starts the raw origin of upWire, which ends with the test:
// the cleanup closes the listener and every open connection and waits for
// their goroutines.
func (o *origin) startWire(t testing.TB) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	o.addrs[upWire] = ln.Addr().String()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		open = map[net.Conn]bool{}
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			open[c] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				o.serveWire(c)
				mu.Lock()
				delete(open, c)
				mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for c := range open {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
}

// startBlackhole starts the listener of upBlackhole: it accepts every
// connection and never answers; the cleanup closes them all.
func (o *origin) startBlackhole(t testing.TB) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	o.addrs[upBlackhole] = ln.Addr().String()
	var (
		mu    sync.Mutex
		conns []net.Conn
		done  = make(chan struct{})
	)
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
}

// wireGuard bounds one connection of the raw origin, a hang guard only.
const wireGuard = 30 * time.Second

// serveWire records one request head as it arrived, reads the body by the
// head's framing, answers 200 "ok" and closes the connection.
func (o *origin) serveWire(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(wireGuard))
	br := bufio.NewReader(c)
	var head strings.Builder
	for {
		line, err := br.ReadString('\n')
		head.WriteString(line)
		if err != nil {
			return
		}
		if line == "\r\n" {
			break
		}
	}
	req, err := http.ReadRequest(bufio.NewReader(io.MultiReader(strings.NewReader(head.String()), br)))
	if err != nil {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(req.Body, 16<<20))
	o.record(req.Header.Get(caseHeader), seen{
		Upstream: upWire, Proto: req.Proto, Method: req.Method, RequestURI: req.RequestURI, Host: req.Host,
		Header: req.Header, Body: body, Trailer: req.Trailer, RawHead: head.String(),
	})
	_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n"+originHeader+": wire\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
}

// headLines returns the field lines of a raw request head, without the
// request line, each as name and value with the name canonicalized.
func headLines(raw string) [][2]string {
	var out [][2]string
	lines := strings.Split(strings.TrimSuffix(raw, "\r\n\r\n"), "\r\n")
	for _, l := range lines[1:] {
		name, value, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		out = append(out, [2]string{http.CanonicalHeaderKey(strings.TrimSpace(name)), strings.TrimSpace(value)})
	}
	return out
}

// fixedDate is the Date an origin response of bDate carries.
const fixedDate = "Mon, 02 Jan 2006 15:04:05 GMT"

// handler returns the origin handler of transport u.
func (o *origin) handler(u upstream) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(caseHeader)
		behavior := path.Base(r.URL.Path)
		s := seen{
			Upstream: u, Proto: r.Proto, Method: r.Method, RequestURI: r.RequestURI, Host: r.Host,
			Header: r.Header.Clone(),
		}
		if r.TLS != nil {
			s.ALPN = r.TLS.NegotiatedProtocol
		}
		w.Header().Set(originHeader, u.String())
		if behavior == bHold {
			o.hold(w, r, id, s)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			s.Body, s.Truncated = body, true
			o.record(id, s)
			panic(http.ErrAbortHandler)
		}
		s.Body, s.Trailer = body, r.Trailer.Clone()
		attempt := o.record(id, s)
		o.respond(w, r, u, behavior, attempt)
	})
}

// hold records the request head, then waits for the case's release or the
// request's end before answering 200.
func (o *origin) hold(w http.ResponseWriter, r *http.Request, id string, s seen) {
	ch := o.holdChan(id)
	o.mu.Lock()
	o.held[id]++
	o.peak[id] = max(o.peak[id], o.held[id])
	o.signalLocked()
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.held[id]--
		o.signalLocked()
		o.mu.Unlock()
	}()
	o.record(id, s)
	select {
	case <-ch:
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "released")
	case <-r.Context().Done():
	}
}

// respond writes the answer of behavior.
func (o *origin) respond(w http.ResponseWriter, r *http.Request, u upstream, behavior string, attempt int) {
	h := w.Header()
	switch behavior {
	case bTrailers:
		h.Set("Trailer", "X-Trailer-Declared")
		h.Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "hello")
		h.Set("X-Trailer-Declared", "declared-value")
		h.Set(http.TrailerPrefix+"X-Trailer-Undeclared", "undeclared-value")
	case bRawTrailers:
		o.rawTrailers(w, u)
	case bConnTrailers:
		h.Set("Connection", "X-Trailer-Hop, X-Trailer-Hop-Late")
		h.Set("Trailer", "X-Trailer-Declared, X-Trailer-Hop")
		h.Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "hello")
		h.Set("X-Trailer-Declared", "declared-value")
		h.Set("X-Trailer-Hop", "hop-value")
		h.Set(http.TrailerPrefix+"X-Trailer-Undeclared", "undeclared-value")
		h.Set(http.TrailerPrefix+"X-Trailer-Hop-Late", "hop-late-value")
	case bHints:
		h.Set("Link", "</style.css>; rel=preload; as=style")
		w.WriteHeader(http.StatusEarlyHints)
		h.Del("Link")
		_, _ = io.WriteString(w, "ok")
	case bHopHints:
		h.Set("Link", "</style.css>; rel=preload; as=style")
		h.Set("Keep-Alive", "timeout=5")
		h.Set("Connection", "X-Hint-Private")
		h.Set("X-Hint-Private", "secret")
		w.WriteHeader(http.StatusEarlyHints)
		for _, f := range []string{"Link", "Keep-Alive", "Connection", "X-Hint-Private"} {
			h.Del(f)
		}
		_, _ = io.WriteString(w, "ok")
	case bInterim:
		w.WriteHeader(http.StatusContinue)
		w.WriteHeader(http.StatusProcessing)
		h.Set("Link", "</app.js>; rel=preload; as=script")
		w.WriteHeader(http.StatusEarlyHints)
		h.Del("Link")
		_, _ = io.WriteString(w, "ok")
	case bDate:
		h.Set("Date", fixedDate)
		h.Set("Server", "conformance-origin")
		_, _ = io.WriteString(w, "ok")
	case bNoDate:
		h["Date"] = nil
		_, _ = io.WriteString(w, "ok")
	case bNotModified:
		h.Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusNotModified)
	case bSized:
		h.Set("Content-Length", "1234")
		h.Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(make([]byte, 1234))
		}
	case bHopByHop:
		h.Set("Connection", "X-Origin-Private")
		h.Set("X-Origin-Private", "secret")
		h.Set("Keep-Alive", "timeout=5")
		h.Set("Proxy-Authenticate", `Basic realm="origin"`)
		h.Set("Proxy-Connection", "keep-alive")
		h.Set("X-Origin-Public", "public")
		_, _ = io.WriteString(w, "ok")
	case bFlaky:
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	case bBig:
		size, _ := strconv.Atoi(r.URL.Query().Get("size"))
		h.Set("Content-Length", strconv.Itoa(size))
		buf := make([]byte, 32<<10)
		for size > 0 {
			n := min(size, len(buf))
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			size -= n
		}
	case bVia:
		h.Set("Via", "1.1 origin-proxy")
		_, _ = io.WriteString(w, "ok")
	default:
		h.Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	}
}

// rawTrailers writes a chunked response by hand whose trailer section
// holds a declared field, an undeclared one, and hop-by-hop and framing
// fields that net/http's server refuses to send: an HTTP/1.1 transport
// only; on HTTP/2 it answers like bTrailers.
func (o *origin) rawTrailers(w http.ResponseWriter, u upstream) {
	hj, ok := w.(http.Hijacker)
	if u == upH2 || !ok {
		h := w.Header()
		h.Set("Trailer", "X-Trailer-Declared")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "hello")
		h.Set("X-Trailer-Declared", "declared-value")
		h.Set(http.TrailerPrefix+"X-Trailer-Undeclared", "undeclared-value")
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	writeRawTrailers(brw.Writer)
}

// writeRawTrailers writes the bRawTrailers response.
func writeRawTrailers(bw *bufio.Writer) {
	_, _ = io.WriteString(bw, "HTTP/1.1 200 OK\r\n"+
		originHeader+": raw\r\n"+
		"Content-Type: text/plain\r\n"+
		"Trailer: X-Trailer-Declared\r\n"+
		"Transfer-Encoding: chunked\r\n"+
		"Connection: close\r\n\r\n"+
		"5\r\nhello\r\n0\r\n"+
		"X-Trailer-Declared: declared-value\r\n"+
		"X-Trailer-Undeclared: undeclared-value\r\n"+
		"Keep-Alive: timeout=5\r\n"+
		"Proxy-Authenticate: Basic\r\n"+
		"Content-Length: 5\r\n"+
		"Trailer: X-Sneaky\r\n\r\n")
	_ = bw.Flush()
}
