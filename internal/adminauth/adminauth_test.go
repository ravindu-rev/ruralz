// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminauth

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// Tests for spec 06 requirements 94 (authorization matrix), 95 (bearer
// tokens compared in constant time over SHA-256 digests, accepted only
// over TLS or from a loopback peer; client certificates verified against
// ca.crt with the clientAuth extended key usage; 401 problem documents
// with RZ-AUTH-001 or RZ-AUTH-002 and WWW-Authenticate) and 96 (/tap and
// /config/dump uses logged without the token); spec 04 requirement 70.

// adminPaths are every admin path of spec 04 requirement 69 plus an
// unknown one.
var adminPaths = []string{
	"/healthz", "/readyz", "/metrics", "/debug/pprof/", "/debug/pprof/heap", "/debug/snapshots",
	"/debug/upstreams", "/config/dump", "/tap", "/unknown", "/metrics/", "/",
}

// credConfig is one admin credential configuration of the matrix.
type credConfig struct {
	name              string
	metrics, operator bool
	clientCA          bool
}

// presented is what a request carries.
type presented uint8

const (
	presentNone presented = iota
	presentMetrics
	presentOperator
	presentWrong
	presentCert
	presentCertNoEKU
)

func (p presented) String() string {
	return [...]string{"no header", "metrics token", "operator token", "wrong token", "client cert", "cert without clientAuth"}[p]
}

// transport is how a request arrives.
type transport uint8

const (
	loopbackCleartext transport = iota
	remoteCleartext
	overTLS
)

func (tr transport) String() string {
	return [...]string{"loopback cleartext", "non-loopback cleartext", "TLS"}[tr]
}

// oracle is the authorization matrix of requirement 94 and the transport
// rule of requirement 95, written independently of the implementation.
func oracle(path string, c credConfig, p presented, tr transport) (code string, kind Kind) {
	if path == "/healthz" || path == "/readyz" {
		return "", KindNone
	}
	var have Kind
	switch p {
	case presentNone:
		return CodeMissing, KindNone
	case presentWrong:
		return CodeInvalid, KindNone
	case presentMetrics, presentOperator:
		if tr == remoteCleartext {
			return CodeInvalid, KindNone
		}
		if p == presentMetrics && c.metrics {
			have = KindMetrics
		}
		if p == presentOperator && c.operator {
			have = KindOperator
		}
	case presentCert:
		if c.clientCA {
			have = KindCertificate
		}
	case presentCertNoEKU:
	}
	if have == KindNone {
		return CodeInvalid, KindNone
	}
	if path == "/metrics" {
		return "", have
	}
	if c.operator && (have == KindOperator || have == KindCertificate) {
		return "", have
	}
	return CodeInvalid, KindNone
}

func TestAuthorizationMatrixReq94Req95(t *testing.T) {
	ca := newCA(t, "operators")
	clientLeaf, _ := ca.leaf(t, "alice", x509.ExtKeyUsageClientAuth)
	serverOnlyLeaf, _ := ca.leaf(t, "bob", x509.ExtKeyUsageServerAuth)
	dir := t.TempDir()
	metricsFile := writeFile(t, dir, "metrics", metricsToken+"\n")
	operatorFile := writeFile(t, dir, "operator", operatorToken)

	configs := []credConfig{
		{name: "no credential configured"},
		{name: "metrics token only", metrics: true},
		{name: "operator token only", operator: true},
		{name: "both tokens", metrics: true, operator: true},
		{name: "TLS client CA only", clientCA: true},
		{name: "metrics token and client CA", metrics: true, clientCA: true},
		{name: "operator token and client CA", operator: true, clientCA: true},
		{name: "everything", metrics: true, operator: true, clientCA: true},
	}
	cases := 0
	for _, c := range configs {
		s := Settings{TLSDir: tlsDir(t, ca, c.clientCA)}
		if c.metrics {
			s.MetricsTokenFile = metricsFile
		}
		if c.operator {
			s.TokenFile = operatorFile
		}
		a, err := New(s, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if a.Configured() != (c.metrics || c.operator || c.clientCA) {
			t.Fatalf("%s: Configured = %v", c.name, a.Configured())
		}
		for _, path := range adminPaths {
			for p := presentNone; p <= presentCertNoEKU; p++ {
				for tr := loopbackCleartext; tr <= overTLS; tr++ {
					if (p == presentCert || p == presentCertNoEKU) && tr != overTLS {
						continue // a certificate needs TLS
					}
					r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://admin.local"+path, nil)
					r.RemoteAddr = "192.0.2.10:40000"
					switch tr {
					case loopbackCleartext:
						r.RemoteAddr = "127.0.0.1:40000"
						r.TLS = nil
					case remoteCleartext:
						r.TLS = nil
					case overTLS:
						r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true}
					}
					switch p {
					case presentNone:
					case presentMetrics:
						r.Header.Set("Authorization", "Bearer "+metricsToken)
					case presentOperator:
						r.Header.Set("Authorization", "Bearer "+operatorToken)
					case presentWrong:
						r.Header.Set("Authorization", "Bearer "+wrongToken)
					case presentCert:
						r.TLS.PeerCertificates = []*x509.Certificate{clientLeaf, ca.cert}
						r.TLS.VerifiedChains = [][]*x509.Certificate{{clientLeaf, ca.cert}}
					case presentCertNoEKU:
						r.TLS.PeerCertificates = []*x509.Certificate{serverOnlyLeaf}
						r.TLS.VerifiedChains = [][]*x509.Certificate{{serverOnlyLeaf, ca.cert}}
					}
					wantCode, wantKind := oracle(path, c, p, tr)
					got, err := a.Authenticate(r)
					code, _ := errcode.CodeOf(err)
					name := fmt.Sprintf("%s / %s / %s / %s", c.name, path, p, tr)
					if code != wantCode || got.Kind != wantKind {
						t.Errorf("%s: got (%v, %q, %v), want (%v, %q)", name, got.Kind, code, err, wantKind, wantCode)
					}
					if wantKind == KindCertificate && got.Subject != clientLeaf.Subject.String() {
						t.Errorf("%s: subject %q", name, got.Subject)
					}
					if err != nil && errcode.Status(code) != http.StatusUnauthorized {
						t.Errorf("%s: status %d", name, errcode.Status(code))
					}
					cases++
				}
			}
		}
	}
	if cases < 1000 {
		t.Fatalf("only %d matrix cases ran", cases)
	}
}

func TestAuthenticateReasons(t *testing.T) {
	ca := newCA(t, "operators")
	leaf, _ := ca.leaf(t, "alice", x509.ExtKeyUsageClientAuth)
	dir := t.TempDir()
	s := Settings{MetricsTokenFile: writeFile(t, dir, "m", metricsToken)}
	metricsOnly, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.TokenFile = writeFile(t, dir, "o", operatorToken)
	s.TLSDir = tlsDir(t, ca, true)
	both, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tlsState := func(certs ...*x509.Certificate) *tls.ConnectionState {
		st := &tls.ConnectionState{}
		if len(certs) > 0 {
			st.PeerCertificates = certs
			st.VerifiedChains = [][]*x509.Certificate{append(certs, ca.cert)}
		}
		return st
	}
	tests := []struct {
		name   string
		a      *Authenticator
		path   string
		auth   []string
		remote string
		tls    *tls.ConnectionState
		code   string
		err    error
		kind   Kind
	}{
		{name: "no credential", a: both, path: "/tap", code: CodeMissing, err: ErrNoCredential},
		{name: "basic scheme is no credential", a: both, path: "/tap", auth: []string{"Basic b3A6cGFzcw=="}, code: CodeMissing, err: ErrNoCredential},
		{name: "two Authorization fields", a: both, path: "/metrics", auth: []string{"Bearer " + operatorToken, "Bearer " + operatorToken}, code: CodeInvalid, err: ErrMultipleAuthorization},
		{name: "empty bearer", a: both, path: "/metrics", auth: []string{"Bearer "}, code: CodeInvalid, err: ErrMalformedToken},
		{name: "scheme only", a: both, path: "/metrics", auth: []string{"Bearer"}, code: CodeInvalid, err: ErrMalformedToken},
		{name: "space in token", a: both, path: "/metrics", auth: []string{"Bearer op canary"}, code: CodeInvalid, err: ErrMalformedToken},
		{name: "oversized token", a: both, path: "/metrics", auth: []string{"Bearer " + strings.Repeat("a", MaxTokenBytes+1)}, code: CodeInvalid, err: ErrMalformedToken},
		{name: "lowercase scheme and extra spaces", a: both, path: "/tap", auth: []string{"bearer   " + operatorToken + " "}, kind: KindOperator},
		{name: "cleartext non-loopback", a: both, path: "/metrics", auth: []string{"Bearer " + metricsToken}, remote: "198.51.100.7:5000", code: CodeInvalid, err: ErrCleartext},
		{name: "cleartext mapped loopback", a: both, path: "/metrics", auth: []string{"Bearer " + metricsToken}, remote: "[::ffff:127.0.0.1]:5000", kind: KindMetrics},
		{name: "cleartext IPv6 loopback", a: both, path: "/tap", auth: []string{"Bearer " + operatorToken}, remote: "[::1]:5000", kind: KindOperator},
		{name: "TLS from anywhere", a: both, path: "/tap", auth: []string{"Bearer " + operatorToken}, remote: "198.51.100.7:5000", tls: tlsState(), kind: KindOperator},
		{name: "wrong token", a: both, path: "/metrics", auth: []string{"Bearer " + wrongToken}, code: CodeInvalid, err: ErrInvalidToken},
		{name: "metrics token on /tap", a: both, path: "/tap", auth: []string{"Bearer " + metricsToken}, code: CodeInvalid, err: ErrNotPermitted},
		{name: "metrics token on /tap without operator token", a: metricsOnly, path: "/tap", auth: []string{"Bearer " + metricsToken}, code: CodeInvalid, err: ErrOperatorOff},
		{name: "wrong token beats a valid certificate", a: both, path: "/tap", auth: []string{"Bearer " + wrongToken}, tls: tlsState(leaf), code: CodeInvalid, err: ErrInvalidToken},
		{name: "metrics token with a certificate on /tap", a: both, path: "/tap", auth: []string{"Bearer " + metricsToken}, tls: tlsState(leaf), kind: KindCertificate},
		{name: "certificate without ca.crt", a: metricsOnly, path: "/metrics", tls: tlsState(leaf), code: CodeInvalid, err: ErrCertificate},
		{name: "certificate with an empty chain", a: both, path: "/metrics", tls: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{}}}, code: CodeInvalid, err: ErrCertificate},
		{name: "certificate unverified", a: both, path: "/metrics", tls: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}, code: CodeInvalid, err: ErrCertificate},
		{name: "open path ignores a wrong token", a: both, path: "/healthz", auth: []string{"Bearer " + wrongToken}, remote: "198.51.100.7:5000"},
		{name: "path class is exact", a: metricsOnly, path: "/metrics/../tap", auth: []string{"Bearer " + metricsToken}, code: CodeInvalid, err: ErrOperatorOff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://admin.local/", nil)
			r.URL.Path = tt.path
			r.RemoteAddr = "127.0.0.1:1234"
			if tt.remote != "" {
				r.RemoteAddr = tt.remote
			}
			r.TLS = tt.tls
			for _, v := range tt.auth {
				r.Header.Add("Authorization", v)
			}
			p, err := tt.a.Authenticate(r)
			code, _ := errcode.CodeOf(err)
			if code != tt.code || p.Kind != tt.kind {
				t.Fatalf("Authenticate = (%+v, %v), want kind %v code %q", p, err, tt.kind, tt.code)
			}
			if tt.err != nil && !errors.Is(err, tt.err) {
				t.Fatalf("error %v, want %v", err, tt.err)
			}
			for _, tok := range []string{operatorToken, metricsToken, wrongToken} {
				if err != nil && strings.Contains(err.Error(), tok) {
					t.Fatalf("the error quotes a token: %v", err)
				}
			}
		})
	}
}

// TestCertificateAnchoredInCACrtReq95: a verified chain is admitted only
// when it ends at a certificate of ca.crt and starts at the presented
// leaf, whatever roots the TLS configuration verified it against.
func TestCertificateAnchoredInCACrtReq95(t *testing.T) {
	ca := newCA(t, "operators")
	other := newCA(t, "other-root")
	leaf, _ := ca.leaf(t, "alice", x509.ExtKeyUsageClientAuth)
	otherLeaf, _ := other.leaf(t, "mallory", x509.ExtKeyUsageClientAuth)
	a, err := New(Settings{TLSDir: tlsDir(t, ca, true)}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		peer   []*x509.Certificate
		chains [][]*x509.Certificate
		ok     bool
	}{
		{name: "anchored in ca.crt", peer: []*x509.Certificate{leaf}, chains: [][]*x509.Certificate{{leaf, ca.cert}}, ok: true},
		{name: "verified against another root", peer: []*x509.Certificate{otherLeaf}, chains: [][]*x509.Certificate{{otherLeaf, other.cert}}},
		{name: "ca.crt leaf verified against another root", peer: []*x509.Certificate{leaf}, chains: [][]*x509.Certificate{{leaf, other.cert}}},
		{name: "leaf alone", peer: []*x509.Certificate{leaf}, chains: [][]*x509.Certificate{{leaf}}},
		{name: "second chain anchored", peer: []*x509.Certificate{leaf}, chains: [][]*x509.Certificate{{leaf, other.cert}, {leaf, ca.cert}}, ok: true},
		{name: "chain of another leaf", peer: []*x509.Certificate{otherLeaf}, chains: [][]*x509.Certificate{{leaf, ca.cert}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://admin.local/metrics", nil)
			r.TLS = &tls.ConnectionState{PeerCertificates: tt.peer, VerifiedChains: tt.chains}
			p, err := a.Authenticate(r)
			if tt.ok {
				if err != nil || p.Kind != KindCertificate || p.Subject != leaf.Subject.String() {
					t.Fatalf("Authenticate = %+v, %v", p, err)
				}
				return
			}
			if code, _ := errcode.CodeOf(err); code != CodeInvalid || !errors.Is(err, ErrCertificate) {
				t.Fatalf("Authenticate = %+v, %v", p, err)
			}
		})
	}
}

func TestAuditEnabledReq96(t *testing.T) {
	p := writeFile(t, t.TempDir(), "o", operatorToken)
	quiet, err := New(Settings{TokenFile: p}, Options{})
	if err != nil || quiet.AuditEnabled() {
		t.Fatalf("without a logger: AuditEnabled = %v, %v", quiet.AuditEnabled(), err)
	}
	audited, err := New(Settings{TokenFile: p}, Options{Logger: newLogger(&logBuffer{})})
	if err != nil || !audited.AuditEnabled() {
		t.Fatalf("with a logger: AuditEnabled = %v, %v", audited.AuditEnabled(), err)
	}
}

func TestConstantTimeComparisonReq95(t *testing.T) {
	a, err := New(Settings{TokenFile: writeFile(t, t.TempDir(), "o", operatorToken)}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	last := operatorToken[len(operatorToken)-1]
	flipped := operatorToken[:len(operatorToken)-1] + string(last^1)
	for _, tok := range []string{
		flipped,                        // equal length, last byte differs
		"X" + operatorToken[1:],        // equal length, first byte differs
		operatorToken[:22],             // a prefix
		operatorToken + "a",            // one byte longer
		strings.ToUpper(operatorToken), // case matters
		operatorToken + "==",           // padding is part of the token
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tap", nil)
		r.RemoteAddr = "127.0.0.1:1"
		r.Header.Set("Authorization", "Bearer "+tok)
		if _, err := a.Authenticate(r); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("token %q: %v", tok, err)
		}
	}
	// The digests compared have one length whatever the token length.
	d := [32]byte{}
	if a.operator.match(&d) {
		t.Fatal("zero digest matched")
	}
}

func TestClassOf(t *testing.T) {
	tests := map[string]Class{
		"/healthz": ClassOpen, "/readyz": ClassOpen, "/metrics": ClassMetrics,
		"/tap": ClassOperator, "/config/dump": ClassOperator, "/debug/pprof/": ClassOperator,
		"/healthz/": ClassOperator, "/HEALTHZ": ClassOperator, "/metrics/": ClassOperator, "": ClassOperator,
		"//metrics": ClassOperator,
	}
	for path, want := range tests {
		if got := ClassOf(path); got != want {
			t.Errorf("ClassOf(%q) = %v, want %v", path, got, want)
		}
	}
	for c, want := range map[Class]string{ClassOpen: "open", ClassMetrics: "metrics", ClassOperator: "operator", 9: ""} {
		if c.String() != want {
			t.Errorf("Class(%d) = %q", c, c.String())
		}
	}
	for k, want := range map[Kind]string{KindNone: "none", KindMetrics: "metrics", KindOperator: "operator", KindCertificate: "certificate", 9: ""} {
		if k.String() != want {
			t.Errorf("Kind(%d) = %q", k, k.String())
		}
	}
	a := &Authenticator{}
	if !a.admits(ClassOpen, KindNone) || a.admits(Class(9), KindOperator) {
		t.Fatal("admits")
	}
}

func TestLoopbackReq95(t *testing.T) {
	tests := map[string]bool{
		"127.0.0.1:9901": true, "127.255.0.9:1": true, "[::1]:9901": true, "[::ffff:127.0.0.1]:1": true,
		"127.0.0.1": true, "::1": true, "::ffff:127.1.2.3": true, "[::1%lo]:9901": true,
		"10.0.0.1:1": false, "[::ffff:10.0.0.1]:1": false, "0.0.0.0:1": false, "[::]:1": false,
		"192.0.2.1:9901": false, "[2001:db8::1]:1": false, "[fe80::1%eth0]:1": false,
		"": false, "@": false, "localhost:9901": false, "garbage": false,
	}
	for addr, want := range tests {
		if got := Loopback(addr); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestMiddlewareReq95Req96(t *testing.T) {
	ca := newCA(t, "operators")
	leaf, _ := ca.leaf(t, "alice", x509.ExtKeyUsageClientAuth)
	dir := t.TempDir()
	logs := &logBuffer{}
	a, err := New(Settings{
		TokenFile:        writeFile(t, dir, "o", operatorToken),
		MetricsTokenFile: writeFile(t, dir, "m", metricsToken),
		TLSDir:           tlsDir(t, ca, true),
	}, Options{
		Logger:    newLogger(logs),
		RequestID: func(*http.Request) string { return "4bf92f3577b34da6a3ce929d0e0e4736" },
	})
	if err != nil {
		t.Fatal(err)
	}
	var seen *http.Request
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		w.WriteHeader(http.StatusNoContent)
	}))
	do := func(path, auth string, st *tls.ConnectionState) *httptest.ResponseRecorder {
		seen = nil
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://admin.local"+path, nil)
		r.RemoteAddr = "127.0.0.1:51000"
		r.TLS = st
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// Refusals are problem documents with the challenge.
	for _, tt := range []struct{ auth, code string }{{"", CodeMissing}, {"Bearer " + wrongToken, CodeInvalid}} {
		w := do("/tap", tt.auth, nil)
		if w.Code != http.StatusUnauthorized || seen != nil {
			t.Fatalf("%q: status %d, next called %v", tt.auth, w.Code, seen != nil)
		}
		if got := w.Header().Values("WWW-Authenticate"); len(got) != 1 || got[0] != `Bearer realm="ruralz-admin"` {
			t.Fatalf("WWW-Authenticate = %q", got)
		}
		if w.Header().Get("Content-Type") != problem.ContentType || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("headers %v", w.Header())
		}
		doc, err := problem.Decode(w.Body.Bytes())
		if err != nil || doc.Code != tt.code || doc.Status != 401 || doc.Title != "Unauthorized" || doc.RequestID != "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Fatalf("problem %+v, %v", doc, err)
		}
		if strings.Contains(w.Body.String(), wrongToken) {
			t.Fatal("the body echoes the token")
		}
	}

	// Open paths pass untouched, without a principal.
	if w := do("/readyz", "Bearer "+wrongToken, nil); w.Code != http.StatusNoContent || seen.Header.Get("Authorization") == "" {
		t.Fatalf("/readyz: %d", w.Code)
	}
	if _, ok := PrincipalFrom(seen.Context()); ok {
		t.Fatal("an open path has a principal")
	}

	// Admitted requests carry the principal and lose Authorization.
	if w := do("/metrics", "Bearer "+metricsToken, nil); w.Code != http.StatusNoContent {
		t.Fatalf("/metrics: %d", w.Code)
	}
	if p, ok := PrincipalFrom(seen.Context()); !ok || p.Kind != KindMetrics || seen.Header.Get("Authorization") != "" {
		t.Fatalf("principal %+v %v, Authorization %q", p, ok, seen.Header.Get("Authorization"))
	}
	if w := do("/tap", "Bearer "+operatorToken, nil); w.Code != http.StatusNoContent {
		t.Fatalf("/tap: %d", w.Code)
	}
	st := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf, ca.cert}}}
	if w := do("/config/dump", "", st); w.Code != http.StatusNoContent {
		t.Fatalf("/config/dump: %d", w.Code)
	}
	if p, _ := PrincipalFrom(seen.Context()); p.Kind != KindCertificate || !strings.Contains(p.Subject, "CN=alice") {
		t.Fatalf("principal %+v", p)
	}
	if w := do("/debug/snapshots", "Bearer "+operatorToken, nil); w.Code != http.StatusNoContent {
		t.Fatalf("/debug/snapshots: %d", w.Code)
	}

	// Requirement 96: exactly the /tap and /config/dump uses are logged at
	// info with path, peer and credential kind, never the token.
	access := logs.lines("admin access")
	if len(access) != 2 {
		t.Fatalf("access records:\n%s", logs.String())
	}
	for i, want := range []string{
		`"level":"INFO","msg":"admin access","path":"/tap","peer":"127.0.0.1:51000","credential":"operator"}`,
		`"level":"INFO","msg":"admin access","path":"/config/dump","peer":"127.0.0.1:51000","credential":"certificate","subject":"CN=alice,O=Ruralz Operators"}`,
	} {
		if !strings.Contains(access[i], want) {
			t.Errorf("record %d = %s, want %s", i, access[i], want)
		}
	}
	if refused := logs.lines("admin request refused"); len(refused) != 2 || !strings.Contains(refused[0], `"level":"DEBUG"`) || !strings.Contains(refused[1], CodeInvalid) {
		t.Errorf("refusal records:\n%s", logs.String())
	}
	for _, tok := range []string{operatorToken, metricsToken, wrongToken} {
		if strings.Contains(logs.String(), tok) {
			t.Fatalf("a log record holds a token:\n%s", logs.String())
		}
	}
}

func TestMiddlewareWithoutLoggerOrRequestID(t *testing.T) {
	a, err := New(Settings{TokenFile: writeFile(t, t.TempDir(), "o", operatorToken)}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tap", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	doc, _ := problem.Decode(w.Body.Bytes())
	if w.Code != http.StatusUnauthorized || doc.RequestID != "" || doc.Code != CodeMissing {
		t.Fatalf("status %d, %+v", w.Code, doc)
	}
	r.Header.Set("Authorization", "Bearer "+operatorToken)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
}

// FuzzBearer: the Authorization parser never panics and returns only
// b64token tokens within the size limit.
func FuzzBearer(f *testing.F) {
	for _, s := range []string{"Bearer " + operatorToken, "Bearer", "Basic x", "bearer  a==", "Bearer a b", "Bearer =", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		h := http.Header{}
		if v != "" {
			h.Set("Authorization", v)
		}
		tok, present, err := bearer(h)
		if err != nil && !errors.Is(err, ErrMalformedToken) {
			t.Fatalf("bearer(%q) error %v", v, err)
		}
		if err == nil && present != (tok != "") {
			t.Fatalf("bearer(%q) = %q, present %v", v, tok, present)
		}
		if tok != "" && (!b64token(tok) || len(tok) > MaxTokenBytes || !strings.Contains(v, tok)) {
			t.Fatalf("bearer(%q) = %q", v, tok)
		}
	})
}

func BenchmarkAuthenticateToken(b *testing.B) {
	dir := b.TempDir()
	a, err := New(Settings{TokenFile: dir + "/o", MetricsTokenFile: dir + "/m"}, Options{})
	if err == nil || a != nil {
		b.Fatal("missing files accepted")
	}
	for name, tok := range map[string]string{"o": operatorToken, "m": metricsToken} {
		if err := os.WriteFile(dir+"/"+name, []byte(tok), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	a, err = New(Settings{TokenFile: dir + "/o", MetricsTokenFile: dir + "/m"}, Options{})
	if err != nil {
		b.Fatal(err)
	}
	r := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/metrics", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Authorization", "Bearer "+metricsToken)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := a.Authenticate(r); err != nil {
			b.Fatal(err)
		}
	}
}
