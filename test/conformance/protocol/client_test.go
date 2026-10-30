// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package protocol

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// proto is a client protocol of the wire rules
// (docs/architecture/03-data-plane.md "Listeners and protocols").
type proto int

// Client protocols.
const (
	// h1 is HTTP/1.1 in cleartext on the http listener.
	h1 proto = iota
	// h1TLS is HTTP/1.1 over TLS on the https listener, negotiated by ALPN
	// http/1.1.
	h1TLS
	// h2c is HTTP/2 in cleartext by prior knowledge on the http listener.
	h2c
	// h2TLS is HTTP/2 over TLS on the https listener, negotiated by ALPN
	// h2.
	h2TLS
)

// protos returns every client protocol.
func protos() []proto { return []proto{h1, h1TLS, h2c, h2TLS} }

// String names the protocol.
func (p proto) String() string {
	switch p {
	case h1:
		return "h1"
	case h1TLS:
		return "h1tls"
	case h2c:
		return "h2c"
	case h2TLS:
		return "h2"
	default:
		return "proto(" + strconv.Itoa(int(p)) + ")"
	}
}

// isH2 reports an HTTP/2 protocol.
func (p proto) isH2() bool { return p == h2c || p == h2TLS }

// isTLS reports a protocol over TLS.
func (p proto) isTLS() bool { return p == h1TLS || p == h2TLS }

// scheme returns the listener's scheme.
func (p proto) scheme() string {
	if p.isTLS() {
		return "https"
	}
	return "http"
}

// via returns the Via entry the client hop adds on HTTP/1.1 or HTTP/2.
func (p proto) via() string {
	if p.isH2() {
		return "2 ruralz"
	}
	return "1.1 ruralz"
}

// Listener names of the suite's Bundles.
const (
	// plainListener is the http listener.
	plainListener = "web"
	// secureListener is the https listener.
	secureListener = "secure"
)

// gateway is one Node of a test and the means to reach it: its listener
// addresses, a client TLS configuration trusting the harness's CA, and
// the origin behind it.
type gateway struct {
	top    *topology.Topology
	node   *topology.Node
	origin *origin
	tls    *tls.Config
	creds  *credentials
}

// addr returns the loopback address of the listener p reaches.
func (g *gateway) addr(p proto) string {
	if p.isTLS() {
		return g.node.Addr(secureListener)
	}
	return g.node.Addr(plainListener)
}

// request is one client request, written by the protocol's own rules.
type request struct {
	// Method defaults to GET.
	Method string
	// Target is the request target: origin-form, "*" or, on HTTP/1.x,
	// absolute-form. On HTTP/2 it is :path.
	Target string
	// Host is the Host field on HTTP/1.x and :authority on HTTP/2;
	// default the listener address.
	Host string
	// Header holds the fields in order; names are lowercased on HTTP/2.
	Header [][2]string
	// Body is sent with a declared length unless Chunked.
	Body []byte
	// Trailer is sent after the body, which it frames without a declared
	// length.
	Trailer [][2]string
	// Chunked sends the body without a declared length: chunked on
	// HTTP/1.1, without content-length on HTTP/2.
	Chunked bool
	// HTTP10 sends an HTTP/1.0 request on an HTTP/1.x protocol.
	HTTP10 bool
	// NoHost sends no Host field on HTTP/1.x.
	NoHost bool
	// Pseudo replaces the HTTP/2 pseudo-header fields when set.
	Pseudo [][2]string
}

// method returns the request method.
func (r request) method() string {
	if r.Method == "" {
		return http.MethodGet
	}
	return r.Method
}

// chunked reports a body framed without a declared length.
func (r request) chunked() bool { return r.Chunked || len(r.Trailer) > 0 }

// h1Bytes serializes r as an HTTP/1.x request to host.
func (r request) h1Bytes(host string) []byte {
	var b bytes.Buffer
	ver := "HTTP/1.1"
	if r.HTTP10 {
		ver = "HTTP/1.0"
	}
	b.WriteString(r.method() + " " + r.Target + " " + ver + "\r\n")
	if !r.NoHost {
		if r.Host != "" {
			host = r.Host
		}
		b.WriteString("Host: " + host + "\r\n")
	}
	for _, f := range r.Header {
		b.WriteString(f[0] + ": " + f[1] + "\r\n")
	}
	switch {
	case r.chunked():
		b.WriteString("Transfer-Encoding: chunked\r\n")
	case r.Body != nil:
		b.WriteString("Content-Length: " + strconv.Itoa(len(r.Body)) + "\r\n")
	}
	b.WriteString("\r\n")
	b.Write(r.h1Body())
	return b.Bytes()
}

// h1Head serializes the request head alone, for Expect: 100-continue.
func (r request) h1Head(host string) []byte {
	full := r.h1Bytes(host)
	i := bytes.Index(full, []byte("\r\n\r\n"))
	return full[:i+4]
}

// h1Body serializes the body and trailer section.
func (r request) h1Body() []byte {
	if !r.chunked() {
		return r.Body
	}
	var b bytes.Buffer
	if len(r.Body) > 0 {
		b.WriteString(strconv.FormatInt(int64(len(r.Body)), 16) + "\r\n")
		b.Write(r.Body)
		b.WriteString("\r\n")
	}
	b.WriteString("0\r\n")
	for _, f := range r.Trailer {
		b.WriteString(f[0] + ": " + f[1] + "\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// h2Fields returns the HTTP/2 header list of r: the pseudo-header fields,
// then the regular fields lowercased, then content-length for a body sent
// with a declared length.
func (r request) h2Fields(p proto, authority string) [][2]string {
	var out [][2]string
	if r.Pseudo != nil {
		out = append(out, r.Pseudo...)
	} else {
		if r.Host != "" {
			authority = r.Host
		}
		out = append(out, [2]string{":method", r.method()}, [2]string{":scheme", p.scheme()},
			[2]string{":authority", authority}, [2]string{":path", r.Target})
	}
	for _, f := range r.Header {
		out = append(out, [2]string{strings.ToLower(f[0]), f[1]})
	}
	if r.Body != nil && !r.chunked() {
		out = append(out, [2]string{"content-length", strconv.Itoa(len(r.Body))})
	}
	return out
}

// interim is one 1xx response.
type interim struct {
	Status int
	Header http.Header
}

// response is what the client saw of one exchange.
type response struct {
	// Proto is the response's protocol: HTTP/1.0, HTTP/1.1 or HTTP/2.0.
	Proto   string
	Interim []interim
	// Status is 0 when no final response arrived.
	Status  int
	Header  http.Header
	Body    []byte
	Trailer http.Header
	// Close reports an HTTP/1.x response that carried Connection: close,
	// which http.ReadResponse takes out of Header.
	Close bool
	// Reset is the RST_STREAM code of an HTTP/2 stream the server reset.
	Reset     http2.ErrCode
	WasReset  bool
	GoAway    http2.ErrCode
	SawGoAway bool
	// Broken is the read error that ended the exchange before a whole
	// response: an EOF, a reset or a deadline.
	Broken error
}

// String sums the response up for a failure message.
func (r *response) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d", r.Proto, r.Status)
	for _, i := range r.Interim {
		fmt.Fprintf(&b, " [1xx %d]", i.Status)
	}
	if r.WasReset {
		fmt.Fprintf(&b, " RST_STREAM %v", r.Reset)
	}
	if r.SawGoAway {
		fmt.Fprintf(&b, " GOAWAY %v", r.GoAway)
	}
	if r.Broken != nil {
		fmt.Fprintf(&b, " broken: %v", r.Broken)
	}
	if len(r.Body) > 0 {
		body := r.Body
		if len(body) > 300 {
			body = body[:300]
		}
		fmt.Fprintf(&b, " body %q", body)
	}
	return b.String()
}

// problem is an RFC 9457 problem document with the Ruralz members
// (docs/architecture/03-data-plane.md "Error response format").
type problem struct {
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Code      string `json:"code"`
	RequestID string `json:"requestId"`
}

// isProblem reports a problem document response.
func (r *response) isProblem() bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/problem+json"
}

// wantProblem fails t unless r is a problem document of status and code.
func wantProblem(t testing.TB, r *response, status int, code string) {
	t.Helper()
	if r.Status != status || !r.isProblem() {
		t.Fatalf("response %v, want a %d problem document with %s", r, status, code)
	}
	var doc problem
	if err := json.Unmarshal(r.Body, &doc); err != nil {
		if len(r.Body) == 0 {
			// A response to HEAD carries the header section only.
			return
		}
		t.Fatalf("problem document %q: %v", r.Body, err)
	}
	if doc.Code != code || doc.Status != status || len(doc.RequestID) != 32 {
		t.Fatalf("problem document %+v, want status %d, code %s and a 32-hex requestId", doc, status, code)
	}
}

// wantPlain fails t unless r is a status answered by net/http itself,
// before any handler: no problem document.
func wantPlain(t testing.TB, r *response, status int) {
	t.Helper()
	if r.Status != status || r.isProblem() {
		t.Fatalf("response %v (%s), want net/http's plain %d", r, r.Header.Get("Content-Type"), status)
	}
}

// wantOK fails t unless r is the origin's 200.
func wantOK(t testing.TB, r *response) {
	t.Helper()
	if r.Status != http.StatusOK || r.Header.Get(originHeader) == "" {
		t.Fatalf("response %v, want the origin's 200", r)
	}
}

// do sends req over a new connection of protocol p and returns the
// response, which it hands to the teardown leak scan (capture).
func (g *gateway) do(t testing.TB, p proto, req request) *response {
	t.Helper()
	ctx := guard(t, exchangeGuard)
	var r *response
	if p.isH2() {
		c := g.dialH2(t, p)
		defer c.close()
		r = c.roundTrip(ctx, p, req)
	} else {
		c := g.dialH1(t, p)
		defer c.close()
		r = c.exchange(ctx, req.method(), req.h1Bytes(g.addr(p)))
	}
	g.capture(r)
	return r
}

// capture hands every header section and the body of r to the topology,
// whose teardown scan fails the test when a response echoes a canary,
// such as a Consumer credential the request carried.
func (g *gateway) capture(r *response) {
	var b bytes.Buffer
	for _, i := range r.Interim {
		_ = i.Header.Write(&b)
	}
	_ = r.Header.Write(&b)
	b.Write(r.Body)
	_ = r.Trailer.Write(&b)
	g.top.Capture("client", b.Bytes())
}

// dialTimeout bounds a dial of the suite's clients.
const dialTimeout = 10 * time.Second

// dial opens a TCP connection to the listener of p, TLS with the ALPN
// protocols alpn on a TLS protocol.
func (g *gateway) dial(t testing.TB, p proto, alpn ...string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), dialTimeout)
	defer cancel()
	nd := &net.Dialer{}
	if !p.isTLS() {
		c, err := nd.DialContext(ctx, "tcp", g.addr(p))
		if err != nil {
			t.Fatalf("dial %v: %v", p, err)
		}
		return c
	}
	cfg := g.tls.Clone()
	cfg.NextProtos = alpn
	d := &tls.Dialer{NetDialer: nd, Config: cfg}
	c, err := d.DialContext(ctx, "tcp", g.addr(p))
	if err != nil {
		t.Fatalf("dial %v: %v", p, err)
	}
	if got := c.(*tls.Conn).ConnectionState().NegotiatedProtocol; len(alpn) > 0 && got != alpn[0] {
		_ = c.Close()
		t.Fatalf("ALPN negotiated %q, want %q", got, alpn[0])
	}
	return c
}

// h1Conn is one HTTP/1.x client connection.
type h1Conn struct {
	t  testing.TB
	c  net.Conn
	br *bufio.Reader
}

// dialH1 opens an HTTP/1.x connection to the listener of p.
func (g *gateway) dialH1(t testing.TB, p proto) *h1Conn {
	t.Helper()
	c := g.dial(t, p, "http/1.1")
	return &h1Conn{t: t, c: c, br: bufio.NewReader(c)}
}

// close closes the connection.
func (c *h1Conn) close() { _ = c.c.Close() }

// write sends raw bytes, failing the test on an error.
func (c *h1Conn) write(b []byte) {
	c.t.Helper()
	if _, err := c.c.Write(b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// exchange writes raw, while it reads the response to a request of method,
// so a server that answers before reading the whole request never blocks
// the writer.
func (c *h1Conn) exchange(ctx context.Context, method string, raw []byte) *response {
	go func() { _, _ = c.c.Write(raw) }()
	return c.read(ctx, method)
}

// readOne reads one response head of a request of method: a 1xx, or the
// final response whose body the caller reads.
func (c *h1Conn) readOne(ctx context.Context, method string) (*http.Response, error) {
	if d, ok := ctx.Deadline(); ok {
		_ = c.c.SetReadDeadline(d)
	}
	return http.ReadResponse(c.br, &http.Request{Method: method})
}

// read reads every interim response, then the final response with its
// body and trailer section.
func (c *h1Conn) read(ctx context.Context, method string) *response {
	out := &response{}
	for {
		resp, err := c.readOne(ctx, method)
		if err != nil {
			out.Broken = err
			return out
		}
		out.Proto = resp.Proto
		if resp.StatusCode >= 100 && resp.StatusCode < 200 && resp.StatusCode != http.StatusSwitchingProtocols {
			out.Interim = append(out.Interim, interim{Status: resp.StatusCode, Header: resp.Header})
			continue
		}
		out.Status, out.Header, out.Close = resp.StatusCode, resp.Header, resp.Close
		out.Body, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			out.Broken = err
		}
		out.Trailer = resp.Trailer
		return out
	}
}

// waitClosed reports whether the server closed the connection within d,
// reading and discarding what arrives meanwhile, and how long it took.
func (c *h1Conn) waitClosed(d time.Duration) (bool, time.Duration) {
	start := time.Now()
	_ = c.c.SetReadDeadline(start.Add(d))
	_, err := io.Copy(io.Discard, c.br)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false, time.Since(start)
	}
	return true, time.Since(start)
}

// h2Event is one frame the server sent, decoded and copied, or the read
// error that ended the connection.
type h2Event struct {
	Type      http2.FrameType
	StreamID  uint32
	EndStream bool
	// Fields are a HEADERS frame's decoded fields, CONTINUATION merged.
	Fields []hpack.HeaderField
	Data   []byte
	Code   http2.ErrCode
	// LastStreamID is a GOAWAY's.
	LastStreamID uint32
	Settings     []http2.Setting
	IsAck        bool
	PingData     [8]byte
	Increment    uint32
	Err          error
	At           time.Time
}

// status returns the :status of a HEADERS event, 0 for none.
func (e *h2Event) status() int {
	for _, f := range e.Fields {
		if f.Name == ":status" {
			n, _ := strconv.Atoi(f.Value)
			return n
		}
	}
	return 0
}

// header returns the regular fields of a HEADERS event.
func (e *h2Event) header() http.Header {
	h := http.Header{}
	for _, f := range e.Fields {
		if !strings.HasPrefix(f.Name, ":") {
			h.Add(f.Name, f.Value)
		}
	}
	return h
}

// Client settings of the suite's HTTP/2 connections.
const (
	// h2ClientWindow is the receive window the client advertises, per
	// stream and per connection, so responses never wait for it.
	h2ClientWindow = 1 << 30
	// h2DefaultWindow is the window of a new connection or stream
	// (RFC 9113 section 6.9.2).
	h2DefaultWindow = 65535
	// h2EventBuffer bounds the frames the reader queues for the test.
	h2EventBuffer = 1 << 16
)

// h2Conn is one HTTP/2 client connection on the Framer. A reader goroutine
// decodes every frame into an event, answers SETTINGS and PING, and tracks
// the send windows; the test reads the events in order.
type h2Conn struct {
	t      testing.TB
	c      net.Conn
	fr     *http2.Framer
	events chan h2Event
	done   chan struct{}
	exited chan struct{}
	// preface is closed once the client preface is written: over TLS the
	// server sends its SETTINGS first, and the reader's acknowledgment
	// must not precede the preface (RFC 9113 section 3.4).
	preface chan struct{}

	wmu    sync.Mutex
	encBuf bytes.Buffer
	enc    *hpack.Encoder

	// settings are the server's first SETTINGS, and connIncrement the
	// increment of its first connection WINDOW_UPDATE, which the reader
	// records once.
	settings      map[http2.SettingID]uint32
	connIncrement atomic.Uint32
	pending       []h2Event
	nextID        uint32

	flow *flowWindows
}

// h2Options configure an HTTP/2 connection.
type h2Options struct {
	// Settings replace the client's SETTINGS when set.
	Settings []http2.Setting
	// NoWindowUpdate skips the client's connection WINDOW_UPDATE.
	NoWindowUpdate bool
	// NoSettingsAck leaves the server's SETTINGS unacknowledged.
	NoSettingsAck bool
}

// dialH2 opens an HTTP/2 connection to the listener of p, h2c by prior
// knowledge or h2 by ALPN, sends the preface, the client's SETTINGS and a
// connection WINDOW_UPDATE, and waits for the server's SETTINGS.
func (g *gateway) dialH2(t testing.TB, p proto, opts ...h2Options) *h2Conn {
	t.Helper()
	var o h2Options
	if len(opts) > 0 {
		o = opts[0]
	}
	c := newH2Conn(t, g.dial(t, p, "h2"), o)
	t.Cleanup(c.close)
	c.handshake(o)
	return c
}

// newH2Conn starts the reader of an HTTP/2 connection over nc.
func newH2Conn(t testing.TB, nc net.Conn, o h2Options) *h2Conn {
	c := &h2Conn{
		t: t, c: nc, events: make(chan h2Event, h2EventBuffer), done: make(chan struct{}), exited: make(chan struct{}),
		preface: make(chan struct{}), nextID: 1, flow: newFlowWindows(),
	}
	c.enc = hpack.NewEncoder(&c.encBuf)
	c.fr = http2.NewFramer(nc, nc)
	c.fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	c.fr.MaxHeaderListSize = 16 << 20
	c.fr.SetMaxReadFrameSize(1 << 24)
	go c.readLoop(!o.NoSettingsAck)
	return c
}

// handshake writes the preface and the client's settings, then waits for
// the server's SETTINGS.
func (c *h2Conn) handshake(o h2Options) {
	c.t.Helper()
	settings := o.Settings
	if settings == nil {
		settings = []http2.Setting{
			{ID: http2.SettingEnablePush, Val: 0},
			{ID: http2.SettingInitialWindowSize, Val: h2ClientWindow},
		}
	}
	c.wmu.Lock()
	_, err := io.WriteString(c.c, http2.ClientPreface)
	if err == nil {
		err = c.fr.WriteSettings(settings...)
	}
	if err == nil && !o.NoWindowUpdate {
		err = c.fr.WriteWindowUpdate(0, h2ClientWindow-h2DefaultWindow)
	}
	c.wmu.Unlock()
	close(c.preface)
	if err != nil {
		c.t.Fatalf("HTTP/2 preface: %v", err)
	}
	ctx := guard(c.t, exchangeGuard)
	for c.settings == nil {
		ev, ok := c.nextRaw(ctx)
		if !ok {
			c.t.Fatalf("no SETTINGS from the server: %v", ev.Err)
		}
		if ev.Type == http2.FrameSettings && !ev.IsAck {
			c.settings = map[http2.SettingID]uint32{}
			for _, s := range ev.Settings {
				c.settings[s.ID] = s.Val
			}
			continue
		}
		c.pending = append(c.pending, ev)
	}
}

// ping sends a PING and waits for its acknowledgment: the server answers
// frames in order, so every frame it sent before reading the PING has
// arrived. Other events stay for the test.
func (c *h2Conn) ping(ctx context.Context) error {
	data := [8]byte{'c', 'o', 'n', 'f', 'o', 'r', 'm', 'x'}
	if err := c.writeFrames(func(fr *http2.Framer) error { return fr.WritePing(false, data) }); err != nil {
		return err
	}
	var kept []h2Event
	defer func() { c.pending = append(kept, c.pending...) }()
	for {
		ev, ok := c.next(ctx)
		if !ok {
			return fmt.Errorf("no PING acknowledgment: %w", ev.Err)
		}
		if ev.Type == http2.FramePing && ev.IsAck && ev.PingData == data {
			return nil
		}
		kept = append(kept, ev)
	}
}

// close ends the connection and waits for its reader.
func (c *h2Conn) close() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	_ = c.c.Close()
	<-c.exited
}

// readLoop decodes frames until the connection ends.
func (c *h2Conn) readLoop(ackSettings bool) {
	defer close(c.exited)
	for {
		f, err := c.fr.ReadFrame()
		ev := h2Event{Err: err, At: time.Now()}
		if err == nil {
			ev = c.decode(f, ackSettings)
		}
		if err != nil {
			c.flow.fail()
		}
		select {
		case c.events <- ev:
		case <-c.done:
			return
		}
		if err != nil {
			return
		}
	}
}

// decode copies frame f into an event, answers SETTINGS and PING and
// feeds the send windows.
func (c *h2Conn) decode(f http2.Frame, ackSettings bool) h2Event {
	fh := f.Header()
	ev := h2Event{Type: fh.Type, StreamID: fh.StreamID, At: time.Now()}
	switch f := f.(type) {
	case *http2.MetaHeadersFrame:
		ev.Type = http2.FrameHeaders
		ev.Fields = append([]hpack.HeaderField(nil), f.Fields...)
		ev.EndStream = f.StreamEnded()
		if ev.EndStream {
			c.flow.closeStream(ev.StreamID)
		}
	case *http2.DataFrame:
		ev.Data = bytes.Clone(f.Data())
		ev.EndStream = f.StreamEnded()
		if ev.EndStream {
			c.flow.closeStream(ev.StreamID)
		}
	case *http2.RSTStreamFrame:
		ev.Code = f.ErrCode
		c.flow.closeStream(ev.StreamID)
	case *http2.GoAwayFrame:
		ev.Code, ev.LastStreamID = f.ErrCode, f.LastStreamID
	case *http2.SettingsFrame:
		ev.IsAck = f.IsAck()
		_ = f.ForeachSetting(func(s http2.Setting) error {
			ev.Settings = append(ev.Settings, s)
			if s.ID == http2.SettingInitialWindowSize {
				c.flow.setInitial(int64(s.Val))
			}
			return nil
		})
		if !ev.IsAck && ackSettings && c.afterPreface() {
			c.wmu.Lock()
			_ = c.fr.WriteSettingsAck()
			c.wmu.Unlock()
		}
	case *http2.PingFrame:
		ev.IsAck, ev.PingData = f.IsAck(), f.Data
		if !ev.IsAck && c.afterPreface() {
			c.wmu.Lock()
			_ = c.fr.WritePing(true, f.Data)
			c.wmu.Unlock()
		}
	case *http2.WindowUpdateFrame:
		ev.Increment = f.Increment
		if ev.StreamID == 0 {
			c.connIncrement.CompareAndSwap(0, f.Increment)
		}
		c.flow.add(ev.StreamID, int64(f.Increment))
	}
	return ev
}

// afterPreface waits until the client preface is written, false when the
// connection ends first.
func (c *h2Conn) afterPreface() bool {
	select {
	case <-c.preface:
		return true
	case <-c.done:
		return false
	}
}

// nextRaw returns the next event from the reader, ok false when ctx
// ended or the connection's last event was read.
func (c *h2Conn) nextRaw(ctx context.Context) (h2Event, bool) {
	select {
	case ev, ok := <-c.events:
		if !ok {
			return h2Event{Err: io.EOF}, false
		}
		return ev, ev.Err == nil
	case <-ctx.Done():
		return h2Event{Err: ctx.Err()}, false
	}
}

// next returns the next event, the pending ones first.
func (c *h2Conn) next(ctx context.Context) (h2Event, bool) {
	if len(c.pending) > 0 {
		ev := c.pending[0]
		c.pending = c.pending[1:]
		return ev, ev.Err == nil
	}
	return c.nextRaw(ctx)
}

// newStream returns the next client stream ID.
func (c *h2Conn) newStream() uint32 {
	id := c.nextID
	c.nextID += 2
	return id
}

// encode returns the HPACK block of fields.
func (c *h2Conn) encode(fields [][2]string) []byte {
	c.encBuf.Reset()
	for _, f := range fields {
		_ = c.enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	return bytes.Clone(c.encBuf.Bytes())
}

// h2MaxFrame is the largest frame payload the client writes, the protocol
// minimum every peer accepts (RFC 9113 section 4.2).
const h2MaxFrame = 16384

// writeHeaders writes fields as a HEADERS frame and as many CONTINUATION
// frames as the block needs.
func (c *h2Conn) writeHeaders(id uint32, fields [][2]string, endStream bool) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeBlockLocked(id, c.encode(fields), endStream)
}

// writeBlock writes a raw HPACK block as HEADERS and CONTINUATION frames
// of at most h2MaxFrame bytes.
func (c *h2Conn) writeBlock(id uint32, block []byte, endStream bool) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeBlockLocked(id, block, endStream)
}

func (c *h2Conn) writeBlockLocked(id uint32, block []byte, endStream bool) error {
	first := block[:min(len(block), h2MaxFrame)]
	rest := block[len(first):]
	if err := c.fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: id, BlockFragment: first, EndStream: endStream, EndHeaders: len(rest) == 0,
	}); err != nil {
		return err
	}
	for len(rest) > 0 {
		frag := rest[:min(len(rest), h2MaxFrame)]
		rest = rest[len(frag):]
		if err := c.fr.WriteContinuation(id, len(rest) == 0, frag); err != nil {
			return err
		}
	}
	return nil
}

// writeData writes one DATA frame as it is, without flow control.
func (c *h2Conn) writeData(id uint32, endStream bool, data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.fr.WriteData(id, endStream, data)
}

// writeRST resets a stream.
func (c *h2Conn) writeRST(id uint32, code http2.ErrCode) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.fr.WriteRSTStream(id, code)
}

// writeFrames runs write under the write lock, for a batch of frames.
func (c *h2Conn) writeFrames(write func(fr *http2.Framer) error) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return write(c.fr)
}

// sendBody writes body as DATA frames within the server's flow-control
// windows, then trailer as a HEADERS frame; the last frame ends the
// stream. It stops when ctx ends or the server closes the stream.
func (c *h2Conn) sendBody(ctx context.Context, id uint32, body []byte, trailer [][2]string) error {
	for len(body) > 0 {
		n, err := c.flow.take(ctx, id, min(len(body), h2MaxFrame))
		if err != nil {
			return err
		}
		chunk := body[:n]
		body = body[n:]
		if err := c.writeData(id, len(body) == 0 && len(trailer) == 0, chunk); err != nil {
			return err
		}
	}
	if len(trailer) > 0 {
		return c.writeHeaders(id, lowerFields(trailer), true)
	}
	return nil
}

// lowerFields lowercases the field names, as HTTP/2 requires.
func lowerFields(fields [][2]string) [][2]string {
	out := make([][2]string, len(fields))
	for i, f := range fields {
		out[i] = [2]string{strings.ToLower(f[0]), f[1]}
	}
	return out
}

// roundTrip sends req on a new stream and collects its response.
func (c *h2Conn) roundTrip(ctx context.Context, p proto, req request) *response {
	id := c.newStream()
	if err := c.send(ctx, p, id, req); err != nil {
		return &response{Proto: "HTTP/2.0", Broken: err}
	}
	return c.collect(ctx, id)
}

// send writes the request head of req on stream id and starts its body
// and trailer section, which a goroutine writes within the flow-control
// windows until the stream closes.
func (c *h2Conn) send(ctx context.Context, p proto, id uint32, req request) error {
	fields := req.h2Fields(p, c.c.RemoteAddr().String())
	end := req.Body == nil && len(req.Trailer) == 0
	if err := c.writeHeaders(id, fields, end); err != nil {
		return err
	}
	if !end {
		go func() { _ = c.sendBody(ctx, id, req.Body, req.Trailer) }()
	}
	return nil
}

// collect reads the events of stream id until its response ends: the
// interim responses, the final response, its DATA and trailer section, or
// a reset, a GOAWAY that excludes the stream, or the connection's end.
func (c *h2Conn) collect(ctx context.Context, id uint32) *response {
	out := &response{Proto: "HTTP/2.0"}
	for {
		ev, ok := c.next(ctx)
		if !ok {
			out.Broken = ev.Err
			return out
		}
		switch {
		case ev.Type == http2.FrameGoAway:
			out.SawGoAway, out.GoAway = true, ev.Code
			if ev.LastStreamID < id {
				out.Broken = fmt.Errorf("GOAWAY %v excludes stream %d (last %d)", ev.Code, id, ev.LastStreamID)
				return out
			}
			continue
		case ev.StreamID != id:
			continue
		}
		switch ev.Type {
		case http2.FrameHeaders:
			st := ev.status()
			switch {
			case out.Status == 0 && st >= 100 && st < 200:
				out.Interim = append(out.Interim, interim{Status: st, Header: ev.header()})
			case out.Status == 0:
				out.Status, out.Header = st, ev.header()
			default:
				out.Trailer = ev.header()
			}
		case http2.FrameData:
			out.Body = append(out.Body, ev.Data...)
		case http2.FrameRSTStream:
			out.WasReset, out.Reset = true, ev.Code
			return out
		default:
		}
		if ev.EndStream {
			return out
		}
	}
}

// waitEvent reads events until match accepts one, which it returns; ok
// is false when ctx ended or the connection ended first.
func (c *h2Conn) waitEvent(ctx context.Context, match func(h2Event) bool) (h2Event, bool) {
	for {
		ev, ok := c.next(ctx)
		if !ok {
			return ev, false
		}
		if match(ev) {
			return ev, true
		}
	}
}

// flowWindows tracks the client's send windows: the connection's and each
// stream's, from the server's SETTINGS and WINDOW_UPDATE frames.
type flowWindows struct {
	mu      sync.Mutex
	changed chan struct{}
	conn    int64
	initial int64
	streams map[uint32]int64
	closed  map[uint32]bool
	failed  bool
}

func newFlowWindows() *flowWindows {
	return &flowWindows{
		changed: make(chan struct{}), conn: h2DefaultWindow, initial: h2DefaultWindow,
		streams: map[uint32]int64{}, closed: map[uint32]bool{},
	}
}

// signal wakes the waiters; the caller holds mu.
func (f *flowWindows) signal() {
	close(f.changed)
	f.changed = make(chan struct{})
}

// add applies a WINDOW_UPDATE.
func (f *flowWindows) add(id uint32, inc int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == 0 {
		f.conn += inc
	} else {
		f.streams[id] = f.window(id) + inc
	}
	f.signal()
}

// window returns stream id's window; the caller holds mu.
func (f *flowWindows) window(id uint32) int64 {
	if w, ok := f.streams[id]; ok {
		return w
	}
	return f.initial
}

// setInitial applies a SETTINGS_INITIAL_WINDOW_SIZE change to every
// stream (RFC 9113 section 6.9.2).
func (f *flowWindows) setInitial(v int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delta := v - f.initial
	f.initial = v
	for id, w := range f.streams {
		f.streams[id] = w + delta
	}
	f.signal()
}

// closeStream marks a stream the server closed.
func (f *flowWindows) closeStream(id uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed[id] = true
	f.signal()
}

// fail wakes every waiter at the connection's end.
func (f *flowWindows) fail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = true
	f.signal()
}

// errStreamClosed reports a body write the server's close cut short.
var errStreamClosed = errors.New("the server closed the stream")

// take waits until both windows allow at least one byte and takes up to n
// bytes from them.
func (f *flowWindows) take(ctx context.Context, id uint32, n int) (int, error) {
	for {
		f.mu.Lock()
		if f.failed || f.closed[id] {
			f.mu.Unlock()
			return 0, errStreamClosed
		}
		avail := min(int64(n), f.conn, f.window(id))
		if avail > 0 {
			f.conn -= avail
			f.streams[id] = f.window(id) - avail
			f.mu.Unlock()
			return int(avail), nil
		}
		ch := f.changed
		f.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}
