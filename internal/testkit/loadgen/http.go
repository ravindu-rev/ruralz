// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/problem"
)

// Protocol is the wire protocol of an HTTP target.
type Protocol int

// Protocols.
const (
	// HTTP1 is HTTP/1.1, over TLS for an https URL.
	HTTP1 Protocol = iota
	// H2C is HTTP/2 over cleartext TCP with prior knowledge.
	H2C
	// H2 is HTTP/2 over TLS, negotiated by ALPN.
	H2
)

// String names the protocol.
func (p Protocol) String() string {
	switch p {
	case HTTP1:
		return "http/1.1"
	case H2C:
		return "h2c"
	case H2:
		return "h2"
	}
	return fmt.Sprintf("Protocol(%d)", int(p))
}

// Connection defaults, from PBB "Scenarios".
const (
	// DefaultHTTP1Connections is S1's 256 keep-alive connections (target).
	DefaultHTTP1Connections = 256
	// DefaultHTTP2Connections is S3's 64 connections (target).
	DefaultHTTP2Connections = 64
	// DefaultHTTP2Streams is S3's up to 64 streams per connection
	// (target).
	DefaultHTTP2Streams = 64
)

// maxProblemBytes bounds the problem document read for its code
// (target); Ruralz problem documents are a few hundred bytes
// (hypothesis).
const maxProblemBytes = 64 << 10

// HTTPConfig configures an HTTP target.
type HTTPConfig struct {
	// URL is the request URL, http or https.
	URL string
	// Method is the request method; default GET.
	Method string
	// Header is added to every request.
	Header http.Header
	// Body is every request's body.
	Body []byte
	// Host, when set, replaces the Host header.
	Host string
	// KeyHeader and Keys set a per-request key header, such as S2's
	// x-bench-key; set both or neither.
	KeyHeader string
	Keys      KeySource
	// Tokens, when set, rotate as "Authorization: Bearer" credentials,
	// request seq taking Tokens[seq % len(Tokens)] (S2 rotates 10,000
	// tokens, target).
	Tokens []string
	// Prepare, when set, adjusts each request last (method, body, query),
	// for example to mix writes into a read load.
	Prepare func(r *http.Request, seq uint64)
	// Protocol selects HTTP/1.1, h2c or HTTP/2 over TLS.
	Protocol Protocol
	// TLS configures https URLs; nil uses the system roots.
	TLS *tls.Config
	// Connections bounds HTTP/1.1 connections (default
	// DefaultHTTP1Connections), or sets the HTTP/2 connections requests
	// spread over (default DefaultHTTP2Connections).
	Connections int
	// StreamsPerConn bounds concurrent streams per HTTP/2 connection;
	// default DefaultHTTP2Streams.
	StreamsPerConn int
	// DisableKeepAlives opens a connection per HTTP/1.1 request.
	DisableKeepAlives bool
	// Dial, when set, dials connections instead of a net.Dialer; it
	// excludes SourceAddrs.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// SourceAddrs, when set, are the local addresses connections dial
	// from, so a scenario that needs per-client source.ip sends from
	// distinct clients (Testing "Load generator and L4 balancer", HTTP):
	// request seq leaves from SourceAddrs[seq % len(SourceAddrs)], on its
	// own share of Connections, and Response.Conn.Local names it. They
	// must be distinct loopback addresses, at most Connections of them.
	// On Linux every 127.0.0.0/8 address is local (LoopbackAddrs); other
	// systems answer only 127.0.0.1 unless the others are aliased onto
	// the loopback interface, and a dial from a missing one fails.
	SourceAddrs []netip.Addr
}

// MaxLoopbackAddrs is how many source addresses LoopbackAddrs gives:
// 127.0.0.1 to 127.0.0.254.
const MaxLoopbackAddrs = 254

// LoopbackAddrs returns the n IPv4 loopback addresses 127.0.0.1 to
// 127.0.0.n for HTTPConfig.SourceAddrs; n is 1 to MaxLoopbackAddrs.
func LoopbackAddrs(n int) ([]netip.Addr, error) {
	if n < 1 || n > MaxLoopbackAddrs {
		return nil, fmt.Errorf("loadgen: %d loopback addresses, want 1 to %d", n, MaxLoopbackAddrs)
	}
	out := make([]netip.Addr, n)
	for i := range out {
		out[i] = netip.AddrFrom4([4]byte{127, 0, 0, byte(i + 1)})
	}
	return out, nil
}

// HTTPTarget sends HTTP requests. It never follows redirects and never
// asks for compression, so each response is the one the server sent.
// Requests spread over HTTP/2 connections by sequence number; with
// SourceAddrs they first spread over the source addresses.
type HTTPTarget struct {
	cfg HTTPConfig
	// groups holds one group of lanes per source address, or one group
	// dialing from the system's choice.
	groups [][]*lane
}

// lane is one connection pool: every HTTP/1.1 connection, or one HTTP/2
// connection with its stream bound.
type lane struct {
	tr     *http.Transport
	client *http.Client
	sem    chan struct{} // nil for HTTP/1.1
}

// NewHTTP returns an HTTP target.
func NewHTTP(c HTTPConfig) (*HTTPTarget, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, fmt.Errorf("loadgen: URL: %w", err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("loadgen: URL %q is not an absolute http or https URL", c.URL)
	}
	switch {
	case c.Protocol == H2C && u.Scheme != "http":
		return nil, errors.New("loadgen: h2c needs an http URL")
	case c.Protocol == H2 && u.Scheme != "https":
		return nil, errors.New("loadgen: HTTP/2 over TLS needs an https URL")
	case c.Protocol < HTTP1 || c.Protocol > H2:
		return nil, fmt.Errorf("loadgen: unknown protocol %v", c.Protocol)
	case (c.KeyHeader == "") != (c.Keys == nil):
		return nil, errors.New("loadgen: set KeyHeader and Keys together")
	case c.Connections < 0 || c.StreamsPerConn < 0:
		return nil, errors.New("loadgen: negative connection or stream bound")
	case c.Protocol == HTTP1 && c.StreamsPerConn != 0:
		return nil, errors.New("loadgen: StreamsPerConn applies to HTTP/2")
	case c.Protocol != HTTP1 && c.DisableKeepAlives:
		return nil, errors.New("loadgen: DisableKeepAlives applies to HTTP/1.1")
	}
	if c.Method == "" {
		c.Method = http.MethodGet
	}
	if _, err := http.NewRequestWithContext(context.Background(), c.Method, c.URL, nil); err != nil {
		return nil, fmt.Errorf("loadgen: %w", err)
	}
	switch {
	case c.Connections == 0 && c.Protocol == HTTP1:
		c.Connections = DefaultHTTP1Connections
	case c.Connections == 0:
		c.Connections = DefaultHTTP2Connections
	}
	if c.StreamsPerConn == 0 && c.Protocol != HTTP1 {
		c.StreamsPerConn = DefaultHTTP2Streams
	}
	sources, err := checkSources(c)
	if err != nil {
		return nil, err
	}
	c.SourceAddrs = sources
	t := &HTTPTarget{cfg: c}
	// Each source address (or the one implicit source) gets an even share
	// of the connections, the first ones one more.
	groups := max(len(sources), 1)
	for g := range groups {
		var src netip.Addr
		if len(sources) > 0 {
			src = sources[g]
		}
		share := c.Connections / groups
		if g < c.Connections%groups {
			share++
		}
		if c.Protocol == HTTP1 {
			t.groups = append(t.groups, []*lane{t.newLane(src, share, 0)})
			continue
		}
		var lanes []*lane
		for range share {
			lanes = append(lanes, t.newLane(src, 1, c.StreamsPerConn))
		}
		t.groups = append(t.groups, lanes)
	}
	return t, nil
}

// checkSources validates c.SourceAddrs and returns them unmapped.
func checkSources(c HTTPConfig) ([]netip.Addr, error) {
	if len(c.SourceAddrs) == 0 {
		return nil, nil
	}
	if c.Dial != nil {
		return nil, errors.New("loadgen: SourceAddrs and Dial are exclusive")
	}
	if len(c.SourceAddrs) > c.Connections {
		return nil, fmt.Errorf("loadgen: %d source addresses for %d connections", len(c.SourceAddrs), c.Connections)
	}
	out := make([]netip.Addr, len(c.SourceAddrs))
	seen := make(map[netip.Addr]bool, len(c.SourceAddrs))
	for i, a := range c.SourceAddrs {
		a = a.Unmap()
		switch {
		case !a.IsValid() || !a.IsLoopback():
			return nil, fmt.Errorf("loadgen: source address %v is not a loopback address", c.SourceAddrs[i])
		case seen[a]:
			return nil, fmt.Errorf("loadgen: source address %v repeats", a)
		}
		seen[a] = true
		out[i] = a
	}
	return out, nil
}

// newLane returns a pool of conns connections (HTTP/1.1) or one HTTP/2
// connection of streams streams, dialing from src when it is valid.
func (t *HTTPTarget) newLane(src netip.Addr, conns, streams int) *lane {
	c := t.cfg
	dial := c.Dial
	if dial == nil {
		d := &net.Dialer{}
		if src.IsValid() {
			d.LocalAddr = &net.TCPAddr{IP: src.AsSlice()}
		}
		dial = d.DialContext
	}
	var protocols http.Protocols
	switch c.Protocol {
	case HTTP1:
		protocols.SetHTTP1(true)
	case H2C:
		protocols.SetUnencryptedHTTP2(true)
	case H2:
		protocols.SetHTTP2(true)
	}
	tr := &http.Transport{
		DialContext:         dial,
		TLSClientConfig:     c.TLS.Clone(),
		DisableKeepAlives:   c.DisableKeepAlives,
		DisableCompression:  true,
		MaxConnsPerHost:     conns,
		MaxIdleConns:        conns,
		MaxIdleConnsPerHost: conns,
		Protocols:           &protocols,
	}
	l := &lane{
		tr: tr,
		client: &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if streams > 0 {
		l.sem = make(chan struct{}, streams)
	}
	return l
}

// Close closes the target's idle connections.
func (t *HTTPTarget) Close() error {
	for _, g := range t.groups {
		for _, l := range g {
			l.tr.CloseIdleConnections()
		}
	}
	return nil
}

// lane returns the lane of request seq: the group of source address
// seq % groups, then its lanes in turn.
func (t *HTTPTarget) lane(seq uint64) *lane {
	n := uint64(len(t.groups))
	g := t.groups[seq%n]
	return g[(seq/n)%uint64(len(g))]
}

// connCapture records the connection a request got.
type connCapture struct {
	mu   sync.Mutex
	info ConnInfo
}

func (c *connCapture) got(i httptrace.GotConnInfo) {
	ci := ConnInfo{Reused: i.Reused}
	if i.Conn != nil {
		ci.Local, ci.Remote = i.Conn.LocalAddr().String(), i.Conn.RemoteAddr().String()
	}
	c.mu.Lock()
	c.info = ci
	c.mu.Unlock()
}

func (c *connCapture) get() ConnInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Do sends request r and reads the whole response body.
func (t *HTTPTarget) Do(ctx context.Context, r Request) Response {
	l := t.lane(r.Seq)
	var resp Response
	if t.cfg.Keys != nil {
		resp.Key = t.cfg.Keys.Key(r.Seq)
	}
	if l.sem != nil {
		select {
		case l.sem <- struct{}{}:
			defer func() { <-l.sem }()
		case <-ctx.Done():
			resp.Err = context.Cause(ctx)
			resp.Outcome = Classify(resp.Err)
			return resp
		}
	}
	var cc connCapture
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: cc.got})
	req, err := t.newRequest(ctx, r.Seq, resp.Key)
	if err != nil {
		resp.Err, resp.Outcome = err, Other
		return resp
	}
	res, err := l.client.Do(req)
	resp.Conn = cc.get()
	if err != nil {
		resp.Err = err
		resp.Outcome = Classify(err)
		return resp
	}
	defer func() { _ = res.Body.Close() }()
	resp.Status, resp.Proto, resp.Header = res.StatusCode, res.Proto, res.Header
	resp.Outcome = OutcomeForStatus(res.StatusCode)
	body := io.Reader(res.Body)
	if res.StatusCode >= 400 && isProblem(res.Header.Get("Content-Type")) {
		head, err := io.ReadAll(io.LimitReader(res.Body, maxProblemBytes))
		resp.Bytes += int64(len(head))
		if err != nil {
			return failBody(resp, err)
		}
		if doc, err := problem.Decode(head); err == nil {
			resp.Code = doc.Code
		}
	}
	n, err := io.Copy(io.Discard, body)
	resp.Bytes += n
	if err != nil {
		return failBody(resp, err)
	}
	return resp
}

// failBody marks a response whose body broke off.
func failBody(resp Response, err error) Response {
	resp.Err = err
	if resp.Outcome = Classify(err); resp.Outcome == Success {
		resp.Outcome = Other
	}
	return resp
}

func isProblem(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == problem.ContentType
}

func (t *HTTPTarget) newRequest(ctx context.Context, seq uint64, key string) (*http.Request, error) {
	c := t.cfg
	var body io.Reader
	if c.Body != nil {
		body = bytes.NewReader(c.Body)
	}
	req, err := http.NewRequestWithContext(ctx, c.Method, c.URL, body)
	if err != nil {
		return nil, fmt.Errorf("loadgen: %w", err)
	}
	if c.Header != nil {
		req.Header = c.Header.Clone()
	}
	if c.Host != "" {
		req.Host = c.Host
	}
	if c.Keys != nil {
		req.Header.Set(c.KeyHeader, key)
	}
	if len(c.Tokens) > 0 {
		req.Header.Set("Authorization", "Bearer "+c.Tokens[seq%uint64(len(c.Tokens))])
	}
	if c.Prepare != nil {
		c.Prepare(req, seq)
	}
	return req, nil
}
