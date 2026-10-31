// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"crypto/tls"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

// certRef names a stateStore.tls file reference.
func certRef(name string) secret.Ref {
	return secret.Ref{Provider: "file", Name: "/etc/ruralz/redis/" + name}
}

// tlsSecrets is a fake secret.Store of PEM values; watches counts the
// watches open, when set.
type tlsSecrets struct {
	vals    map[secret.Ref][]byte
	watches *atomic.Int64
}

func (s tlsSecrets) Get(r secret.Ref) (secret.Value, bool) {
	b, ok := s.vals[r]
	if !ok {
		return secret.Value{}, false
	}
	return secret.NewValue(b), true
}

func (s tlsSecrets) Watch(secret.Ref, func(secret.Value) error) func() {
	if s.watches != nil {
		s.watches.Add(1)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if s.watches != nil {
				s.watches.Add(-1)
			}
		})
	}
}

// mtls is a CA, a server configuration for 127.0.0.1 that requires a
// client certificate of that CA, and stateStore.tls with the CA and a
// client certificate in a fake secrets table.
type mtls struct {
	server  *tls.Config
	tls     *statestore.TLS
	watches *atomic.Int64
}

func newMTLS(t *testing.T) mtls {
	t.Helper()
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ca.Server("127.0.0.1", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	server, err := srv.ServerTLS(ca)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := ca.Client("ruralzd")
	if err != nil {
		t.Fatal(err)
	}
	caRef, crtRef, keyRef := certRef("ca.crt"), certRef("tls.crt"), certRef("tls.key")
	w := &atomic.Int64{}
	sec := tlsSecrets{vals: map[secret.Ref][]byte{caRef: ca.CertPEM, crtRef: cl.ChainPEM, keyRef: cl.KeyPEM}, watches: w}
	return mtls{
		server:  server,
		tls:     &statestore.TLS{CACertificate: &caRef, ClientCertificate: &crtRef, ClientKey: &keyRef, Secrets: sec},
		watches: w,
	}
}

// TestStateStoreTLS: stateStore.tls verifies the server against its CA,
// without Options.RootCAs, presents its client certificate to a server
// that requires one, follows rotations through its watches and stops them
// at Close.
func TestStateStoreTLS(t *testing.T) {
	m := newMTLS(t)
	f := newFixture(t, fixtureOptions{
		fake: fakeOptions{tls: m.server, password: "pw"}, tls: m.tls,
		urlFor: func(s *fakeServer) string { return "rediss://:pw@" + s.addr },
	})
	f.ready()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil || !c.Done {
		t.Fatalf("call over mutual TLS: Done %v Err %v", c.Done, c.Err)
	}
	if st := f.d.Status(); st.Cleartext || st.Unauthenticated {
		t.Errorf("Status = %+v, want neither cleartext nor unauthenticated", st)
	}
	if m.watches.Load() == 0 {
		t.Fatal("stateStore.tls follows no rotation")
	}
	if err := f.d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := m.watches.Load(); n != 0 {
		t.Errorf("%d watches open after Close", n)
	}
}

// TestStateStoreTLSClientCertificateRequired: without stateStore.tls the
// server, which requires a client certificate the system roots cannot
// vouch for either, refuses every dial (RZ-STS-002), while the same
// server takes the CA and certificate of stateStore.tls.
func TestStateStoreTLSClientCertificateRequired(t *testing.T) {
	m := newMTLS(t)
	only := *m.tls
	only.ClientCertificate, only.ClientKey = nil, nil
	f := newFixture(t, fixtureOptions{
		fake: fakeOptions{tls: m.server, password: "pw"}, tls: &only,
		urlFor: func(s *fakeServer) string { return "rediss://:pw@" + s.addr },
	})
	f.ready()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != statestore.ErrFailed {
		t.Fatalf("call without a client certificate: Err %v, want RZ-STS-002", c.Err)
	}
	if len(f.srv.commands()) != 0 {
		t.Fatalf("commands crossed a refused handshake: %v", f.srv.commands())
	}
}

// TestSentinelTLS: under rediss:// the sentinel connections run TLS too,
// verified and authenticated with the TLS block, and the URL's
// credentials reach the sentinel, which takes them without a password of
// its own, in HELLO, while only the primary gets AUTH before the client's
// HELLO.
func TestSentinelTLS(t *testing.T) {
	m := newMTLS(t)
	f := newFixture(t, fixtureOptions{
		sentinel: 1, sentinelTLS: m.server, limits: quietLimits(),
		fake: fakeOptions{tls: m.server, password: "pw"}, tls: m.tls,
		sentinelURL: func(addr string) string { return "rediss://:pw@" + addr + "?master_set=main" },
	})
	f.ready()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil {
		t.Fatalf("call through a TLS sentinel: Err %v", c.Err)
	}
	if f.srv.count("AUTH") == 0 || f.sent.count("HELLO AUTH") == 0 || f.sent.count("AUTH") != 0 {
		t.Fatalf("AUTH on the primary %d; HELLO AUTH %d and AUTH %d on the sentinel; want some, some, none",
			f.srv.count("AUTH"), f.sent.count("HELLO AUTH"), f.sent.count("AUTH"))
	}
}
