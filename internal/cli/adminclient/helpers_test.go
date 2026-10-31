// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// Test tokens: RFC 6750 b64token, at least 22 bytes; canaries, not
// credentials.
//
//nolint:gosec // G101: test canaries, not credentials.
const (
	testToken    = "op-canary-4fQ9x2LmZ7rT1vB8nK3sW6yH"
	rotatedToken = "op-canary-rotated-8Hs2Lq9Zp4Xw7Rt1"
)

// testVersion is the CLI version of the test clients.
const testVersion = "0.4.0"

func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// writeFile writes content to dir/name and returns the path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// tokenFile writes a token file holding tok and a newline.
func tokenFile(t *testing.T, tok string) string {
	t.Helper()
	return writeFile(t, t.TempDir(), "token", tok+"\n")
}

// countingRand yields 1, 2, 3, ... so every traceparent differs.
type countingRand struct {
	mu sync.Mutex
	n  byte
}

func (r *countingRand) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range p {
		r.n++
		p[i] = r.n
	}
	return len(p), nil
}

// newTestClient returns a client of cfg on a fake clock, which no test
// advances unless it tests a deadline.
func newTestClient(t *testing.T, cfg Config) (*Client, *clocktest.Fake) {
	t.Helper()
	clk := clocktest.New(epoch())
	c, err := New(cfg, Options{Clock: clk, Rand: &countingRand{}, Version: testVersion})
	if err != nil {
		t.Fatalf("New(%+v) = %v", cfg, err)
	}
	t.Cleanup(c.Close)
	return c, clk
}

// serve starts an http server for h, closed when the test ends.
func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// recorded is one request a test server saw.
type recorded struct {
	uri    string
	header http.Header
}

// recorder records the requests a test server saw.
type recorder struct {
	mu   sync.Mutex
	reqs []recorded
}

func (r *recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, recorded{uri: req.RequestURI, header: req.Header.Clone()})
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

// waitFor polls cond under a real-time deadline.
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

// interruptible returns a context and the function that cancels it as a
// first Ctrl-C does.
func interruptible(t *testing.T) (context.Context, func()) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(t.Context())
	t.Cleanup(func() { cancel(nil) })
	return ctx, func() { cancel(command.Interrupted{Signal: os.Interrupt}) }
}

// result is what a call run in a goroutine returned.
type result struct {
	body []byte
	err  error
}

// await waits for a call run in a goroutine, under a real-time deadline.
func await(t *testing.T, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not return")
		return result{}
	}
}

// goDump runs ConfigDump in a goroutine.
func goDump(ctx context.Context, c *Client) <-chan result {
	ch := make(chan result, 1)
	go func() {
		b, err := c.ConfigDump(ctx)
		ch <- result{body: b, err: err}
	}()
	return ch
}

// peerConn reports remote as its peer address and counts the bytes
// written to it.
type peerConn struct {
	net.Conn
	remote net.Addr
	wrote  atomic.Int64
}

func (c *peerConn) RemoteAddr() net.Addr { return c.remote }

func (c *peerConn) Write(p []byte) (int, error) {
	c.wrote.Add(int64(len(p)))
	return c.Conn.Write(p)
}

func tcpAddr(s string) net.Addr { return net.TCPAddrFromAddrPort(netip.MustParseAddrPort(s)) }

// dialer is a dial hook that connects to target whatever address the
// transport asks for, reports remote as the peer, and records the
// addresses asked for and the connections made.
type dialer struct {
	target, remote string

	mu    sync.Mutex
	addrs []string
	conns []*peerConn
}

func (d *dialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, network, d.target)
	if err != nil {
		return nil, err
	}
	pc := &peerConn{Conn: conn, remote: tcpAddr(d.remote)}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrs = append(d.addrs, addr)
	d.conns = append(d.conns, pc)
	return pc, nil
}

func (d *dialer) asked() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// testPKI is a test CA, the admin TLS directory of a Node it issued
// (tls.crt and tls.key for localhost, 127.0.0.1 and ::1, and ca.crt
// admitting its client certificates), the CA file for --ca-file and a
// client certificate with clientAuth.
type testPKI struct {
	ca                    *pki.CA
	adminDir              string
	caFile                string
	clientCert, clientKey string
}

func newPKI(t *testing.T) *testPKI {
	t.Helper()
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ca.Server()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, _, err := srv.WriteFiles(dir, "tls"); err != nil {
		t.Fatal(err)
	}
	if err := ca.WriteFile(filepath.Join(dir, tlsconf.AdminClientCAFile)); err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := ca.WriteFile(caFile); err != nil {
		t.Fatal(err)
	}
	cl, err := ca.Client("operator")
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := cl.WriteFiles(t.TempDir(), "client")
	if err != nil {
		t.Fatal(err)
	}
	return &testPKI{ca: ca, adminDir: dir, caFile: caFile, clientCert: cert, clientKey: key}
}

// serveTLS starts an https server with the Node's admin TLS configuration
// over p.adminDir (tlsconf.Admin) for h.
func serveTLS(t *testing.T, p *testPKI, h http.Handler) *httptest.Server {
	t.Helper()
	cfg, err := tlsconf.Admin(p.adminDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// traceIDOf returns the trace ID of a valid version 00 traceparent, the
// requestId a Node gives the request's problem documents.
func traceIDOf(traceparent string) (string, bool) {
	m := regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$`).FindStringSubmatch(traceparent)
	if m == nil || m[1] == "00000000000000000000000000000000" || m[2] == "0000000000000000" {
		return "", false
	}
	return m[1], true
}
