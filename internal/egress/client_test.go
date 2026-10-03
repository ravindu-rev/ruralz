// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for the guarded HTTP client, spec 06 requirements 37 (redirects
// https to https, at most 3 hops, each re-checked; body cap), 69 (tokenUrl
// redirects never followed), 84 (guard per hop), 85 (proxy variables only
// with env-proxy, the proxy's address checked) and 86 (no https to http,
// no cookies, capped bodies), and R-49 (a destination-bound client never
// contacts a second origin).

// tlsServer is an httptest TLS server that counts its requests.
type tlsServer struct {
	*httptest.Server
	hits atomic.Int64
}

func newTLSServer(t *testing.T, h http.HandlerFunc) *tlsServer {
	t.Helper()
	s := &tlsServer{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// rootsFor trusts the httptest certificate (SANs example.com, 127.0.0.1,
// ::1).
func rootsFor(s *httptest.Server) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func loopbackGuard(t *testing.T) *Guard {
	t.Helper()
	g, err := ParseAllow("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// get fetches u and returns the status (also of a redirect the
// CheckRedirect policy refused, whose body the client already closed), the
// body and the error.
func get(t *testing.T, c *http.Client, u string) (int, []byte, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if resp == nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err != nil {
		return resp.StatusCode, nil, err
	}
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

func TestClientFetchReq84(t *testing.T) {
	s := newTLSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"keys":[]}`)
	})
	c := loopbackGuard(t).Client(ClientOptions{TLS: rootsFor(s.Server), Timeout: 5 * time.Second})
	defer c.CloseIdleConnections()
	status, body, err := get(t, c, s.URL)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || string(body) != `{"keys":[]}` {
		t.Fatalf("status %d body %q", status, body)
	}
	if c.Jar != nil {
		t.Fatal("the guarded client must have no cookie jar")
	}
}

// TestClientRefusesDeniedDestinationReq84 fetches a loopback server without
// an allow entry: refused before any byte reaches it.
func TestClientRefusesDeniedDestinationReq84(t *testing.T) {
	s := newTLSServer(t, func(http.ResponseWriter, *http.Request) {})
	var g *Guard
	c := g.Client(ClientOptions{TLS: rootsFor(s.Server)})
	defer c.CloseIdleConnections()
	if _, _, err := get(t, c, s.URL); !errors.Is(err, ErrDenied) {
		t.Fatalf("fetch = %v, want ErrDenied", err)
	}
	// A DNS name resolving to loopback is refused at connect time.
	g2 := &Guard{resolver: fakeResolver(t, netip.MustParseAddr("127.0.0.1"))}
	_, port, _ := net.SplitHostPort(s.Listener.Addr().String())
	c2 := g2.Client(ClientOptions{TLS: rootsFor(s.Server)})
	defer c2.CloseIdleConnections()
	if _, _, err := get(t, c2, "https://rebind.test:"+port+"/"); !errors.Is(err, ErrDenied) {
		t.Fatalf("rebinding fetch = %v, want ErrDenied", err)
	}
	if n := s.hits.Load(); n != 0 {
		t.Fatalf("server saw %d requests", n)
	}
}

// TestClientRedirectsReq37Req86 covers the JWKS redirect policy.
func TestClientRedirectsReq37Req86(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the https to http redirect target was contacted")
	}))
	defer plain.Close()

	var s *tlsServer
	s = newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/hops/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hops/"))
			if n == 0 {
				_, _ = io.WriteString(w, "done")
				return
			}
			http.Redirect(w, r, fmt.Sprintf("%s/hops/%d", s.URL, n-1), http.StatusFound)
		case r.URL.Path == "/to-http":
			http.Redirect(w, r, plain.URL+"/", http.StatusMovedPermanently)
		case r.URL.Path == "/to-denied-literal":
			http.Redirect(w, r, "https://127.0.0.2:1/jwks", http.StatusFound)
		case r.URL.Path == "/to-denied-name":
			_, port, _ := net.SplitHostPort(s.Listener.Addr().String())
			http.Redirect(w, r, "https://rebind.test:"+port+"/hops/0", http.StatusTemporaryRedirect)
		case r.URL.Path == "/to-metadata":
			http.Redirect(w, r, "https://[fd00:ec2::254]/latest/meta-data", http.StatusFound)
		}
	})
	g := loopbackGuard(t)
	g.resolver = fakeResolver(t, netip.MustParseAddr("127.0.0.2"))
	c := g.Client(ClientOptions{TLS: rootsFor(s.Server), MaxRedirects: 3, Timeout: 5 * time.Second})
	defer c.CloseIdleConnections()

	tests := []struct {
		path string
		want error
	}{
		{"/hops/0", nil},
		{"/hops/3", nil},
		{"/hops/4", ErrTooManyRedirects},
		{"/to-http", ErrScheme},
		{"/to-denied-literal", ErrDenied},
		{"/to-denied-name", ErrDenied},
		{"/to-metadata", ErrDenied},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			status, body, err := get(t, c, s.URL+tt.path)
			if tt.want == nil {
				if err != nil || status != http.StatusOK || string(body) != "done" {
					t.Fatalf("fetch: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("fetch = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestClientNoRedirectReq69Req86 is the tokenUrl client: a redirect fails
// and the second origin is never contacted.
func TestClientNoRedirectReq69Req86(t *testing.T) {
	second := newTLSServer(t, func(http.ResponseWriter, *http.Request) {})
	first := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/token", http.StatusTemporaryRedirect)
	})
	cfg := rootsFor(first.Server)
	cfg.RootCAs.AddCert(second.Certificate())
	c := loopbackGuard(t).Client(ClientOptions{TLS: cfg, MaxRedirects: 0})
	defer c.CloseIdleConnections()
	status, _, err := get(t, c, first.URL+"/token")
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("fetch = %v, want ErrRedirect", err)
	}
	if status != 0 {
		t.Fatalf("the refused response must not be returned, got status %d", status)
	}
	if !strings.Contains(err.Error(), "307") {
		t.Fatalf("error %q should name the status", err)
	}
	if second.hits.Load() != 0 {
		t.Fatal("the redirect target was contacted")
	}
}

// TestClientNoRedirectEvery3xxReq69Req86 covers the 3xx responses
// http.Client returns without consulting CheckRedirect (no Location, 300,
// 304, and a 307 or 308 answering a POST whose body has no GetBody): a
// client that follows no redirect (MaxRedirects 0, or destination-bound,
// R-49) fails every one with ErrRedirect, and a client that follows
// redirects returns them with a nil error for the caller's status check.
func TestClientNoRedirectEvery3xxReq69Req86(t *testing.T) {
	second := newTLSServer(t, func(http.ResponseWriter, *http.Request) {})
	first := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		code, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			_, _ = io.WriteString(w, "token")
			return
		}
		if r.URL.Query().Get("location") != "" {
			w.Header().Set("Location", second.URL+"/token")
		}
		w.WriteHeader(code)
	})
	cfg := rootsFor(first.Server)
	cfg.RootCAs.AddCert(second.Certificate())
	u, err := url.Parse(first.URL)
	if err != nil {
		t.Fatal(err)
	}
	g := loopbackGuard(t)
	clients := []struct {
		name string
		c    *http.Client
	}{
		{"MaxRedirects 0", g.Client(ClientOptions{TLS: cfg})},
		{"destination-bound", g.Client(ClientOptions{TLS: cfg, MaxRedirects: 3, Destination: Origin(u)})},
	}
	following := g.Client(ClientOptions{TLS: cfg, MaxRedirects: 3})
	defer following.CloseIdleConnections()
	cases := []struct {
		name, method, path string
		follows            bool // the following client gets the response back
	}{
		{"300 without Location", http.MethodGet, "/300", true},
		{"302 without Location", http.MethodGet, "/302", true},
		{"304", http.MethodGet, "/304", true},
		{"301 with Location", http.MethodGet, "/301?location=1", false},
		{"307 on a POST without GetBody", http.MethodPost, "/307?location=1", true},
		{"308 on a POST without GetBody", http.MethodPost, "/308?location=1", true},
	}
	do := func(t *testing.T, c *http.Client, method, path string) (*http.Response, error) {
		t.Helper()
		var body io.Reader = http.NoBody
		if method == http.MethodPost {
			// Not a type NewRequest recognizes: GetBody stays nil.
			body = io.NopCloser(strings.NewReader("grant_type=client_credentials"))
		}
		req, err := http.NewRequestWithContext(t.Context(), method, first.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if method == http.MethodPost && req.GetBody != nil {
			t.Fatal("the request body must have no GetBody")
		}
		return c.Do(req)
	}
	for _, cl := range clients {
		defer cl.c.CloseIdleConnections()
		for _, tc := range cases {
			t.Run(cl.name+"/"+tc.name, func(t *testing.T) {
				resp, err := do(t, cl.c, tc.method, tc.path)
				if resp != nil {
					_ = resp.Body.Close()
					t.Fatalf("a %s response was returned (status %d, err %v)", tc.name, resp.StatusCode, err)
				}
				if !errors.Is(err, ErrRedirect) {
					t.Fatalf("fetch = %v, want ErrRedirect", err)
				}
				if !strings.Contains(err.Error(), "status "+tc.path[1:4]) {
					t.Fatalf("error %q should name the status", err)
				}
			})
		}
	}
	for _, tc := range cases {
		if !tc.follows {
			continue
		}
		t.Run("following/"+tc.name, func(t *testing.T) {
			resp, err := do(t, following, tc.method, tc.path)
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			_ = resp.Body.Close()
			if want := tc.path[1:4]; strconv.Itoa(resp.StatusCode) != want {
				t.Fatalf("status %d, want %s", resp.StatusCode, want)
			}
		})
	}
	if second.hits.Load() != 0 {
		t.Fatal("a second origin was contacted")
	}
}

// TestCheckRedirectNoFollowReq69 checks that the CheckRedirect policy of a
// client that follows no redirect refuses on its own, although the
// transport already refuses every 3xx before it runs.
func TestCheckRedirectNoFollowReq69(t *testing.T) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://idp.example/next", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	var g *Guard
	if err := g.checkRedirect(req, nil, 0); !errors.Is(err, ErrRedirect) || !strings.Contains(err.Error(), "status 0") {
		t.Fatalf("checkRedirect without a response = %v, want ErrRedirect", err)
	}
	req.Response = &http.Response{StatusCode: http.StatusFound}
	if err := g.checkRedirect(req, nil, -1); !errors.Is(err, ErrRedirect) || !strings.Contains(err.Error(), "status 302") {
		t.Fatalf("checkRedirect = %v, want ErrRedirect naming 302", err)
	}
}

// TestClientDestinationBoundR49 pins the client to one origin: a redirect
// is never followed even with MaxRedirects set, and another origin is
// refused before any connection.
func TestClientDestinationBoundR49(t *testing.T) {
	other := newTLSServer(t, func(http.ResponseWriter, *http.Request) {})
	var home *tlsServer
	home = newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/self" {
			http.Redirect(w, r, home.URL+"/token", http.StatusFound)
			return
		}
		if r.URL.Path == "/away" {
			http.Redirect(w, r, other.URL+"/token", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "token")
	})
	cfg := rootsFor(home.Server)
	cfg.RootCAs.AddCert(other.Certificate())
	u, err := url.Parse(home.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := loopbackGuard(t).Client(ClientOptions{TLS: cfg, MaxRedirects: 3, Destination: strings.ToUpper(u.Scheme) + "://" + u.Host})
	defer c.CloseIdleConnections()

	if _, body, err := get(t, c, home.URL+"/token"); err != nil || string(body) != "token" {
		t.Fatalf("fetch at the destination: %q, %v", body, err)
	}
	if _, _, err := get(t, c, home.URL+"/self"); !errors.Is(err, ErrRedirect) {
		t.Fatalf("same-origin redirect = %v, want ErrRedirect", err)
	}
	if _, _, err := get(t, c, home.URL+"/away"); !errors.Is(err, ErrRedirect) {
		t.Fatalf("cross-origin redirect = %v, want ErrRedirect", err)
	}
	if _, _, err := get(t, c, other.URL+"/token"); !errors.Is(err, ErrDestination) {
		t.Fatalf("other origin = %v, want ErrDestination", err)
	}
	if other.hits.Load() != 0 {
		t.Fatal("a second origin was contacted")
	}

	bad := loopbackGuard(t).Client(ClientOptions{TLS: cfg, Destination: "::not a url"})
	defer bad.CloseIdleConnections()
	if _, _, err := get(t, bad, home.URL+"/token"); !errors.Is(err, ErrDestination) {
		t.Fatalf("unparsable destination = %v, want ErrDestination (fail closed)", err)
	}
}

func TestClientOnlyHTTPSReq86(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a cleartext URL was fetched")
	}))
	defer plain.Close()
	c := loopbackGuard(t).Client(ClientOptions{})
	defer c.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, plain.URL, strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrScheme) {
		t.Fatalf("http fetch = %v, want ErrScheme", err)
	}
}

// TestClientBodyCapReq37Req70Req86 checks the cap with and without a
// Content-Length, and a body exactly at the cap.
func TestClientBodyCapReq37Req70Req86(t *testing.T) {
	const limit = 64 << 10
	s := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if r.URL.Query().Get("chunked") == "" {
			w.Header().Set("Content-Length", strconv.Itoa(n))
		}
		_, _ = w.Write([]byte(strings.Repeat("a", n)))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	c := loopbackGuard(t).Client(ClientOptions{TLS: rootsFor(s.Server), MaxBodyBytes: limit})
	defer c.CloseIdleConnections()
	tests := []struct {
		query   string
		wantErr bool
	}{
		{"n=0", false},
		{fmt.Sprintf("n=%d", limit), false},
		{fmt.Sprintf("n=%d", limit+1), true},
		{fmt.Sprintf("n=%d&chunked=1", limit), false},
		{fmt.Sprintf("n=%d&chunked=1", limit+1), true},
		{fmt.Sprintf("n=%d&chunked=1", 4*limit), true},
	}
	for _, tt := range tests {
		_, body, err := get(t, c, s.URL+"/?"+tt.query)
		if tt.wantErr {
			if !errors.Is(err, ErrBodyTooLarge) {
				t.Errorf("%s: err = %v, want ErrBodyTooLarge", tt.query, err)
			}
			if len(body) > limit {
				t.Errorf("%s: read %d bytes past the cap", tt.query, len(body))
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tt.query, err)
		}
	}
	// The default cap is 1 MiB.
	d := loopbackGuard(t).Client(ClientOptions{TLS: rootsFor(s.Server)})
	defer d.CloseIdleConnections()
	if _, _, err := get(t, d, fmt.Sprintf("%s/?n=%d", s.URL, DefaultMaxBodyBytes)); err != nil {
		t.Fatalf("1 MiB body: %v", err)
	}
	if _, _, err := get(t, d, fmt.Sprintf("%s/?n=%d&chunked=1", s.URL, DefaultMaxBodyBytes+1)); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("1 MiB + 1 body = %v, want ErrBodyTooLarge", err)
	}
}

func TestCappedBodyStickyError(t *testing.T) {
	b := &cappedBody{rc: io.NopCloser(strings.NewReader("abcdef")), left: 3}
	buf := make([]byte, 10)
	n, err := b.Read(buf)
	if n != 3 || !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Read = %d, %v", n, err)
	}
	if n, err := b.Read(buf); n != 0 || !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("second Read = %d, %v", n, err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestClientNoCookiesReq37Req86 checks that a Set-Cookie is never sent back.
func TestClientNoCookiesReq37Req86(t *testing.T) {
	var cookies atomic.Int64
	s := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			cookies.Add(1)
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "x", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/final", http.StatusFound)
		}
	})
	c := loopbackGuard(t).Client(ClientOptions{TLS: rootsFor(s.Server), MaxRedirects: 3})
	defer c.CloseIdleConnections()
	for range 2 {
		if _, _, err := get(t, c, s.URL+"/redirect"); err != nil {
			t.Fatal(err)
		}
	}
	if cookies.Load() != 0 {
		t.Fatalf("%d requests carried a cookie", cookies.Load())
	}
}

// TestClientTLSVerifiedReq82 checks that verification cannot be turned off
// and that the minimum version is raised to TLS 1.2.
func TestClientTLSVerifiedReq82(t *testing.T) {
	s := newTLSServer(t, func(http.ResponseWriter, *http.Request) {})
	insecure := &tls.Config{MinVersion: tls.VersionTLS10} //nolint:gosec // The client must raise it; this test checks that.
	insecure.InsecureSkipVerify = true
	c := loopbackGuard(t).Client(ClientOptions{TLS: insecure})
	defer c.CloseIdleConnections()
	_, _, err := get(t, c, s.URL)
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("fetch with an untrusted certificate = %v, want x509.UnknownAuthorityError", err)
	}
	if s.hits.Load() != 0 {
		t.Fatal("request sent over an unverified connection")
	}
	if !insecure.InsecureSkipVerify {
		t.Fatal("the caller's configuration must not be modified")
	}
	tr := c.Transport.(*transport).base
	if tr.TLSClientConfig.InsecureSkipVerify || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("transport TLS: skip %v min %x", tr.TLSClientConfig.InsecureSkipVerify, tr.TLSClientConfig.MinVersion)
	}
	if tr.Proxy != nil {
		t.Fatal("proxy variables must be ignored without env-proxy")
	}
	if tr.MaxResponseHeaderBytes != MaxResponseHeaderBytes {
		t.Fatalf("header cap %d", tr.MaxResponseHeaderBytes)
	}
	var nilGuard *Guard
	dc := nilGuard.Client(ClientOptions{Timeout: time.Second})
	dtr := dc.Transport.(*transport).base
	if dtr.TLSClientConfig.MinVersion != tls.VersionTLS12 || dtr.TLSHandshakeTimeout != time.Second {
		t.Fatalf("default TLS: min %x handshake %v", dtr.TLSClientConfig.MinVersion, dtr.TLSHandshakeTimeout)
	}
}

// connectProxy is a CONNECT proxy that maps every target to one address and
// counts tunnels.
type connectProxy struct {
	*httptest.Server
	tunnels atomic.Int64
	wg      sync.WaitGroup
}

func newConnectProxy(t *testing.T, target string) *connectProxy {
	t.Helper()
	p := &connectProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		p.tunnels.Add(1)
		var d net.Dialer
		up, err := d.DialContext(r.Context(), "tcp", target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			_ = up.Close()
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			_ = up.Close()
			return
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
		p.wg.Add(2)
		go func() {
			defer p.wg.Done()
			_, _ = io.Copy(up, bufio.NewReader(rw))
			_ = up.Close()
		}()
		go func() {
			defer p.wg.Done()
			_, _ = io.Copy(conn, up)
			_ = conn.Close()
		}()
	}))
	t.Cleanup(func() {
		p.CloseClientConnections()
		p.Close()
		p.wg.Wait()
	})
	return p
}

// TestClientEnvProxyReq85 checks that proxy variables apply only with
// env-proxy and that the guard checks the proxy's address.
func TestClientEnvProxyReq85(t *testing.T) {
	s := newTLSServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	proxy := newConnectProxy(t, s.Listener.Addr().String())
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := rootsFor(s.Server)
	cfg.ServerName = "example.com"
	useProxy := func(*http.Request) (*url.URL, error) { return proxyURL, nil }

	// Without env-proxy the proxy function is never consulted: the name is
	// resolved locally, by a resolver that fails every lookup (no network).
	direct := loopbackGuard(t)
	direct.proxy = useProxy
	direct.resolver = failingResolver()
	dc := direct.Client(ClientOptions{TLS: cfg, Timeout: 2 * time.Second})
	defer dc.CloseIdleConnections()
	var dnsErr *net.DNSError
	if _, _, err := get(t, dc, "https://idp.invalid/jwks"); !errors.As(err, &dnsErr) {
		t.Fatalf("fetch without a proxy = %v, want a local resolution failure", err)
	}
	if proxy.tunnels.Load() != 0 {
		t.Fatal("proxy used without env-proxy")
	}

	// With env-proxy the tunnel goes through the proxy, whose loopback
	// address the allow list admits; the target name is resolved by the
	// proxy.
	g, err := ParseAllow("127.0.0.1/32,env-proxy")
	if err != nil {
		t.Fatal(err)
	}
	g.proxy = useProxy
	pc := g.Client(ClientOptions{TLS: cfg, Timeout: 5 * time.Second})
	defer pc.CloseIdleConnections()
	if _, body, err := get(t, pc, "https://idp.invalid/jwks"); err != nil || string(body) != "ok" {
		t.Fatalf("fetch through the proxy: %q, %v", body, err)
	}
	if proxy.tunnels.Load() != 1 {
		t.Fatalf("tunnels = %d, want 1", proxy.tunnels.Load())
	}

	// A proxy at a denied address is refused.
	denied, err := ParseAllow("env-proxy")
	if err != nil {
		t.Fatal(err)
	}
	denied.proxy = useProxy
	xc := denied.Client(ClientOptions{TLS: cfg, Timeout: 2 * time.Second})
	defer xc.CloseIdleConnections()
	if _, _, err := get(t, xc, "https://idp.invalid/jwks"); !errors.Is(err, ErrDenied) {
		t.Fatalf("fetch through a denied proxy = %v, want ErrDenied", err)
	}
	if proxy.tunnels.Load() != 1 {
		t.Fatal("the denied proxy was contacted")
	}
}

// TestClientTimeout blocks the handler until the test releases it, so only
// ClientOptions.Timeout can end the fetch.
func TestClientTimeout(t *testing.T) {
	release := make(chan struct{})
	s := newTLSServer(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	c := loopbackGuard(t).Client(ClientOptions{TLS: rootsFor(s.Server), Timeout: 100 * time.Millisecond})
	defer c.CloseIdleConnections()
	_, _, err := get(t, c, s.URL)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("fetch = %v, want a timeout", err)
	}
}

// failingResolver fails every lookup without contacting a DNS server.
func failingResolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("no DNS in tests")
		},
	}
}
