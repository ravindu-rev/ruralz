// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

// tlsServer returns a server configuration for 127.0.0.1 and localhost
// issued by a fresh test CA, and a pool trusting that CA.
func tlsServer(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Server("127.0.0.1", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := leaf.ServerTLS(nil)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, ca.Pool()
}

// TestStatusTransport checks the transport flags the Manager turns into
// the state_store cleartext hop and state_store_unauthenticated: a
// redis:// URL is cleartext and a loopback URL without a password
// unauthenticated; rediss:// with a password is neither.
func TestStatusTransport(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	if st := f.d.Status(); !st.Cleartext || !st.Unauthenticated {
		t.Fatalf("redis:// loopback without a password: %+v, want cleartext and unauthenticated", st)
	}
	cfg, roots := tlsServer(t)
	g := newFixture(t, fixtureOptions{
		fake: fakeOptions{tls: cfg, password: "pw"}, opts: Options{RootCAs: roots},
		urlFor: func(s *fakeServer) string { return "rediss://:pw@" + s.addr },
	})
	if st := g.d.Status(); st.Cleartext || st.Unauthenticated {
		t.Fatalf("rediss:// with a password: %+v, want neither", st)
	}
}

// TestStatusEvictionPolicy checks Status's eviction policy: yes for
// noeviction, no for another policy, unknown without the field, which the
// driver logs once.
func TestStatusEvictionPolicy(t *testing.T) {
	for _, tc := range []struct {
		policy string
		want   statestore.Tristate
		logs   int
	}{
		{noEviction, statestore.Yes, 0},
		{"allkeys-lru", statestore.No, 0},
		{"", statestore.Unknown, 1},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{fake: fakeOptions{info: memInfo(100, 10_000, tc.policy)}})
			f.ready()
			f.readingOf(100, 0)
			if got := f.d.Status().EvictionPolicy; got != tc.want {
				t.Fatalf("eviction policy %v, want %v", got, tc.want)
			}
			f.srv.setInfo(memInfo(101, 10_000, tc.policy))
			f.readingOf(101, time.Second)
			if got := f.logs.count("state store reading lacks maxmemory_policy; eviction policy unknown"); got != tc.logs {
				t.Fatalf("%d unknown-policy warnings, want %d", got, tc.logs)
			}
		})
	}
}

// TestTLSAndAuthentication checks rediss:// end to end: the server's
// certificate is verified against the roots for the dialed host, the
// connection authenticates with the URL's credentials before loading the
// library, and calls run over it.
func TestTLSAndAuthentication(t *testing.T) {
	cfg, roots := tlsServer(t)
	f := newFixture(t, fixtureOptions{
		fake: fakeOptions{tls: cfg, user: "ruralz", password: "s3cret"}, opts: Options{RootCAs: roots},
		urlFor: func(s *fakeServer) string { return "rediss://ruralz:s3cret@" + s.addr },
	})
	f.ready()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil || !c.Done {
		t.Fatalf("call over TLS: Done %v Err %v", c.Done, c.Err)
	}
	if f.srv.count("AUTH") == 0 {
		t.Fatal("the connection did not authenticate before loading the library")
	}
}

// TestTLSRefusesUntrustedServer checks that a server certificate the
// roots do not trust fails the dial, a connect failure, and no command
// crosses the connection.
func TestTLSRefusesUntrustedServer(t *testing.T) {
	cfg, _ := tlsServer(t)
	_, otherRoots := tlsServer(t)
	f := newFixture(t, fixtureOptions{
		fake: fakeOptions{tls: cfg, password: "pw"}, opts: Options{RootCAs: otherRoots},
		urlFor: func(s *fakeServer) string { return "rediss://:pw@" + s.addr },
	})
	f.ready()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != statestore.ErrFailed {
		t.Fatalf("call to an untrusted server: Err %v, want RZ-STS-002", c.Err)
	}
	if len(f.srv.commands()) != 0 {
		t.Fatalf("commands crossed an unverified TLS connection: %v", f.srv.commands())
	}
	if !strings.Contains(f.logs.text(), "error_type=tls") {
		t.Fatalf("the dial failure is not logged as tls:\n%s", f.logs.text())
	}
}

// TestWrongPasswordFailsCalls checks that a refused AUTH leaves the
// connection to the client's HELLO, which fails, so calls are RZ-STS-002
// and no script runs.
func TestWrongPasswordFailsCalls(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		fake:   fakeOptions{password: "right"},
		urlFor: func(s *fakeServer) string { return "redis://:wrong@" + s.addr },
	})
	f.ready()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != statestore.ErrFailed {
		t.Fatalf("call with a wrong password: Err %v, want RZ-STS-002", c.Err)
	}
	if f.srv.count("EVALSHA") != 0 || f.srv.count("SCRIPT LOAD") != 0 {
		t.Fatal("an unauthenticated connection loaded or ran a script")
	}
}
