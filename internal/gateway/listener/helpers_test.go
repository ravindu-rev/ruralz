// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// waitFor polls cond with a real-time bound; it waits only for goroutines
// the test or the Set started, never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

// recv waits for one value from ch with a real-time bound.
func recv[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// Telemetry fakes.

type fakeCounter struct{ n atomic.Int64 }

func (c *fakeCounter) Add(_ emit.Stripe, n uint64) { c.n.Add(int64(n)) } //nolint:gosec // G115: test counts stay small.

type fakeGauge struct{ n atomic.Int64 }

func (g *fakeGauge) Add(_ emit.Stripe, d int64) { g.n.Add(d) }
func (g *fakeGauge) Set(v int64)                { g.n.Store(v) }

type fakeHistogram struct{ n atomic.Int64 }

func (h *fakeHistogram) Record(emit.Stripe, uint64)                        { h.n.Add(1) }
func (h *fakeHistogram) RecordExemplar(emit.Stripe, uint64, emit.Exemplar) { h.n.Add(1) }

type fakeRequests struct{}

func (fakeRequests) Inc(emit.Stripe, int, int, int) {}

// fakeMetrics is one listener's handles with readable values.
type fakeMetrics struct {
	m         *emit.ListenerMetrics
	open      [emit.NumProtocols]*fakeGauge
	conns     [emit.NumProtocols][emit.NumConnResults]*fakeCounter
	handshake *fakeHistogram
}

func newFakeMetrics() *fakeMetrics {
	f := &fakeMetrics{handshake: &fakeHistogram{}}
	f.m = &emit.ListenerMetrics{
		Requests:        fakeRequests{},
		GatewayDuration: &fakeHistogram{},
		RequestBody:     &fakeHistogram{},
		ResponseBody:    &fakeHistogram{},
		Active:          &fakeGauge{},
		TLSHandshake:    f.handshake,
	}
	for p := range f.open {
		f.open[p] = &fakeGauge{}
		f.m.OpenConns[p] = f.open[p]
		for r := range f.conns[p] {
			f.conns[p][r] = &fakeCounter{}
			f.m.Conns[p][r] = f.conns[p][r]
		}
	}
	return f
}

// count returns ruralz_listener_connections_total{protocol, result}.
func (f *fakeMetrics) count(proto, result int) int64 { return f.conns[proto][result].n.Load() }

// openConns returns ruralz_listener_open_connections{protocol}.
func (f *fakeMetrics) openConns(proto int) int64 { return f.open[proto].n.Load() }

// fakeStatus records the cleartext client hops.
type fakeStatus struct{ hops atomic.Int64 }

func (*fakeStatus) SetDegraded(catalog.Reason, string, bool) {}
func (s *fakeStatus) SetCleartextHops(h catalog.Hop, n int) {
	if h == catalog.HopClient {
		s.hops.Store(int64(n))
	}
}

// fakeSecrets is a secret.Store with fixed values; it counts live watches.
type fakeSecrets struct {
	mu      sync.Mutex
	vals    map[secret.Ref]secret.Value
	watches atomic.Int64
}

func newFakeSecrets() *fakeSecrets { return &fakeSecrets{vals: make(map[secret.Ref]secret.Value)} }

func (f *fakeSecrets) set(r secret.Ref, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vals[r] = secret.NewValue(b)
}

func (f *fakeSecrets) Get(r secret.Ref) (secret.Value, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vals[r]
	return v, ok
}

func (f *fakeSecrets) Watch(secret.Ref, func(secret.Value) error) func() {
	f.watches.Add(1)
	var once sync.Once
	return func() { once.Do(func() { f.watches.Add(-1) }) }
}

// Certificates.

// testPKI issues server and client certificates from one test CA.
type testPKI struct {
	ca      *pki.CA
	secrets *fakeSecrets
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return &testPKI{ca: ca, secrets: newFakeSecrets()}
}

// cert issues a server certificate for hosts, stores it in the secrets
// and returns the listener's certificate entry named name.
func (p *testPKI) cert(t *testing.T, name string, hosts ...string) snapshot.Certificate {
	t.Helper()
	leaf, err := p.ca.Issue(pki.LeafOptions{CommonName: name, DNSNames: dnsOf(hosts), IPAddresses: ipsOf(hosts), Server: true})
	if err != nil {
		t.Fatal(err)
	}
	c := snapshot.Certificate{
		Name:        name,
		Certificate: secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/tls/" + name + ".crt"},
		PrivateKey:  secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/tls/" + name + ".key"},
	}
	p.secrets.set(c.Certificate, leaf.ChainPEM)
	p.secrets.set(c.PrivateKey, leaf.KeyPEM)
	return c
}

// client issues a client certificate.
func (p *testPKI) client(t *testing.T) tls.Certificate {
	t.Helper()
	leaf, err := p.ca.Client("client")
	if err != nil {
		t.Fatal(err)
	}
	c, err := leaf.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (p *testPKI) roots() *x509.CertPool { return p.ca.Pool() }

func dnsOf(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		if net.ParseIP(h) == nil {
			out = append(out, h)
		}
	}
	return out
}

func ipsOf(hosts []string) []net.IP {
	var out []net.IP
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

// Snapshots.

func httpListener(name string, port int) *snapshot.Listener {
	return &snapshot.Listener{Name: name, Protocol: v1alpha1.ListenerProtocolHTTP, Port: port}
}

// httpsListener returns an https listener on a free port.
func httpsListener(name string, mtls bool, certs ...snapshot.Certificate) *snapshot.Listener {
	return &snapshot.Listener{
		Name:     name,
		Protocol: v1alpha1.ListenerProtocolHTTPS,
		TLS:      &snapshot.ListenerTLS{MinVersion: tls.VersionTLS13, Certificates: certs, RequestClientCert: mtls},
	}
}

func snap(ls ...*snapshot.Listener) *snapshot.Snapshot {
	return &snapshot.Snapshot{Listeners: ls, Gateway: snapshot.Gateway{AdminPort: 9901}}
}

// Handler.

// testHandler answers "<proto> <listener> <requested> <certs> <peer>
// <stripe> <tag>". /block waits until release is closed.
type testHandler struct {
	tag     string
	entered chan string
	release chan struct{}
}

func newTestHandler(tag string) *testHandler {
	return &testHandler{tag: tag, entered: make(chan string, 64), release: make(chan struct{})}
}

func (h *testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := ConnFrom(r.Context())
	if c == nil {
		http.Error(w, "no listener connection", http.StatusInternalServerError)
		return
	}
	if r.URL.Path == "/block" {
		h.entered <- r.Proto
		<-h.release
	}
	certs := 0
	if r.TLS != nil {
		certs = len(r.TLS.PeerCertificates)
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprintf(w, "%s %s %t %d %s %d %s", r.Proto, c.Listener(), c.ClientCertRequested(), certs, c.Peer(), c.Stripe(), h.tag) //nolint:gosec // G705: a plain-text test answer.
}

// Sets.

type harness struct {
	set     *Set
	clk     *clocktest.Fake
	conns   *admission.ConnLimiter
	handler *testHandler
	status  *fakeStatus
}

// newHarness returns a Set on loopback with a fake clock that tracks wall
// time, closed at the end of the test.
func newHarness(t *testing.T, mod ...func(*Options)) *harness {
	t.Helper()
	h := &harness{
		clk:     clocktest.New(clock.Real().Now()),
		conns:   admission.NewConnLimiter(admission.DefaultConnections),
		handler: newTestHandler("set"),
		status:  &fakeStatus{},
	}
	o := Options{Handler: h.handler, Conns: h.conns, Clock: h.clk, Hosts: []string{"127.0.0.1"}, Status: h.status}
	for _, m := range mod {
		m(&o)
	}
	h.handler, _ = o.Handler.(*testHandler)
	s, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.set = s
	t.Cleanup(func() {
		if h.handler != nil {
			h.handler.unblock()
		}
		_ = s.Close()
	})
	return h
}

// unblock releases every /block request, once.
func (h *testHandler) unblock() {
	select {
	case <-h.release:
	default:
		close(h.release)
	}
}

// activate prepares and commits next.
func (h *harness) activate(t *testing.T, next *snapshot.Snapshot, secrets secret.Store) {
	t.Helper()
	p, err := h.set.Prepare(t.Context(), next, secrets)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := p.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// addr returns the bound address of listener name.
func (h *harness) addr(t *testing.T, name string) string {
	t.Helper()
	a := h.set.Addr(name)
	if a == nil {
		t.Fatalf("listener %q is not serving", name)
	}
	return a.String()
}

// endpoint returns the serving endpoint named name.
func (h *harness) endpoint(t *testing.T, name string) *endpoint {
	t.Helper()
	h.set.mu.Lock()
	defer h.set.mu.Unlock()
	for key, ep := range h.set.active {
		if key.Name == name {
			return ep
		}
	}
	t.Fatalf("listener %q is not serving", name)
	return nil
}

// Clients.

// client returns a client of the given protocols: HTTP/1.1, HTTP/2 over
// TLS, h2c by prior knowledge, and the log of the connections it dials.
func client(t *testing.T, cfg *tls.Config, h1, h2, h2c bool) (*http.Client, *dialLog) {
	t.Helper()
	var p http.Protocols
	p.SetHTTP1(h1)
	p.SetHTTP2(h2)
	p.SetUnencryptedHTTP2(h2c)
	dials := &dialLog{}
	var d net.Dialer
	if cfg != nil {
		cfg = cfg.Clone() // an HTTP/2 transport adds "h2" to its config's NextProtos
	}
	tr := &http.Transport{
		Protocols:       &p,
		TLSClientConfig: cfg,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return dials.add(c), nil
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}, dials
}

// dialLog records, in order, the connections a test client dials, and when
// its transport closes each. The HTTP/2 transport marks a connection dead
// before it closes it, once it processed the server's GOAWAY or close, so
// a request sent after the close never reuses that connection, while one
// sent before may still go out on it.
type dialLog struct {
	mu    sync.Mutex
	conns []*trackedConn
}

// add records c and returns it wrapped.
func (l *dialLog) add(c net.Conn) net.Conn {
	tc := &trackedConn{Conn: c, closed: make(chan struct{})}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.conns = append(l.conns, tc)
	return tc
}

// count returns the number of connections dialed.
func (l *dialLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

// closed returns a channel closed once the client closed dialed
// connection i, counting from 0.
func (l *dialLog) closed(t *testing.T, i int) <-chan struct{} {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if i >= len(l.conns) {
		t.Fatalf("connection %d was never dialed (%d were)", i, len(l.conns))
	}
	return l.conns[i].closed
}

// trackedConn is a dialed connection that reports its Close.
type trackedConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// get sends GET url and returns the status and the body.
func get(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("GET %s: body: %v", url, err)
	}
	return resp.StatusCode, string(b)
}

// rawConn is an HTTP/1.1 connection the test drives by hand.
type rawConn struct {
	net.Conn
	br *bufio.Reader
}

// dialRaw opens a TCP connection, after a PROXY v2 header when hdr is set.
func dialRaw(t *testing.T, addr string, hdr []byte) *rawConn {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if hdr != nil {
		if _, err := c.Write(hdr); err != nil {
			t.Fatalf("write PROXY header: %v", err)
		}
	}
	return &rawConn{Conn: c, br: bufio.NewReader(c)}
}

// dialTLS opens a TLS connection with ALPN http/1.1.
func dialTLS(t *testing.T, addr string, cfg *tls.Config) *rawConn {
	t.Helper()
	cfg = cfg.Clone()
	cfg.NextProtos = []string{"http/1.1"}
	d := tls.Dialer{Config: cfg}
	c, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("TLS dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &rawConn{Conn: c, br: bufio.NewReader(c)}
}

// reply is one HTTP/1.1 response read from a rawConn.
type reply struct {
	status int
	// close is true when the response ends the connection (Connection:
	// close).
	close bool
	body  string
}

// roundTrip sends one HTTP/1.1 request and reads its response.
func (c *rawConn) roundTrip(ctx context.Context, path string, header http.Header) (reply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://test"+path, nil)
	if err != nil {
		return reply{}, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if err := req.Write(c); err != nil {
		return reply{}, err
	}
	resp, err := http.ReadResponse(c.br, req)
	if err != nil {
		return reply{}, err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return reply{status: resp.StatusCode, close: resp.Close, body: string(b)}, err
}

// mustRoundTrip sends GET / and fails the test on error.
func (c *rawConn) mustRoundTrip(t *testing.T) reply {
	t.Helper()
	r, err := c.roundTrip(t.Context(), "/", nil)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	return r
}

// closedByPeer reports whether the peer closed the connection: a read
// returns EOF or a reset, never data. The read deadline only turns a hang
// into a failure.
func (c *rawConn) closedByPeer(t *testing.T) bool {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	n, err := c.br.Read(b)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return false
	}
	return n == 0 && err != nil
}

// proxyV2 returns a PROXY protocol v2 header for TCP over IPv4.
func proxyV2(src, dst netip.AddrPort) []byte {
	b := []byte("\r\n\r\n\x00\r\nQUIT\n\x21\x11\x00\x0c")
	s, d := src.Addr().As4(), dst.Addr().As4()
	b = append(b, s[:]...)
	b = append(b, d[:]...)
	b = binary.BigEndian.AppendUint16(b, src.Port())
	return binary.BigEndian.AppendUint16(b, dst.Port())
}

// refused reports whether dialing addr is refused.
func refused(t *testing.T, addr string) bool {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		return true
	}
	_ = c.Close()
	return false
}

// portOf returns the port of addr.
func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// field returns field i of a testHandler body.
func field(body string, i int) string {
	f := strings.Fields(body)
	if i >= len(f) {
		return ""
	}
	return f[i]
}
