// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Tests for spec 06 requirements 74 (versions and suites, no field
// disables verification), 80 (State Store client), 82 (IdP client) and
// OQ-tech-stack-and-libraries-25 (system roots check).

func TestTLS12SuitesReq74(t *testing.T) {
	want := []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	}
	got := TLS12Suites()
	if !slices.Equal(got, want) {
		t.Fatalf("TLS12Suites() = %v, want exactly %v", got, want)
	}
	got[0] = 0
	if TLS12Suites()[0] != want[0] {
		t.Fatal("TLS12Suites must return a fresh copy")
	}
	insecure := tls.InsecureCipherSuites()
	for _, id := range want {
		for _, s := range insecure {
			if s.ID == id {
				t.Fatalf("suite %s is insecure", s.Name)
			}
		}
	}
	if !slices.Equal(NextProtos(), []string{"h2", "http/1.1"}) {
		t.Fatalf("NextProtos() = %v", NextProtos())
	}
}

func TestParseMinVersionReq74(t *testing.T) {
	tests := []struct {
		in   string
		want uint16
		err  bool
	}{
		{"", tls.VersionTLS13, false},
		{"1.3", tls.VersionTLS13, false},
		{"1.2", tls.VersionTLS12, false},
		{"1.1", 0, true},
		{"1.0", 0, true},
		{"TLS1.3", 0, true},
		{" 1.3", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseMinVersion(tt.in)
		if tt.err {
			if code, _ := errcode.CodeOf(err); code != "RZ-CFG-005" {
				t.Errorf("ParseMinVersion(%q) = %v, want RZ-CFG-005", tt.in, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseMinVersion(%q) = %x, %v", tt.in, got, err)
		}
	}
}

func TestClientBuildersReq80Req82(t *testing.T) {
	pool := x509.NewCertPool()
	c := Client(pool)
	if c.MinVersion != tls.VersionTLS12 || c.MaxVersion != tls.VersionTLS13 || c.RootCAs != pool ||
		!slices.Equal(c.CipherSuites, TLS12Suites()) || c.Renegotiation != tls.RenegotiateNever {
		t.Fatalf("Client: %+v", c)
	}
	idp := IdentityProvider()
	if idp.RootCAs != nil || idp.MinVersion != tls.VersionTLS12 || idp.ServerName != "" {
		t.Fatal("IdP must use the system roots and TLS 1.2 or newer")
	}
	tests := map[string]string{
		"redis.example.com":  "redis.example.com",
		"Redis.Example.COM.": "redis.example.com",
		"10.0.0.7":           "10.0.0.7",
		"[2001:db8::7]":      "2001:db8::7",
		"fe80::1%eth0":       "fe80::1",
	}
	for host, want := range tests {
		s := StateStore(host)
		if s.ServerName != want || s.RootCAs != nil || s.MinVersion != tls.VersionTLS12 {
			t.Errorf("StateStore(%q): ServerName %q", host, s.ServerName)
		}
	}
}

func TestCheckRootsOQTech25(t *testing.T) {
	loadErr := errors.New("boom")
	if err := checkRoots(func() (*x509.CertPool, error) { return nil, loadErr }); !errors.Is(err, ErrNoSystemRoots) || !errors.Is(err, loadErr) {
		t.Fatalf("load error: %v", err)
	}
	if err := checkRoots(func() (*x509.CertPool, error) { return x509.NewCertPool(), nil }); !errors.Is(err, ErrNoSystemRoots) {
		t.Fatalf("empty pool: %v", err)
	}
	if err := checkRoots(func() (*x509.CertPool, error) { return nil, nil }); !errors.Is(err, ErrNoSystemRoots) {
		t.Fatalf("nil pool: %v", err)
	}
	ca := newCA(t, "roots")
	if err := checkRoots(func() (*x509.CertPool, error) { return ca.pool(), nil }); err != nil {
		t.Fatalf("non-empty pool: %v", err)
	}
	// The process result depends on the host; it must not panic.
	_ = CheckSystemRoots()
}

// TestNoInsecureSkipVerifyReq74Req79 checks every configuration a builder
// returns by reflection, and that no non-test source of the package ever
// assigns InsecureSkipVerify.
func TestNoInsecureSkipVerifyReq74Req79(t *testing.T) {
	ca := newCA(t, "ca")
	srv := ca.issue(t, leafOpts{cn: "s", dns: []string{"s.example.com"}})
	st := newFakeStore()
	st.put(ref("crt"), srv.certPEM)
	st.put(ref("key"), srv.keyPEM)
	st.put(ref("ca"), ca.pem)
	ix, err := BuildCertIndex([]CertSpec{{Name: "a", Certificate: ref("crt"), PrivateKey: ref("key")}}, st)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := NewListenerState(ListenerOptions{Certs: ix, RequestClientCert: true})
	if err != nil {
		t.Fatal(err)
	}
	up, err := Upstream(ClientSpec{CACertificate: refp("ca"), ClientCertificate: refp("crt"), ClientKey: refp("key")}, st)
	if err != nil {
		t.Fatal(err)
	}
	otlp, err := OTLP(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, dir, AdminCertFile, srv.certPEM)
	writeFile(t, dir, AdminKeyFile, srv.keyPEM)
	writeFile(t, dir, AdminClientCAFile, ca.pem)
	admin, err := Admin(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfgs := map[string]*tls.Config{
		"Client":        Client(nil),
		"IdP":           IdentityProvider(),
		"StateStore":    StateStore("h"),
		"Server":        Server(ServerOptions{State: func() *ListenerState { return ls }}),
		"ListenerState": ls.Config(),
		"Upstream":      up.Current(),
		"UpstreamHost":  up.ForHost("10.0.0.1"),
		"OTLP":          otlp.ForHost("collector"),
		"Admin":         admin,
	}
	for name, c := range cfgs {
		v := reflect.ValueOf(c).Elem()
		if v.FieldByName("InsecureSkipVerify").Bool() {
			t.Errorf("%s sets InsecureSkipVerify", name)
		}
		if c.MinVersion < tls.VersionTLS12 {
			t.Errorf("%s MinVersion %x", name, c.MinVersion)
		}
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok && id.Name == "InsecureSkipVerify" {
					t.Errorf("%s: InsecureSkipVerify in a composite literal", fset.Position(x.Pos()))
				}
			case *ast.SelectorExpr:
				if x.Sel.Name == "InsecureSkipVerify" {
					t.Errorf("%s: InsecureSkipVerify referenced", fset.Position(x.Pos()))
				}
			}
			return true
		})
	}
}

func writeFile(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServerNameHelper(t *testing.T) {
	tests := map[string]string{
		"Example.COM":         "example.com",
		"example.com.":        "example.com",
		"[::1]":               "::1",
		"127.0.0.1":           "127.0.0.1",
		"fe80::1%lo":          "fe80::1",
		"::ffff:10.0.0.1":     "::ffff:10.0.0.1",
		"upstream.svc.local.": "upstream.svc.local",
	}
	for in, want := range tests {
		if got := serverName(in); got != want {
			t.Errorf("serverName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseKeyPairWithoutLeaf covers the Leaf fallback used when
// GODEBUG=x509keypairleaf=0 leaves tls.Certificate.Leaf unset.
func TestParseKeyPairWithoutLeaf(t *testing.T) {
	t.Setenv("GODEBUG", "x509keypairleaf=0")
	l := newCA(t, "leafless").issue(t, leafOpts{cn: "x", dns: []string{"x.test"}})
	pair, err := parseKeyPair(l.certPEM, l.keyPEM)
	if err != nil || pair.Leaf == nil || pair.Leaf.Subject.CommonName != "x" {
		t.Fatalf("parseKeyPair: %v", err)
	}
}

func TestParseCertPool(t *testing.T) {
	a, b := newCA(t, "a"), newCA(t, "b")
	junkBlock := []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")
	badCert := []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	tests := []struct {
		name string
		in   []byte
		ok   bool
	}{
		{"one", a.pem, true},
		{"two", append(append([]byte{}, a.pem...), b.pem...), true},
		{"other blocks skipped", append(append([]byte{}, junkBlock...), a.pem...), true},
		{"text around", append(append([]byte("# bundle\n"), a.pem...), "trailer\n"...), true},
		{"empty", nil, false},
		{"only other blocks", junkBlock, false},
		{"one bad certificate", append(append([]byte{}, a.pem...), badCert...), false},
	}
	for _, tt := range tests {
		pool, err := parseCertPool(tt.in)
		if tt.ok != (err == nil) {
			t.Errorf("%s: %v", tt.name, err)
		}
		if err != nil && !errors.Is(err, ErrCABundle) {
			t.Errorf("%s: %v does not wrap ErrCABundle", tt.name, err)
		}
		if tt.ok && pool.Equal(x509.NewCertPool()) {
			t.Errorf("%s: empty pool", tt.name)
		}
	}
}

// FuzzCABundle feeds arbitrary bytes to the PEM parsers every rotation runs
// (spec 06 requirements 76 and 79): no panic, only the fixed errors.
func FuzzCABundle(f *testing.F) {
	ca := newCA(f, "fuzz")
	l := ca.issue(f, leafOpts{cn: "x", dns: []string{"x.test"}})
	f.Add(ca.pem, l.keyPEM)
	f.Add(l.certPEM, l.keyPEM)
	f.Add([]byte("-----BEGIN CERTIFICATE-----\n\n-----END CERTIFICATE-----\n"), []byte{})
	f.Add([]byte{}, []byte("-----BEGIN PRIVATE KEY-----\nAA==\n-----END PRIVATE KEY-----\n"))
	f.Fuzz(func(t *testing.T, a, b []byte) {
		if _, err := parseCertPool(a); err != nil && !errors.Is(err, ErrCABundle) {
			t.Fatalf("parseCertPool: unexpected error %v", err)
		}
		_, err := parseKeyPair(a, b)
		if err != nil && !errors.Is(err, ErrNoCertificate) && !errors.Is(err, ErrKeyPair) && !errors.Is(err, ErrKeyType) {
			t.Fatalf("parseKeyPair: unexpected error %v", err)
		}
	})
}
