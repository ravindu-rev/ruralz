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
	// RoundRobin spreads HTTP/1.1 requests over Connections in turn, as
	// HTTP/2 requests spread, each connection carrying one request at a
	// time, so a light load still uses every connection: behind an L4
	// balancer, which balances connections, it reaches every backend. A
	// request takes the next connection in turn that is free and waits
	// only while every connection is busy, so a connection that never
	// answers holds only its own request (Testing "Load generator and L4
	// balancer", HTTP). Without it Connections bounds a pool that takes
	// its most recently used idle connection first, so a light load keeps
	// few connections busy.
	RoundRobin bool
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
// Requests spread over HTTP/2 connections, and with RoundRobin over
// HTTP/1.1 connections, in turn: each takes the next connection that is
// free, for HTTP/2 one with a free stream, and waits only while every
// connection is busy (Testing "Load generator and L4 balancer", HTTP).
// With SourceAddrs, request seq first goes to source address seq % n.
type HTTPTarget struct {
	cfg HTTPConfig
	// groups holds one group per source address, or one group dialing
	// from the system's choice.
	groups []*group
}

// group is one source address's share of the connections: one lane
// pooling every HTTP/1.1 connection, or one lane per connection, a
// round-robin HTTP/1.1 connection or an HTTP/2 connection, whose free
// request slots the group hands out in turn.
type group struct {
	lanes []*lane
	// free holds a lane index for each free request slot, one per
	// round-robin HTTP/1.1 connection and StreamsPerConn per HTTP/2
	// connection, least recently freed first, so requests take the
	// connections in turn and a busy one, such as one that never answers,
	// is passed over; nil for a pool, whose transport picks an idle
	// connection itself.
	free chan int
}

// lane is one connection pool: every HTTP/1.1 connection of a group, or
// one round-robin HTTP/1.1 connection, or one HTTP/2 connection.
type lane struct {
	tr     *http.Transport
	client *http.Client
}

// take claims the next free request slot in turn and returns its lane,
// waiting only while every slot is taken; ok is false when ctx ended
// first. A free slot is taken even under an ended ctx, which then ends
// the request before anything is sent.
func (g *group) take(ctx context.Context) (lane int, ok bool) {
	select {
	case i := <-g.free:
		return i, true
	default:
	}
	select {
	case i := <-g.free:
		return i, true
	case <-ctx.Done():
		return 0, false
	}
}

// release frees a request slot of lane i, last in turn.
func (g *group) release(i int) { g.free <- i }

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
	case c.Protocol != HTTP1 && c.RoundRobin:
		return nil, errors.New("loadgen: RoundRobin applies to HTTP/1.1; HTTP/2 requests always spread")
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
		if c.Protocol == HTTP1 && !c.RoundRobin {
			t.groups = append(t.groups, &group{lanes: []*lane{t.newLane(src, share)}})
			continue
		}
		// One lane per connection, a round-robin HTTP/1.1 connection with
		// one request slot or an HTTP/2 connection with a slot per stream,
		// the slots queued lane after lane, round after round, so requests
		// arriving one by one take the connections in order.
		slots := 1
		if c.Protocol != HTTP1 {
			slots = c.StreamsPerConn
		}
		grp := &group{free: make(chan int, share*slots)}
		for range share {
			grp.lanes = append(grp.lanes, t.newLane(src, 1))
		}
		for range slots {
			for i := range share {
				grp.free <- i
			}
		}
		t.groups = append(t.groups, grp)
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

// newLane returns a pool of conns connections, dialing from src when it
// is valid; its group bounds the requests each connection carries.
func (t *HTTPTarget) newLane(src netip.Addr, conns int) *lane {
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
	return &lane{
		tr: tr,
		client: &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Close closes the target's idle connections.
func (t *HTTPTarget) Close() error {
	for _, g := range t.groups {
		for _, l := range g.lanes {
			l.tr.CloseIdleConnections()
		}
	}
	return nil
}

// group returns the group of request seq: that of source address
// seq % groups.
func (t *HTTPTarget) group(seq uint64) *group {
	return t.groups[seq%uint64(len(t.groups))]
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

// Do sends request r and reads the whole response body. On a group of
// connections taken in turn it first claims the next free one, and frees
// it once the body is read and closed.
func (t *HTTPTarget) Do(ctx context.Context, r Request) Response {
	g := t.group(r.Seq)
	var resp Response
	if t.cfg.Keys != nil {
		resp.Key = t.cfg.Keys.Key(r.Seq)
	}
	l := g.lanes[0]
	if g.free != nil {
		i, ok := g.take(ctx)
		if !ok {
			resp.Err = context.Cause(ctx)
			resp.Outcome = Classify(resp.Err)
			return resp
		}
		defer g.release(i)
		l = g.lanes[i]
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
