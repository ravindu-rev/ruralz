// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

// Tests for spec 06 requirements 79 (Upstream TLS: verification against
// caCertificate or the system roots, ServerName from sni or the Endpoint
// host, IP literal SAN, optional client certificate, both-or-neither
// RZ-CFG-005, rotation reaching new connections) and 81 (OTLP).

type upstreamFixture struct {
	serverCA, clientCA *testCA
	server             *tls.Config
	store              *fakeStore
}

// newUpstreamFixture starts from an Upstream server certificate for
// up.internal and 127.0.0.1 that requires client certificates from
// clientCA.
func newUpstreamFixture(t *testing.T) *upstreamFixture {
	t.Helper()
	f := &upstreamFixture{serverCA: newCA(t, "upstream-ca"), clientCA: newCA(t, "node-client-ca"), store: newFakeStore()}
	srv := f.serverCA.issue(t, leafOpts{cn: "upstream", dns: []string{"up.internal", "alt.internal"}, ips: []net.IP{net.IPv4(127, 0, 0, 1)}})
	pair, err := tls.X509KeyPair(srv.certPEM, srv.keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	f.server = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    f.clientCA.pool(),
	}
	f.store.put(ref("up-ca"), f.serverCA.pem)
	cl := f.clientCA.issue(t, leafOpts{cn: "node-v1", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	f.store.put(ref("client.crt"), cl.certPEM)
	f.store.put(ref("client.key"), cl.keyPEM)
	return f
}

func (f *upstreamFixture) spec() ClientSpec {
	return ClientSpec{CACertificate: refp("up-ca"), ClientCertificate: refp("client.crt"), ClientKey: refp("client.key")}
}

func TestUpstreamVerificationReq79(t *testing.T) {
	f := newUpstreamFixture(t)
	up, err := Upstream(ClientSpec{CACertificate: refp("up-ca")}, f.store)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()

	// An IP literal Endpoint verifies the IP SAN and sends no SNI.
	cr, sr := handshake(t, f.server, up.ForHost("127.0.0.1"))
	if cr.err != nil || sr.err != nil {
		t.Fatalf("IP Endpoint: %v %v", cr.err, sr.err)
	}
	if sr.state.ServerName != "" {
		t.Fatalf("SNI %q sent for an IP literal", sr.state.ServerName)
	}
	if cr.state.Version != tls.VersionTLS13 {
		t.Fatalf("version %x", cr.state.Version)
	}
	// A DNS Endpoint host is the ServerName.
	cr, sr = handshake(t, f.server, up.ForHost("Up.Internal."))
	if cr.err != nil || sr.state.ServerName != "up.internal" {
		t.Fatalf("DNS Endpoint: %v, SNI %q", cr.err, sr.state.ServerName)
	}
	// A wrong host fails verification.
	if cr, _ := handshake(t, f.server, up.ForHost("other.internal")); cr.err == nil {
		t.Fatal("a certificate for another name was accepted")
	}
	// No client certificate configured: none presented.
	if len(sr.state.PeerCertificates) != 0 {
		t.Fatal("a client certificate was presented without clientCertificate")
	}

	// tls.sni overrides the Endpoint host for SNI and verification.
	withSNI, err := Upstream(ClientSpec{SNI: "alt.internal", CACertificate: refp("up-ca")}, f.store)
	if err != nil {
		t.Fatal(err)
	}
	defer withSNI.Close()
	cr, sr = handshake(t, f.server, withSNI.ForHost("127.0.0.1"))
	if cr.err != nil || sr.state.ServerName != "alt.internal" {
		t.Fatalf("sni: %v, SNI %q", cr.err, sr.state.ServerName)
	}

	// Without caCertificate the system roots apply, which do not hold the
	// test CA.
	sys, err := Upstream(ClientSpec{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cr, _ = handshake(t, f.server, sys.ForHost("127.0.0.1"))
	var unknown x509.UnknownAuthorityError
	if !errors.As(cr.err, &unknown) {
		t.Fatalf("system roots: %v, want x509.UnknownAuthorityError", cr.err)
	}
	if sys.Current().RootCAs != nil || sys.Current().GetClientCertificate != nil {
		t.Fatal("no caCertificate means system roots and no client certificate")
	}

	// A wrong CA fails (RZ-UP-002 in the Upstream layer).
	f.store.put(ref("other-ca"), newCA(t, "other").pem)
	wrong, err := Upstream(ClientSpec{CACertificate: refp("other-ca")}, f.store)
	if err != nil {
		t.Fatal(err)
	}
	if cr, _ := handshake(t, f.server, wrong.ForHost("127.0.0.1")); !errors.As(cr.err, &unknown) {
		t.Fatalf("wrong CA: %v", cr.err)
	}
	// TLS 1.0 and 1.1 servers are refused.
	old := f.server.Clone()
	old.MinVersion, old.MaxVersion = tls.VersionTLS10, tls.VersionTLS11
	if cr, _ := handshake(t, old, up.ForHost("127.0.0.1")); cr.err == nil {
		t.Fatal("a TLS 1.1 server was accepted")
	}
}

func TestUpstreamClientCertificateAndRotationReq79(t *testing.T) {
	f := newUpstreamFixture(t)
	up, err := Upstream(f.spec(), f.store)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	kept := up.Current() // a consumer that keeps one configuration (gRPC)
	peer := func(cfg *tls.Config) string {
		t.Helper()
		if cfg.ServerName == "" {
			cfg = cfg.Clone()
			cfg.ServerName = "up.internal"
		}
		cr, sr := handshake(t, f.server, cfg)
		if cr.err != nil || sr.err != nil {
			t.Fatalf("handshake: %v %v", cr.err, sr.err)
		}
		if len(sr.state.VerifiedChains) == 0 {
			t.Fatal("client certificate not verified by the server")
		}
		return sr.state.PeerCertificates[0].Subject.CommonName
	}
	if got := peer(up.ForHost("up.internal")); got != "node-v1" {
		t.Fatalf("client certificate %q", got)
	}

	// Renew the client pair in one poll: no failure, and new connections
	// present it, also through a kept configuration.
	v2 := f.clientCA.issue(t, leafOpts{cn: "node-v2", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if n := f.store.rotateTogether(update{ref("client.crt"), v2.certPEM}, update{ref("client.key"), v2.keyPEM}); n != 0 {
		t.Fatalf("pair renewed in one poll: %d failures, want 0", n)
	}
	if got := peer(up.ForHost("up.internal")); got != "node-v2" {
		t.Fatalf("after rotation: %q", got)
	}
	if got := peer(kept); got != "node-v2" {
		t.Fatalf("kept configuration after rotation: %q", got)
	}
	// Renewed in two separate polls, the new certificate does not match
	// the current key (a failure, the last pair kept) until the key's poll.
	v3 := f.clientCA.issue(t, leafOpts{cn: "node-v3", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if n := f.store.rotate(ref("client.crt"), v3.certPEM); n != 1 {
		t.Fatalf("certificate in its own poll: %d failures, want 1 (mismatch)", n)
	}
	if got := peer(kept); got != "node-v2" {
		t.Fatalf("after a mismatched certificate: %q", got)
	}
	if n := f.store.rotate(ref("client.key"), v3.keyPEM); n != 0 {
		t.Fatalf("key in its own poll: %d failures", n)
	}
	if got := peer(kept); got != "node-v3" {
		t.Fatalf("after the key's poll: %q", got)
	}

	// Rotate the CA bundle: garbage keeps the last pool; a new CA replaces
	// it for new connections (the old configuration is untouched).
	if n := f.store.rotate(ref("up-ca"), []byte("garbage")); n != 1 {
		t.Fatalf("garbage CA: %d failures", n)
	}
	if got := peer(up.ForHost("up.internal")); got != "node-v3" {
		t.Fatal("after a failed CA rotation")
	}
	before := up.Current()
	if n := f.store.rotate(ref("up-ca"), newCA(t, "replacement").pem); n != 0 {
		t.Fatalf("new CA: %d failures", n)
	}
	if up.Current() == before {
		t.Fatal("a CA rotation must publish a new configuration")
	}
	if cr, _ := handshake(t, f.server, up.ForHost("up.internal")); cr.err == nil {
		t.Fatal("the rotated CA should no longer trust the server")
	}
	if got := peer(before); got != "node-v3" {
		t.Fatalf("a previous configuration keeps its pool: %q", got)
	}

	up.Close()
	up.Close()
	if f.store.watching() != 0 {
		t.Fatalf("%d watches after Close", f.store.watching())
	}
	if err := up.rotate(slotCA, secret.NewValue(f.serverCA.pem)); err != nil {
		t.Fatal(err)
	}
}

// TestUpstreamClientPairOnePollReq79 renews the client pair in one
// resolver poll, whichever watcher runs first and with one PEM file as
// both clientCertificate and clientKey: no rotation failure is counted and
// the new pair is presented (spec 06 requirements 76 and 79).
func TestUpstreamClientPairOnePollReq79(t *testing.T) {
	tests := []struct {
		name     string
		combined bool
		keyFirst bool
	}{
		{name: "separate files, certificate watcher first"},
		{name: "separate files, key watcher first", keyFirst: true},
		{name: "one PEM file as certificate and key", combined: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUpstreamFixture(t)
			spec := f.spec()
			if tt.combined {
				first := f.clientCA.issue(t, leafOpts{cn: "node-v1", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
				f.store.put(ref("client.pem"), first.combinedPEM())
				spec.ClientCertificate, spec.ClientKey = refp("client.pem"), refp("client.pem")
			}
			up, err := Upstream(spec, f.store)
			if err != nil {
				t.Fatal(err)
			}
			defer up.Close()
			next := f.clientCA.issue(t, leafOpts{cn: "node-v2", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
			crt, key := update{*spec.ClientCertificate, next.certPEM}, update{*spec.ClientKey, next.keyPEM}
			var n int
			switch {
			case tt.combined:
				n = f.store.rotate(*spec.ClientCertificate, next.combinedPEM())
			case tt.keyFirst:
				n = f.store.rotateTogether(key, crt)
			default:
				n = f.store.rotateTogether(crt, key)
			}
			if n != 0 {
				t.Fatalf("pair renewed in one poll: %d rotation failures, want 0", n)
			}
			cr, sr := handshake(t, f.server, up.ForHost("up.internal"))
			if cr.err != nil || sr.err != nil {
				t.Fatalf("handshake: %v %v", cr.err, sr.err)
			}
			if got := sr.state.PeerCertificates[0].Subject.CommonName; got != "node-v2" {
				t.Fatalf("client certificate %q, want node-v2", got)
			}
		})
	}
}

// TestUpstreamClientPairSeparatePollsReq79 renews the client certificate
// in one poll and its key in the next: the failed certificate watch is
// registered again once the key completes the pair, so no
// secret_rotation_failed reason is left while the counted failure stays;
// a key that never arrives keeps the reason raised (spec 06 requirements
// 76 and 79).
func TestUpstreamClientPairSeparatePollsReq79(t *testing.T) {
	f := newUpstreamFixture(t)
	up, err := Upstream(f.spec(), f.store)
	if err != nil {
		t.Fatal(err)
	}
	presented := func() string { return up.pair.Load().Leaf.Subject.CommonName }
	v2 := f.clientCA.issue(t, leafOpts{cn: "node-v2", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if n := f.store.rotate(ref("client.crt"), v2.certPEM); n != 1 || f.store.raised() != 1 {
		t.Fatalf("certificate in its own poll: %d failures, %d raised; want 1 and 1", n, f.store.raised())
	}
	if n := f.store.rotate(ref("client.key"), v2.keyPEM); n != 0 {
		t.Fatalf("key in the next poll: %d failures", n)
	}
	if got := presented(); got != "node-v2" || f.store.raised() != 0 || f.store.failures.Load() != 1 {
		t.Fatalf("after the pair completed: %q, %d raised, %d failures; want node-v2, 0, 1", got, f.store.raised(), f.store.failures.Load())
	}
	if f.store.watching() != 3 {
		t.Fatalf("%d watches, want 3", f.store.watching())
	}

	// The key half fails first this time; a CA rotation does not clear it,
	// the certificate's poll does.
	v3 := f.clientCA.issue(t, leafOpts{cn: "node-v3", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	f.store.rotate(ref("client.key"), v3.keyPEM)
	if n := f.store.rotate(ref("up-ca"), newCA(t, "replacement").pem); n != 0 || f.store.raised() != 1 {
		t.Fatalf("CA rotation: %d failures, %d raised; want 0 and 1", n, f.store.raised())
	}
	f.store.rotate(ref("client.crt"), v3.certPEM)
	if got := presented(); got != "node-v3" || f.store.raised() != 0 {
		t.Fatalf("after the pair completed: %q, %d raised", got, f.store.raised())
	}

	// A key that never arrives keeps the reason raised.
	v4 := f.clientCA.issue(t, leafOpts{cn: "node-v4", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	f.store.rotate(ref("client.crt"), v4.certPEM)
	if got := presented(); got != "node-v3" || f.store.raised() != 1 {
		t.Fatalf("certificate without its key: %q, %d raised", got, f.store.raised())
	}
	up.Close()
	if f.store.watching() != 0 {
		t.Fatalf("%d watches after Close", f.store.watching())
	}
}

// TestUpstreamClientPairNewerSiblingValueReq79 is
// TestCertIndexNewerSiblingValueReq76 for the client pair: a pair formed
// with a half's last delivered value leaves that half's watch registered,
// so a newer value of it published in the same poll is examined, counted
// and raised; a pair formed with its current value registers it again and
// clears the reason (spec 06 requirements 76 and 79).
func TestUpstreamClientPairNewerSiblingValueReq79(t *testing.T) {
	for _, certFirst := range []bool{false, true} {
		name := "key alone, then certificate with a newer key"
		if certFirst {
			name = "certificate alone, then key with a newer certificate"
		}
		t.Run(name, func(t *testing.T) {
			f := newUpstreamFixture(t)
			up, err := Upstream(f.spec(), f.store)
			if err != nil {
				t.Fatal(err)
			}
			defer up.Close()
			presented := func() string { return up.pair.Load().Leaf.Subject.CommonName }
			client := leafOpts{eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
			client.cn = "node-v2"
			v2 := f.clientCA.issue(t, client)
			client.cn = "node-v3"
			v3 := f.clientCA.issue(t, client)
			alone, other := ref("client.key"), ref("client.crt")
			alone2, other2, alone3, other3 := v2.keyPEM, v2.certPEM, v3.keyPEM, v3.certPEM
			if certFirst {
				alone, other = other, alone
				alone2, other2, alone3, other3 = other2, alone2, other3, alone3
			}
			if n := f.store.rotate(alone, alone2); n != 1 || f.store.raised() != 1 {
				t.Fatalf("first half alone: %d failures, %d raised; want 1 and 1", n, f.store.raised())
			}
			if n := f.store.rotateTogether(update{other, other2}, update{alone, alone3}); n != 1 {
				t.Fatalf("other half with a newer first half: %d failures, want 1 (the newer value)", n)
			}
			if got, n, r := presented(), f.store.failures.Load(), f.store.raised(); got != "node-v2" || n != 2 || r != 1 {
				t.Fatalf("after the newer value: %q, %d failures, %d raised; want node-v2, 2 and 1", got, n, r)
			}
			if n := f.store.rotate(other, other3); n != 0 {
				t.Fatalf("other half matching the current value: %d failures", n)
			}
			if got, n, r, w := presented(), f.store.failures.Load(), f.store.raised(), f.store.watching(); got != "node-v3" || n != 2 || r != 0 || w != 3 {
				t.Fatalf("after the pair completed: %q, %d failures, %d raised, %d watches; want node-v3, 2, 0 and 3", got, n, r, w)
			}
		})
	}
}

func TestUpstreamErrorsReq79(t *testing.T) {
	f := newUpstreamFixture(t)
	f.store.put(ref("garbage"), []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"))
	f.store.put(ref("empty"), []byte{})
	other := f.clientCA.issue(t, leafOpts{cn: "other"})
	f.store.put(ref("other.key"), other.keyPEM)
	tests := []struct {
		name  string
		spec  ClientSpec
		store secret.Store
		code  string
		is    error
	}{
		{"certificate without key", ClientSpec{ClientCertificate: refp("client.crt")}, f.store, "RZ-CFG-005", nil},
		{"key without certificate", ClientSpec{ClientKey: refp("client.key")}, f.store, "RZ-CFG-005", nil},
		{"nil store", ClientSpec{CACertificate: refp("up-ca")}, nil, "RZ-CFG-026", nil},
		{"unresolved CA", ClientSpec{CACertificate: refp("missing")}, f.store, "RZ-CFG-026", nil},
		{"unresolved certificate", ClientSpec{ClientCertificate: refp("missing"), ClientKey: refp("client.key")}, f.store, "RZ-CFG-026", nil},
		{"unresolved key", ClientSpec{ClientCertificate: refp("client.crt"), ClientKey: refp("missing")}, f.store, "RZ-CFG-026", nil},
		{"garbage CA", ClientSpec{CACertificate: refp("garbage")}, f.store, "RZ-CFG-026", ErrCABundle},
		{"empty CA", ClientSpec{CACertificate: refp("empty")}, f.store, "RZ-CFG-026", ErrCABundle},
		{"mismatched key", ClientSpec{ClientCertificate: refp("client.crt"), ClientKey: refp("other.key")}, f.store, "RZ-CFG-026", ErrKeyPair},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := f.store.watching()
			_, err := Upstream(tt.spec, tt.store)
			if code, _ := errcode.CodeOf(err); code != tt.code {
				t.Fatalf("code %q, want %q (%v)", code, tt.code, err)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Fatalf("err %v, want %v", err, tt.is)
			}
			if strings.Contains(err.Error(), "PRIVATE KEY") {
				t.Fatal("the error repeats secret content")
			}
			if f.store.watching() != before {
				t.Fatal("a failed build left watches registered")
			}
		})
	}
}

// TestClientConfigRotationDuringBuild delivers rotations while watches are
// registered: they win over the values read by Get.
func TestClientConfigRotationDuringBuild(t *testing.T) {
	f := newUpstreamFixture(t)
	fresh := f.clientCA.issue(t, leafOpts{cn: "fresh", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	f.store.onWatch = func(r secret.Ref, fn func(secret.Value) error) {
		var err error
		switch r {
		case ref("client.crt"):
			err = fn(secret.NewValue(fresh.certPEM))
		case ref("client.key"):
			err = fn(secret.NewValue(fresh.keyPEM))
		case ref("up-ca"):
			err = fn(secret.NewValue(f.serverCA.pem))
		}
		if err != nil {
			t.Errorf("setup delivery: %v", err)
		}
	}
	up, err := Upstream(f.spec(), f.store)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	_, sr := handshake(t, f.server, up.ForHost("up.internal"))
	if sr.err != nil || sr.state.PeerCertificates[0].Subject.CommonName != "fresh" {
		t.Fatalf("server saw %v", sr.err)
	}
}

func TestOTLPReq81(t *testing.T) {
	o, err := OTLP(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := o.ForHost("collector.example.com")
	if c.RootCAs != nil || c.ServerName != "collector.example.com" || c.MinVersion != tls.VersionTLS12 {
		t.Fatalf("OTLP default: %+v", c)
	}
	f := newUpstreamFixture(t)
	spec := f.spec()
	o2, err := OTLP(&spec, f.store)
	if err != nil {
		t.Fatal(err)
	}
	defer o2.Close()
	cr, sr := handshake(t, f.server, o2.ForHost("up.internal"))
	if cr.err != nil || sr.state.PeerCertificates[0].Subject.CommonName != "node-v1" {
		t.Fatalf("OTLP with tls: %v", cr.err)
	}
	if _, err := OTLP(&ClientSpec{ClientKey: refp("client.key")}, f.store); err == nil {
		t.Fatal("OTLP key without certificate accepted")
	}
}

func TestClientCertificateWithoutPair(t *testing.T) {
	c := &ClientConfig{}
	got, err := c.clientCertificate(nil)
	if err != nil || len(got.Certificate) != 0 {
		t.Fatal("no pair must present no certificate")
	}
	if err := c.buildLocked(slot(99)); err != nil {
		t.Fatal(err)
	}
	if err := c.buildLocked(slotKey); err != nil {
		t.Fatal("no client certificate configured: nothing to build")
	}
}
