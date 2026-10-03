// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tlsconf

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Handshake tests for spec 06 requirements 74 (TLS 1.3 default, 1.2 with
// exactly six suites, ALPN), 75 (SNI), 76 (rotation reaches new
// handshakes), 77 (handshake timeout, handshake outcome), 46 and 47 (client
// certificate request mode read per handshake and recorded on the
// connection).

type listenerFixture struct {
	*indexFixture
	ix    *CertIndex
	state atomic.Pointer[ListenerState]
}

func newListenerFixture(t *testing.T) *listenerFixture {
	t.Helper()
	f := &listenerFixture{indexFixture: newIndexFixture(t)}
	specs := []CertSpec{
		f.add(t, "a-default", leafOpts{dns: []string{"default.example.com"}, ips: []net.IP{net.IPv4(127, 0, 0, 1)}}),
		f.add(t, "b-api", leafOpts{dns: []string{"api.example.com"}}),
		f.add(t, "c-rsa", leafOpts{dns: []string{"rsa.example.com"}, key: "rsa2048"}),
	}
	ix, err := BuildCertIndex(specs, f.store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ix.Close)
	f.ix = ix
	f.set(t, ListenerOptions{})
	return f
}

func (f *listenerFixture) set(t *testing.T, o ListenerOptions) {
	t.Helper()
	o.Certs = f.ix
	st, err := NewListenerState(o)
	if err != nil {
		t.Fatal(err)
	}
	f.state.Store(st)
}

func (f *listenerFixture) server() *tls.Config {
	return Server(ServerOptions{State: f.state.Load})
}

func TestListenerDefaultTLS13Req74(t *testing.T) {
	f := newListenerFixture(t)
	c := clientFor(f.ca, "api.example.com")
	c.NextProtos = []string{"h2", "http/1.1"}
	cr, sr := handshake(t, f.server(), c)
	if cr.err != nil || sr.err != nil {
		t.Fatalf("handshake: client %v server %v", cr.err, sr.err)
	}
	if cr.state.Version != tls.VersionTLS13 || cr.state.NegotiatedProtocol != "h2" {
		t.Fatalf("version %x ALPN %q", cr.state.Version, cr.state.NegotiatedProtocol)
	}
	if cn := cr.state.PeerCertificates[0].Subject.CommonName; cn != "b-api" {
		t.Fatalf("served %q", cn)
	}

	c12 := clientFor(f.ca, "api.example.com")
	c12.MaxVersion = tls.VersionTLS12
	cr, sr = handshake(t, f.server(), c12)
	if cr.err == nil || sr.err == nil {
		t.Fatal("a TLS 1.2 client must be refused by default")
	}

	h1 := clientFor(f.ca, "api.example.com")
	h1.NextProtos = []string{"http/1.1"}
	cr, _ = handshake(t, f.server(), h1)
	if cr.err != nil || cr.state.NegotiatedProtocol != "http/1.1" {
		t.Fatalf("http/1.1 ALPN: %v %q", cr.err, cr.state.NegotiatedProtocol)
	}
}

func TestListenerTLS12SuitesReq74(t *testing.T) {
	f := newListenerFixture(t)
	f.set(t, ListenerOptions{MinVersion: Version12})
	if st := f.state.Load(); st.MinVersion() != tls.VersionTLS12 || !slices.Equal(st.Config().CipherSuites, TLS12Suites()) {
		t.Fatalf("state: min %x suites %v", st.MinVersion(), st.Config().CipherSuites)
	}
	for _, suite := range TLS12Suites() {
		name, sni := tls.CipherSuiteName(suite), "api.example.com"
		if slices.Contains([]uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		}, suite) {
			sni = "rsa.example.com"
		}
		c := clientFor(f.ca, sni)
		c.MaxVersion = tls.VersionTLS12
		c.CipherSuites = []uint16{suite}
		cr, sr := handshake(t, f.server(), c)
		if cr.err != nil || sr.err != nil {
			t.Fatalf("%s: client %v server %v", name, cr.err, sr.err)
		}
		if cr.state.Version != tls.VersionTLS12 || cr.state.CipherSuite != suite {
			t.Fatalf("%s: negotiated %x %s", name, cr.state.Version, tls.CipherSuiteName(cr.state.CipherSuite))
		}
	}
	// A suite outside the six is refused.
	for _, suite := range []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA, tls.TLS_RSA_WITH_AES_128_GCM_SHA256} {
		c := clientFor(f.ca, "rsa.example.com")
		c.MaxVersion = tls.VersionTLS12
		c.CipherSuites = []uint16{suite}
		if cr, _ := handshake(t, f.server(), c); cr.err == nil {
			t.Fatalf("suite %s accepted", tls.CipherSuiteName(suite))
		}
	}
	// TLS 1.3 stays available.
	if cr, _ := handshake(t, f.server(), clientFor(f.ca, "api.example.com")); cr.err != nil || cr.state.Version != tls.VersionTLS13 {
		t.Fatalf("TLS 1.3 on a 1.2 listener: %v", cr.err)
	}
}

func TestListenerSNIAndRotationReq75Req76(t *testing.T) {
	f := newListenerFixture(t)
	for sni, want := range map[string]string{"api.example.com": "b-api", "rsa.example.com": "c-rsa", "default.example.com": "a-default"} {
		cr, _ := handshake(t, f.server(), clientFor(f.ca, sni))
		if cr.err != nil || cr.state.PeerCertificates[0].Subject.CommonName != want {
			t.Fatalf("SNI %s: %v", sni, cr.err)
		}
	}
	// No SNI (an IP literal ServerName sends none): the first by name,
	// which carries the IP SAN.
	cr, sr := handshake(t, f.server(), clientFor(f.ca, "127.0.0.1"))
	if cr.err != nil || sr.err != nil {
		t.Fatalf("no SNI: %v %v", cr.err, sr.err)
	}
	if sr.state.ServerName != "" || cr.state.PeerCertificates[0].Subject.CommonName != "a-default" {
		t.Fatalf("no SNI: SNI %q served %q", sr.state.ServerName, cr.state.PeerCertificates[0].Subject.CommonName)
	}

	// Rotation reaches the next handshake only.
	next := f.ca.issue(t, leafOpts{cn: "b-api-v2", dns: []string{"api.example.com"}})
	spec := CertSpec{Certificate: ref("b-api.crt"), PrivateKey: ref("b-api.key")}
	f.store.rotate(spec.Certificate, next.certPEM)
	f.store.rotate(spec.PrivateKey, next.keyPEM)
	cr, _ = handshake(t, f.server(), clientFor(f.ca, "api.example.com"))
	if cr.err != nil || cr.state.PeerCertificates[0].Subject.CommonName != "b-api-v2" {
		t.Fatalf("after rotation: %v", cr.err)
	}
	if n := f.store.rotate(spec.PrivateKey, []byte("junk")); n != 1 {
		t.Fatalf("invalid rotation: %d failures", n)
	}
	cr, _ = handshake(t, f.server(), clientFor(f.ca, "api.example.com"))
	if cr.err != nil || cr.state.PeerCertificates[0].Subject.CommonName != "b-api-v2" {
		t.Fatalf("after an invalid rotation: %v", cr.err)
	}
}

// TestListenerClientCertModeReq46Req47 toggles the request mode through
// the published state, as a Hot Reload does, and checks the recorded mode.
func TestListenerClientCertModeReq46Req47(t *testing.T) {
	f := newListenerFixture(t)
	clientCA := newCA(t, "client-ca")
	cl := clientCA.issue(t, leafOpts{cn: "client", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	pair, err := tls.X509KeyPair(cl.certPEM, cl.keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	var asked atomic.Int64
	c := clientFor(f.ca, "api.example.com")
	c.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		asked.Add(1)
		return &pair, nil
	}

	cr, sr := handshake(t, f.server(), c)
	if cr.err != nil || sr.err != nil || asked.Load() != 0 || sr.conn.ClientCertRequested() {
		t.Fatalf("no request mode: asked %d, recorded %v", asked.Load(), sr.conn.ClientCertRequested())
	}

	f.set(t, ListenerOptions{RequestClientCert: true})
	if !f.state.Load().RequestClientCert() || f.state.Load().Certs() != f.ix {
		t.Fatal("state accessors")
	}
	cr, sr = handshake(t, f.server(), c)
	if cr.err != nil || sr.err != nil {
		t.Fatalf("request mode: client %v server %v", cr.err, sr.err)
	}
	if asked.Load() != 1 || !sr.conn.ClientCertRequested() {
		t.Fatalf("request mode: asked %d, recorded %v", asked.Load(), sr.conn.ClientCertRequested())
	}
	if len(sr.state.PeerCertificates) != 1 || sr.state.PeerCertificates[0].Subject.CommonName != "client" {
		t.Fatal("the unverified client certificate should reach the server state")
	}
	if len(sr.state.VerifiedChains) != 0 {
		t.Fatal("the listener must not verify client certificates (auth.mtls does)")
	}

	// Requested but none sent: the handshake succeeds, auth.mtls answers 401.
	none := clientFor(f.ca, "api.example.com")
	cr, sr = handshake(t, f.server(), none)
	if cr.err != nil || sr.err != nil || !sr.conn.ClientCertRequested() || len(sr.state.PeerCertificates) != 0 {
		t.Fatalf("no certificate sent: %v %v", cr.err, sr.err)
	}

	f.set(t, ListenerOptions{})
	_, sr = handshake(t, f.server(), c)
	if sr.conn.ClientCertRequested() || asked.Load() != 1 {
		t.Fatal("the mode must follow the published state")
	}
}

// TestListenerResumptionReq46 checks that a request-mode listener never
// resumes a session (which would skip the certificate request), while the
// default mode does.
func TestListenerResumptionReq46(t *testing.T) {
	for _, request := range []bool{false, true} {
		f := newListenerFixture(t)
		f.set(t, ListenerOptions{RequestClientCert: request})
		srv := f.server()
		c := clientFor(f.ca, "api.example.com")
		c.ClientSessionCache = tls.NewLRUClientSessionCache(4)
		if cr, _ := handshake(t, srv, c); cr.err != nil {
			t.Fatal(cr.err)
		}
		cr, sr := handshake(t, srv, c)
		if cr.err != nil {
			t.Fatal(cr.err)
		}
		if cr.state.DidResume == request {
			t.Fatalf("request mode %v: DidResume %v", request, cr.state.DidResume)
		}
		if request && !sr.conn.ClientCertRequested() {
			t.Fatal("request not recorded")
		}
	}
}

func TestListenerNoStateReq77(t *testing.T) {
	f := newListenerFixture(t)
	_, sr := handshake(t, Server(ServerOptions{State: func() *ListenerState { return nil }}), clientFor(f.ca, "api.example.com"))
	if !errors.Is(sr.err, ErrNoListenerState) {
		t.Fatalf("nil state: %v", sr.err)
	}
	_, sr = handshake(t, Server(ServerOptions{}), clientFor(f.ca, "api.example.com"))
	if !errors.Is(sr.err, ErrNoListenerState) {
		t.Fatalf("no State function: %v", sr.err)
	}
}

// TestListenerHandshakeOutcomeReq77 checks what the listener measures
// requirement 77 on: the server's own HandshakeContext result. A client
// that rejects the server certificate fails it (counted tls_failure, no
// success observed), in TLS 1.3 and 1.2 and with or without a client
// certificate request; a trusting client completes it. The configuration
// sets no VerifyConnection, which runs before the client's last flight and
// so is no success signal, and hands out the shared per-state
// configuration unmodified.
func TestListenerHandshakeOutcomeReq77(t *testing.T) {
	f := newListenerFixture(t)
	srv := Server(ServerOptions{State: f.state.Load})
	if srv.VerifyConnection != nil {
		t.Fatal("the Server configuration must not hook VerifyConnection")
	}
	untrusting := func(maxVersion uint16) *tls.Config {
		return &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "api.example.com", MinVersion: tls.VersionTLS12, MaxVersion: maxVersion}
	}
	trusting := func(maxVersion uint16) *tls.Config {
		c := clientFor(f.ca, "api.example.com")
		c.MaxVersion = maxVersion
		return c
	}
	tests := []struct {
		name       string
		minVersion string
		request    bool
		client     func(uint16) *tls.Config
		maxVersion uint16
		success    bool
	}{
		{"1.3 rejected", "", false, untrusting, tls.VersionTLS13, false},
		{"1.3 rejected with client certificate request", "", true, untrusting, tls.VersionTLS13, false},
		{"1.2 rejected", "1.2", false, untrusting, tls.VersionTLS12, false},
		{"1.3 completed", "", false, trusting, tls.VersionTLS13, true},
		{"1.2 completed", "1.2", true, trusting, tls.VersionTLS12, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.set(t, ListenerOptions{MinVersion: tt.minVersion, RequestClientCert: tt.request})
			cfg, err := srv.GetConfigForClient(&tls.ClientHelloInfo{})
			if err != nil || cfg != f.state.Load().Config() || cfg.VerifyConnection != nil {
				t.Fatalf("per-state configuration replaced or modified (%v)", err)
			}
			// sr.err is the server's HandshakeContext result: nil is the
			// only success the listener observes.
			cr, sr := handshake(t, srv, tt.client(tt.maxVersion))
			if tt.success {
				if cr.err != nil || sr.err != nil {
					t.Fatalf("trusting client: %v, %v", cr.err, sr.err)
				}
				if sr.state.Version != tt.maxVersion {
					t.Fatalf("version %x", sr.state.Version)
				}
				return
			}
			var unknown x509.UnknownAuthorityError
			if !errors.As(cr.err, &unknown) {
				t.Fatalf("client error %v, want x509.UnknownAuthorityError", cr.err)
			}
			if sr.err == nil {
				t.Fatal("a handshake the client rejected completed on the server (observed as a success)")
			}
		})
	}
}

func TestNewListenerStateErrors(t *testing.T) {
	if _, err := NewListenerState(ListenerOptions{MinVersion: "1.1", Certs: &CertIndex{}}); err == nil {
		t.Fatal("minVersion 1.1 accepted")
	}
	_, err := NewListenerState(ListenerOptions{})
	if code, _ := errcode.CodeOf(err); code != "RZ-CFG-005" {
		t.Fatalf("nil Certs: %v", err)
	}
}

type unwrapConn struct{ net.Conn }

func (u unwrapConn) NetConn() net.Conn { return u.Conn }

type plainConn struct{ net.Conn }

func TestRecordClientCertModeUnwrap(t *testing.T) {
	rc := &recConn{}
	recordClientCertMode(unwrapConn{unwrapConn{rc}}, true)
	if !rc.ClientCertRequested() {
		t.Fatal("the recorder behind two NetConn layers was not found")
	}
	recordClientCertMode(plainConn{}, true) // no recorder: no panic
	recordClientCertMode(nil, true)
	recordClientCertMode(unwrapConn{nil}, true)
	var deep net.Conn = rc
	for range maxUnwrap + 1 {
		deep = unwrapConn{deep}
	}
	rc.SetClientCertRequested(false)
	recordClientCertMode(deep, true)
	if rc.ClientCertRequested() {
		t.Fatal("the unwrap depth must be bounded")
	}
}

// TestServerWithHTTPServerReq74Req77 serves HTTP/2 through net/http with the
// Server configuration, the handshake timeout as ReadHeaderTimeout.
func TestServerWithHTTPServerReq74Req77(t *testing.T) {
	f := newListenerFixture(t)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, strconv.Itoa(r.ProtoMajor))
		}),
		TLSConfig:         f.server(),
		ReadHeaderTimeout: HandshakeTimeout,
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(ln, "", "") }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("ServeTLS: %v", err)
		}
	}()
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   clientFor(f.ca, "api.example.com"),
		ForceAttemptHTTP2: true,
	}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+ln.Addr().String()+"/", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "2" {
		t.Fatalf("served over %q", body)
	}
}
