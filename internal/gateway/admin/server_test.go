// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminauth"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/readiness"
	"github.com/ravindu-rev/ruralz/internal/gateway/tap"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// TestNewRefusesUnsafeConfig checks the wiring guards: no server without
// an authenticator, without the audit logger of /tap and /config/dump,
// without readiness, without the requestId function of its problem
// documents, or with TLS that does not follow RURALZ_ADMIN_TLS_DIR.
func TestNewRefusesUnsafeConfig(t *testing.T) {
	tlsDir := t.TempDir()
	for _, name := range []string{adminauth.TLSCertFile, adminauth.TLSKeyFile} {
		if err := os.WriteFile(filepath.Join(tlsDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plain := newAuth(t, operatorSettings(t), &logBuffer{})
	withTLS := newAuth(t, adminauth.Settings{TokenFile: tokenFile(t, "o", operatorToken), TLSDir: tlsDir}, &logBuffer{})
	for _, tc := range []struct {
		name string
		cfg  Config
		want error
	}{
		{"no authenticator", Config{Readiness: readiness.New()}, ErrNoAuthenticator},
		{"no audit logger", Config{Auth: newAuth(t, operatorSettings(t), nil), Readiness: readiness.New()}, ErrNoAudit},
		{"no readiness", Config{Auth: plain}, ErrNoReadiness},
		{"no requestId function", Config{Auth: plain, Readiness: readiness.New()}, ErrNoRequestID},
		{"TLS without the TLS directory", Config{
			Auth: plain, Readiness: readiness.New(), RequestID: requestID, TLS: &tls.Config{MinVersion: tls.VersionTLS12},
		}, ErrTLSMismatch},
		{"TLS directory without TLS", Config{Auth: withTLS, Readiness: readiness.New(), RequestID: requestID}, ErrTLSMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); !errors.Is(err, tc.want) {
				t.Fatalf("New = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestServerBounds checks the http.Server settings of "Bounded
// resources" and the admin port default: header timeout 10 s, idle 120 s,
// a 256 KiB header block, the HTTP/2 bounds, no ReadTimeout or
// WriteTimeout (streams and profiles run long), HTTP/1.1 and HTTP/2 over
// TLS only (no h2c).
func TestServerBounds(t *testing.T) {
	srv := newFixture(t, nil).srv.srv
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 120*time.Second || srv.MaxHeaderBytes != 256<<10 {
		t.Errorf("ReadHeaderTimeout %v, IdleTimeout %v, MaxHeaderBytes %d", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.MaxHeaderBytes)
	}
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Errorf("ReadTimeout %v, WriteTimeout %v; want unset", srv.ReadTimeout, srv.WriteTimeout)
	}
	if h2 := srv.HTTP2; h2 == nil || h2.MaxConcurrentStreams != 250 || h2.MaxReceiveBufferPerConnection != 256<<10 ||
		h2.MaxReceiveBufferPerStream != 64<<10 || h2.MaxReadFrameSize != 16<<10 {
		t.Errorf("HTTP2 %+v, want 250 streams, 256 KiB and 64 KiB receive buffers, 16 KiB frames", h2)
	}
	if p := srv.Protocols; p == nil || !p.HTTP1() || !p.HTTP2() || p.UnencryptedHTTP2() {
		t.Errorf("Protocols %v, want HTTP1 and HTTP2 over TLS only", p)
	}
	if DefaultPort != 9901 {
		t.Errorf("DefaultPort %d, want 9901", DefaultPort)
	}
}

// TestCleartextHop checks that admin without TLS is a reported cleartext
// hop while it runs, and that admin over TLS is not.
func TestCleartextHop(t *testing.T) {
	f := newFixture(t, nil)
	if got := f.status.values(); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("hops after New = %v, want [1]", got)
	}
	if err := f.srv.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.status.values(); !reflect.DeepEqual(got, []int{1, 0}) {
		t.Fatalf("hops after Shutdown = %v, want [1 0]", got)
	}
	tf := newTLSFixture(t)
	if got := tf.status.values(); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("hops of a TLS server = %v, want [0]", got)
	}
}

// TestTapStreamsAndShutdownEndsIt checks /tap over a real connection: an
// operator subscribes and receives the published lines, and Shutdown ends
// the stream at once, so it never waits on a subscriber.
func TestTapStreamsAndShutdownEndsIt(t *testing.T) {
	f := newFixture(t, nil)
	base := serve(t, f.srv, "http")
	resp := clientGet(t, http.DefaultClient, base+PathTap+"?sample=1", operatorToken)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != tap.ContentType {
		t.Fatalf("tap = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !f.hub.Active() {
		t.Fatal("no subscriber after the response headers")
	}
	f.hub.Publish(0, &tap.Event{Listener: "public", Protocol: "http1", Route: "orders", Method: "GET", Path: "/orders", Status: 200})
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, `"route":"orders"`) {
		t.Fatalf("line %q, %v", line, err)
	}
	if err := f.srv.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("stream after Shutdown: %v", err)
	}
	if f.hub.Active() {
		t.Fatal("subscriber kept after Shutdown")
	}
}

// TestServeAfterShutdown checks that Serve refuses to start once the
// server is shut down.
func TestServeAfterShutdown(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.srv.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if err := f.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve after Shutdown = %v, want http.ErrServerClosed", err)
	}
}

// TestConnectionCeiling checks that Serve counts connections against the
// admin listener's own ceiling of 256.
func TestConnectionCeiling(t *testing.T) {
	f := newFixture(t, nil)
	if got := f.srv.limiter.Ceiling(); got != admission.AdminConnections {
		t.Fatalf("ceiling %d, want %d", got, admission.AdminConnections)
	}
	base := serve(t, f.srv, "http")
	c := &http.Client{Transport: &http.Transport{}}
	defer c.CloseIdleConnections()
	resp := clientGet(t, c, base+PathHealthz, "")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	// The keep-alive connection is still open.
	if got := f.srv.limiter.Open(); got != 1 {
		t.Fatalf("open connections %d, want 1", got)
	}
}

// TestDisableKeepAlives checks the handover's readiness move: afterwards
// every response carries Connection: close.
func TestDisableKeepAlives(t *testing.T) {
	f := newFixture(t, nil)
	base := serve(t, f.srv, "http")
	c := &http.Client{Transport: &http.Transport{}}
	defer c.CloseIdleConnections()
	check := func(wantClose bool) {
		t.Helper()
		resp := clientGet(t, c, base+PathHealthz, "")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.Close != wantClose {
			t.Fatalf("Connection: close = %v, want %v", resp.Close, wantClose)
		}
	}
	check(false)
	f.srv.DisableKeepAlives()
	check(true)
}

// TestEnableKeepAlives checks a lost handover's undo: after
// EnableKeepAlives responses keep their connections alive again.
func TestEnableKeepAlives(t *testing.T) {
	f := newFixture(t, nil)
	base := serve(t, f.srv, "http")
	c := &http.Client{Transport: &http.Transport{}}
	defer c.CloseIdleConnections()
	check := func(wantClose bool) {
		t.Helper()
		resp := clientGet(t, c, base+PathHealthz, "")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.Close != wantClose {
			t.Fatalf("Connection: close = %v, want %v", resp.Close, wantClose)
		}
	}
	f.srv.DisableKeepAlives()
	check(true)
	f.srv.EnableKeepAlives()
	check(false)
}

// TestOptionsAsteriskAuthenticated checks that "OPTIONS *" is not
// answered by net/http's built-in handler: it is authenticated like any
// other unknown path.
func TestOptionsAsteriskAuthenticated(t *testing.T) {
	f := newFixture(t, nil)
	base := serve(t, f.srv, "http")
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "OPTIONS * HTTP/1.1\r\nHost: admin\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("OPTIONS * = %d, want 401", resp.StatusCode)
	}
}

// tlsFixture is a TLS admin server with a client certificate authority.
type tlsFixture struct {
	*fixture
	ca     *pki.CA
	client *pki.Leaf
}

// newTLSFixture builds a server from an admin TLS directory holding
// tls.crt, tls.key and ca.crt, as RURALZ_ADMIN_TLS_DIR does.
func newTLSFixture(t *testing.T) *tlsFixture {
	t.Helper()
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server, err := ca.Server()
	if err != nil {
		t.Fatal(err)
	}
	client, err := ca.Client("operator")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, _, err := server.WriteFiles(dir, "tls"); err != nil {
		t.Fatal(err)
	}
	if err := ca.WriteFile(filepath.Join(dir, tlsconf.AdminClientCAFile)); err != nil {
		t.Fatal(err)
	}
	cfg, err := tlsconf.Admin(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(c *Config) {
		c.TLS = cfg
		c.Auth = newAuth(t, adminauth.Settings{TokenFile: tokenFile(t, "o", operatorToken), TLSDir: dir}, &logBuffer{})
	})
	return &tlsFixture{fixture: f, ca: ca, client: client}
}

// TestTLSServesHTTP2AndClientCertificates checks admin TLS: HTTP/2 over
// ALPN, the open paths without a credential, and an operator path
// admitted by a client certificate chaining to ca.crt.
func TestTLSServesHTTP2AndClientCertificates(t *testing.T) {
	f := newTLSFixture(t)
	base := serve(t, f.srv, "https")
	clientTLS, err := pki.ClientTLS(f.ca, f.client)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{TLSClientConfig: clientTLS, ForceAttemptHTTP2: true}
	c := &http.Client{Transport: tr}
	defer c.CloseIdleConnections()
	for _, path := range []string{PathHealthz, PathSnapshots} {
		resp := clientGet(t, c, base+path, "")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
			t.Errorf("GET %s = %d over HTTP/%d, want 200 over HTTP/2", path, resp.StatusCode, resp.ProtoMajor)
		}
	}
	// Without a client certificate an operator path needs the token.
	anon, err := pki.ClientTLS(f.ca, nil)
	if err != nil {
		t.Fatal(err)
	}
	c2 := &http.Client{Transport: &http.Transport{TLSClientConfig: anon}}
	defer c2.CloseIdleConnections()
	resp := clientGet(t, c2, base+PathSnapshots, "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("snapshots without a credential = %d, want 401", resp.StatusCode)
	}
	resp = clientGet(t, c2, base+PathSnapshots, operatorToken)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("snapshots with the operator token = %d, want 200", resp.StatusCode)
	}
}

// TestShutdownBoundedByContext checks that Shutdown returns when its
// context ends even while a request still runs, closing it.
func TestShutdownBoundedByContext(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{})
	f := newFixture(t, func(c *Config) {
		c.Upstreams = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(entered)
			<-block // ignores its context, like a stalled handler
		})
	})
	defer close(block)
	base := serve(t, f.srv, "http")
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(mustRequest(t, base+PathUpstreams))
		if err == nil {
			_ = resp.Body.Close()
		}
		errc <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.srv.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown = %v, want context.Canceled", err)
	}
	if err := <-errc; err == nil {
		t.Fatal("the running request survived Shutdown")
	}
}

// mustRequest returns an authenticated GET request for url.
func mustRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	return req
}
