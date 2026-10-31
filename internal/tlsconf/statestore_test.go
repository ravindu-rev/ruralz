// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// TestStateStoreClient: stateStore.tls verifies each dialed State Store
// host against caCertificate, ignores an SNI (the block has none, and a
// cluster's nodes differ), presents the client certificate, takes the
// rotated CA on new connections, and refuses a certificate without its
// key (RZ-CFG-005) or an unresolved value (RZ-CFG-026).
func TestStateStoreClient(t *testing.T) {
	f := newUpstreamFixture(t)
	f.server.ClientAuth = tls.RequireAndVerifyClientCert
	spec := f.spec()
	spec.SNI = "alt.internal"
	ss, err := StateStoreClient(spec, f.store)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	if ss.Current().ServerName != "" {
		t.Fatalf("ServerName %q kept from an SNI", ss.Current().ServerName)
	}
	cr, sr := handshake(t, f.server, ss.ForHost("127.0.0.1"))
	if cr.err != nil || sr.err != nil {
		t.Fatalf("IP host: %v %v", cr.err, sr.err)
	}
	if sr.state.ServerName != "" || len(sr.state.PeerCertificates) != 1 || sr.state.PeerCertificates[0].Subject.CommonName != "node-v1" {
		t.Fatalf("SNI %q, client certificates %d", sr.state.ServerName, len(sr.state.PeerCertificates))
	}
	cr, sr = handshake(t, f.server, ss.ForHost("up.internal"))
	if cr.err != nil || sr.state.ServerName != "up.internal" {
		t.Fatalf("DNS host: %v, SNI %q", cr.err, sr.state.ServerName)
	}
	if cr, _ := handshake(t, f.server, ss.ForHost("other.internal")); cr.err == nil {
		t.Fatal("a certificate for another host was accepted")
	}

	// A rotated CA reaches the next connection.
	other := newCA(t, "other")
	f.store.rotate(ref("up-ca"), other.pem)
	var unknown x509.UnknownAuthorityError
	if cr, _ := handshake(t, f.server, ss.ForHost("127.0.0.1")); !errors.As(cr.err, &unknown) {
		t.Fatalf("after the CA rotation: %v, want x509.UnknownAuthorityError", cr.err)
	}

	half := ClientSpec{ClientCertificate: refp("client.crt")}
	if _, err := StateStoreClient(half, f.store); err == nil {
		t.Error("a certificate without its key was accepted")
	} else if code, _ := errcode.CodeOf(err); code != codeSchema {
		t.Errorf("certificate without key: %s, want %s", code, codeSchema)
	}
	if _, err := StateStoreClient(ClientSpec{CACertificate: refp("missing-ca")}, f.store); err == nil {
		t.Error("an unresolved CA was accepted")
	} else if code, _ := errcode.CodeOf(err); code != codeResolution {
		t.Errorf("unresolved CA: %s, want %s", code, codeResolution)
	}
}
