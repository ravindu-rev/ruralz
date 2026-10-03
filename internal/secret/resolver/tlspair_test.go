// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// tlsLeaf is a self-signed ECDSA P-256 certificate for a.test and its
// PKCS #8 key, in PEM.
type tlsLeaf struct{ cert, key string }

func newTLSLeaf(t *testing.T, cn string) tlsLeaf {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{"a.test"},
		NotBefore:    epoch().Add(-time.Hour),
		NotAfter:     epoch().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return tlsLeaf{
		cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		key:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})),
	}
}

// TestTLSPairNewerSiblingValue drives the internal/tlsconf certificate
// index and client configuration with real polls (spec 06 requirements 76
// and 79). One half is renewed alone and fails; one poll then renews the
// other half with a value that pairs with it and the first half again
// with a value that matches nothing. The pair is formed with the first
// half's last delivered value, so tlsconf keeps the first half's watch,
// which is delivered the newer value: a second failure is counted and
// secret_rotation_failed stays raised. A later value of the other half
// that pairs with the first half's current value lets tlsconf register
// that watch again (secret.Store.Watch from fn), which clears the reason.
// The poll delivers in reference order, so the file names put the other
// half first.
func TestTLSPairNewerSiblingValue(t *testing.T) {
	type follower struct {
		served func() string
		close  func()
	}
	builds := map[string]func(t *testing.T, st secret.Store, crt, key secret.Ref) follower{
		"CertIndex": func(t *testing.T, st secret.Store, crt, key secret.Ref) follower {
			ix, err := tlsconf.BuildCertIndex([]tlsconf.CertSpec{{Name: "main", Certificate: crt, PrivateKey: key}}, st)
			if err != nil {
				t.Fatal(err)
			}
			return follower{served: func() string { return ix.Lookup("a.test").Leaf.Subject.CommonName }, close: ix.Close}
		},
		"ClientConfig": func(t *testing.T, st secret.Store, crt, key secret.Ref) follower {
			c, err := tlsconf.Upstream(tlsconf.ClientSpec{ClientCertificate: &crt, ClientKey: &key}, st)
			if err != nil {
				t.Fatal(err)
			}
			return follower{served: func() string {
				p, err := c.Current().GetClientCertificate(&tls.CertificateRequestInfo{})
				if err != nil || p.Leaf == nil {
					t.Fatalf("GetClientCertificate: %v", err)
				}
				return p.Leaf.Subject.CommonName
			}, close: c.Close}
		},
	}
	for _, certFirst := range []bool{false, true} {
		for name, build := range builds {
			order := "key alone, then certificate with a newer key"
			crtFile, keyFile := "a.crt", "b.key"
			if certFirst {
				order = "certificate alone, then key with a newer certificate"
				crtFile, keyFile = "b.crt", "a.key"
			}
			t.Run(name+"/"+order, func(t *testing.T) {
				h := newHarness(t, nil)
				v1, v2, v3 := newTLSLeaf(t, "v1"), newTLSLeaf(t, "v2"), newTLSLeaf(t, "v3")
				h.write(crtFile, v1.cert)
				h.write(keyFile, v1.key)
				crt, key := fileRef(h.path(crtFile), ""), fileRef(h.path(keyFile), "")
				st := h.resolve(use(crt, secret.KindPEMCertificate), use(key, secret.KindPEMPrivateKey))
				h.r.Activate(st)
				f := build(t, st, crt, key)
				defer f.close()
				alone, other := keyFile, crtFile
				alone2, other2, alone3, other3 := v2.key, v2.cert, v3.key, v3.cert
				if certFirst {
					alone, other = other, alone
					alone2, other2, alone3, other3 = other2, alone2, other3, alone3
				}

				h.write(alone, alone2)
				h.cycle()
				if n, r := h.fileN.value(), h.status.raised(); n != 1 || len(r) != 1 {
					t.Fatalf("first half alone: failures %d, raised %v; want 1 and one source", n, r)
				}
				h.write(other, other2)
				h.write(alone, alone3)
				h.cycle()
				if got, n, r := f.served(), h.fileN.value(), h.status.raised(); got != "v2" || n != 2 || len(r) != 1 {
					t.Fatalf("other half with a newer first half: served %q, failures %d, raised %v; want v2, 2 and one source", got, n, r)
				}
				h.write(other, other3)
				h.cycle()
				if got, n, r := f.served(), h.fileN.value(), h.status.raised(); got != "v3" || n != 2 || len(r) != 0 {
					t.Fatalf("after the pair completed: served %q, failures %d, raised %v; want v3, 2 and none", got, n, r)
				}
				if n := h.r.watchCount(); n != 2 {
					t.Fatalf("watches = %d, want 2", n)
				}
				f.close()
				if n := h.r.watchCount(); n != 0 {
					t.Fatalf("watches after Close = %d, want 0", n)
				}
			})
		}
	}
}
