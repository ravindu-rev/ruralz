// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tlsconf

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/secret"
)

// In-package PKI: every key and certificate is generated at test time
// (spec 06 section 6, "no private keys committed").

type testCA struct {
	cert *x509.Certificate
	key  crypto.Signer
	pem  []byte
}

// testSerial numbers every generated certificate uniquely per process
// (test-only state).
var testSerial atomic.Int64

func nextSerial() *big.Int { return big.NewInt(testSerial.Add(1)) }

func newCA(t testing.TB, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          nextSerial(),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

type leafOpts struct {
	cn    string
	dns   []string
	ips   []net.IP
	eku   []x509.ExtKeyUsage
	noEKU bool
	key   string // p256 (default), p384, p224, rsa2048, rsa1024, ed25519
}

type leaf struct {
	certPEM, keyPEM []byte
	cert            *x509.Certificate
}

func genKey(t testing.TB, kind string) crypto.Signer {
	t.Helper()
	var (
		k   crypto.Signer
		err error
	)
	switch kind {
	case "", "p256":
		k, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "p384":
		k, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case "p224":
		k, err = ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	case "rsa2048":
		k, err = rsa.GenerateKey(rand.Reader, 2048)
	case "rsa1024":
		k, err = rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // A refused key size is the point of the test.
	case "ed25519":
		_, k, err = ed25519.GenerateKey(rand.Reader)
	default:
		t.Fatalf("unknown key kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (ca *testCA) issue(t testing.TB, o leafOpts) leaf {
	t.Helper()
	key := genKey(t, o.key)
	eku := o.eku
	if eku == nil && !o.noEKU {
		eku = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	ku := x509.KeyUsageDigitalSignature
	if _, ok := key.(*rsa.PrivateKey); ok {
		ku |= x509.KeyUsageKeyEncipherment
	}
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(),
		Subject:      pkix.Name{CommonName: o.cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     o.dns,
		IPAddresses:  o.ips,
		ExtKeyUsage:  eku,
		KeyUsage:     ku,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return leaf{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}),
		cert:    cert,
	}
}

// fakeStore is a secret.Store whose values the test sets and rotates. A
// rotation updates the value, then calls the watchers outside the lock and
// counts their errors as rotation failures, as the resolver does.
type fakeStore struct {
	mu       sync.Mutex
	vals     map[secret.Ref]secret.Value
	watchers map[secret.Ref]map[int]func(secret.Value) error
	next     int
	failures atomic.Int64
	// onWatch, when set, runs after a registration (outside the lock),
	// to simulate a rotation racing a build.
	onWatch func(r secret.Ref, fn func(secret.Value) error)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		vals:     map[secret.Ref]secret.Value{},
		watchers: map[secret.Ref]map[int]func(secret.Value) error{},
	}
}

func ref(name string) secret.Ref { return secret.Ref{Provider: "file", Name: "/etc/ruralz/" + name} }

func refp(name string) *secret.Ref {
	r := ref(name)
	return &r
}

func (s *fakeStore) Get(r secret.Ref) (secret.Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vals[r]
	return v, ok
}

func (s *fakeStore) Watch(r secret.Ref, fn func(secret.Value) error) func() {
	s.mu.Lock()
	id := s.next
	s.next++
	if s.watchers[r] == nil {
		s.watchers[r] = map[int]func(secret.Value) error{}
	}
	s.watchers[r][id] = fn
	hook := s.onWatch
	s.mu.Unlock()
	if hook != nil {
		hook(r, fn)
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.watchers[r], id)
	}
}

func (s *fakeStore) put(r secret.Ref, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[r] = secret.NewValue(b)
}

// rotate sets a new value and fires the watchers (one reference changed
// in one poll); it returns the number of watcher errors (rotation
// failures).
func (s *fakeStore) rotate(r secret.Ref, b []byte) int {
	return s.rotateTogether(update{r, b})
}

// update is one changed reference of rotateTogether.
type update struct {
	ref secret.Ref
	val []byte
}

// rotateTogether is one resolver poll that changed several references: it
// sets every new value first, then fires the watchers of each reference in
// the order given (secret/resolver updates every cell before its fanout).
// It returns the number of watcher errors.
func (s *fakeStore) rotateTogether(us ...update) int {
	vals := make([]secret.Value, len(us))
	s.mu.Lock()
	for i, u := range us {
		vals[i] = secret.NewValue(u.val)
		s.vals[u.ref] = vals[i]
	}
	s.mu.Unlock()
	failed := 0
	for i, u := range us {
		failed += s.fire(u.ref, vals[i])
	}
	return failed
}

// deliver fires the watchers of r with b but leaves the value Get returns
// unchanged: a Store holding an older value than the one its watches
// follow (secret/resolver gives a new Store a new cell after a refused
// rotation, while a carried-over owner keeps the old Store).
func (s *fakeStore) deliver(r secret.Ref, b []byte) int {
	return s.fire(r, secret.NewValue(b))
}

// drop removes the value of r: Get reports false.
func (s *fakeStore) drop(r secret.Ref) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.vals, r)
}

// fire calls the watchers of r with v outside the lock and counts their
// errors.
func (s *fakeStore) fire(r secret.Ref, v secret.Value) int {
	s.mu.Lock()
	fns := make([]func(secret.Value) error, 0, len(s.watchers[r]))
	for _, fn := range s.watchers[r] {
		fns = append(fns, fn)
	}
	s.mu.Unlock()
	failed := 0
	for _, fn := range fns {
		if err := fn(v); err != nil {
			failed++
			s.failures.Add(1)
		}
	}
	return failed
}

// combinedPEM is one PEM file holding a leaf's certificate and key, used
// as both references of a pair.
func (l leaf) combinedPEM() []byte {
	return append(append([]byte{}, l.certPEM...), l.keyPEM...)
}

func (s *fakeStore) watching() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.watchers {
		n += len(m)
	}
	return n
}

// recConn is a listener connection wrapper recording the client
// certificate request mode.
type recConn struct {
	net.Conn
	ClientCertRequest
}

type serverResult struct {
	state tls.ConnectionState
	err   error
	conn  *recConn
}

type clientResult struct {
	state tls.ConnectionState
	err   error
}

// handshake runs one TLS handshake over loopback TCP: the server wraps the
// accepted connection in a recConn, and after a successful handshake
// writes one byte that the client reads (so TLS 1.3 session tickets are
// processed) before both close.
func handshake(t *testing.T, serverCfg, clientCfg *tls.Config) (clientResult, serverResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	res := make(chan serverResult, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			res <- serverResult{err: err}
			return
		}
		rc := &recConn{Conn: c}
		tc := tls.Server(rc, serverCfg)
		r := serverResult{conn: rc}
		r.err = tc.HandshakeContext(ctx)
		if r.err == nil {
			r.state = tc.ConnectionState()
			_, _ = tc.Write([]byte("x"))
		}
		_ = tc.Close()
		res <- r
	}()
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: clientCfg}
	var cr clientResult
	c, err := d.DialContext(ctx, "tcp4", ln.Addr().String())
	cr.err = err
	if err == nil {
		tc := c.(*tls.Conn)
		cr.state = tc.ConnectionState()
		buf := make([]byte, 1)
		if _, err := io.ReadFull(tc, buf); err != nil {
			cr.err = err
		}
		_ = c.Close()
	}
	return cr, <-res
}

// clientFor returns a client configuration trusting ca for serverName.
func clientFor(ca *testCA, serverName string) *tls.Config {
	return &tls.Config{RootCAs: ca.pool(), ServerName: serverName, MinVersion: tls.VersionTLS12}
}
