// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mtls

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// spiffe is the URI SAN of the fixture's workload leaves.
const spiffe = "spiffe://example.org/ns/prod/sa/web"

// deny runs onRequestHeaders on x and checks a rejection with code and
// its registered status, without a challenge and without an identity.
func deny(t *testing.T, f *Filter, x *filtertest.Exchange, code string, status int) {
	t.Helper()
	r := f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if r.Outcome != filter.Respond || r.Response == nil {
		t.Fatalf("Handle = %+v, want Respond %d %s", r, status, code)
	}
	if resp := r.Response; resp.Status != status || resp.Code != code || len(resp.Header) != 0 || resp.Body != nil || resp.Detail != "" {
		t.Fatalf("response = %+v, want %d %s without challenge, body or detail", resp, status, code)
	}
	if x.Ident != nil && x.Ident.Method == filter.MethodMTLS {
		t.Fatalf("identity %+v set on a rejection", x.Ident)
	}
}

// allow runs onRequestHeaders on x, checks Continue and returns the
// identity it set.
func allow(t *testing.T, f *Filter, x *filtertest.Exchange) *filter.Identity {
	t.Helper()
	r := f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if r.Outcome != filter.Continue || x.Ident == nil {
		t.Fatalf("Handle = %+v (code %s), identity %v; want Continue with an identity", r, codeOf(r), x.Ident)
	}
	return x.Ident
}

// codeOf returns the code of a rejection, for messages.
func codeOf(r filter.Result) string {
	if r.Response != nil {
		return r.Response.Code
	}
	return r.Code
}

// TestNoCertificateRequestIs421 checks that an auth.mtls Route reached
// over a connection whose handshake requested no client certificate, a
// cleartext one included, answers 421 RZ-AUTH-008 (Security and identity,
// "Mutual TLS").
func TestNoCertificateRequestIs421(t *testing.T) {
	root := newCA(t, "Root")
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
	f := fx.build(config(false))
	notAsked := conn(client(t, root, "client-a", time.Time{}, time.Time{}))
	notAsked.ClientCertRequested = false
	deny(t, f, request(nil, t0), auth.CodeNoCertRequest, 421)
	deny(t, f, request(notAsked, t0), auth.CodeNoCertRequest, 421)
	if fx.rec.allow != 0 || len(fx.rec.deny) != 2 || fx.rec.deny[0] != auth.CodeNoCertRequest {
		t.Errorf("recorded allow %d, deny %v; want two denials %s", fx.rec.allow, fx.rec.deny, auth.CodeNoCertRequest)
	}
}

// TestNoCertificateIs401 checks that a connection that requested a client
// certificate and got none answers 401 RZ-AUTH-001 without a challenge
// (credential missing; auth.mtls answers without WWW-Authenticate).
func TestNoCertificateIs401(t *testing.T) {
	root := newCA(t, "Root")
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
	f := fx.build(config(false))
	deny(t, f, request(conn(), t0), auth.CodeMissing, 401)
	empty := conn()
	empty.State = nil
	deny(t, f, request(empty, t0), auth.CodeMissing, 401)
	if got := f.Challenge(); got != "" {
		t.Errorf("Challenge() = %q, want none", got)
	}
}

// TestVerifiedChainAuthenticates checks that a leaf whose chain verifies
// against config.caCertificate, through an intermediate the client sent
// or issued by the anchor itself, authenticates: method mtls, the Policy
// name, the RFC 4514 subject, empty claims, no Consumer and the principal
// mtls:<subject>, with one allow counted.
func TestVerifiedChainAuthenticates(t *testing.T) {
	root := newCA(t, "Root")
	inter := intermediate(t, root)
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
	f := fx.build(config(false))
	chains := map[string][]*x509.Certificate{
		"client-a": {client(t, inter, "client-a", time.Time{}, time.Time{}), inter.Cert},
		"client-b": {client(t, root, "client-b", time.Time{}, time.Time{})},
	}
	for cn, chain := range chains {
		id := allow(t, f, request(conn(chain...), t0))
		subject := subjectOf(cn)
		if id.Method != filter.MethodMTLS || id.Policy != policyName || id.CertSubject != subject ||
			id.Consumer != nil || id.Principal != "mtls:"+subject {
			t.Errorf("identity of %s = %+v, want mtls %s, subject %q, unbound, principal mtls:<subject>", cn, id, policyName, subject)
		}
		if b, err := id.Claims.AppendBody(nil); err != nil || string(b) != "{}" {
			t.Errorf("claims of %s = %s, %v; want {}", cn, b, err)
		}
	}
	if fx.rec.allow != 2 || len(fx.rec.deny) != 0 {
		t.Errorf("recorded allow %d, deny %v; want two allows", fx.rec.allow, fx.rec.deny)
	}
}

// TestChainRejections checks that a leaf without the clientAuth extended
// key usage (a server certificate, or one listing no extended key usage
// at all, which crypto/x509 alone would accept) and a chain that does not
// verify against config.caCertificate at the request time (another CA, an
// expired or not yet valid leaf, a missing intermediate) answer 401
// RZ-AUTH-002.
func TestChainRejections(t *testing.T) {
	root := newCA(t, "Root")
	inter := intermediate(t, root)
	stranger := newCA(t, "Stranger")
	server, err := inter.Issue(pkiServer())
	if err != nil {
		t.Fatal(err)
	}
	noEKU := custom(t, inter, &x509.Certificate{Subject: pkix.Name{CommonName: "no-eku"}, KeyUsage: x509.KeyUsageDigitalSignature})
	tests := []struct {
		name  string
		chain []*x509.Certificate
	}{
		{"another CA", []*x509.Certificate{client(t, stranger, "client-a", time.Time{}, time.Time{})}},
		{"expired leaf", []*x509.Certificate{client(t, inter, "old", t0.Add(-48*time.Hour), t0.Add(-time.Second)), inter.Cert}},
		{"leaf not yet valid", []*x509.Certificate{client(t, inter, "early", t0.Add(time.Hour), notAfter), inter.Cert}},
		{"missing intermediate", []*x509.Certificate{client(t, inter, "alone", time.Time{}, time.Time{})}},
		{"server certificate", []*x509.Certificate{server.Cert, inter.Cert}},
		{"no extended key usage", []*x509.Certificate{noEKU, inter.Cert}},
		{"intermediate as leaf", []*x509.Certificate{inter.Cert}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
			f := fx.build(config(false))
			deny(t, f, request(conn(tt.chain...), t0), auth.CodeInvalid, 401)
			if len(fx.rec.deny) != 1 || fx.rec.deny[0] != auth.CodeInvalid {
				t.Errorf("recorded denials %v, want one %s", fx.rec.deny, auth.CodeInvalid)
			}
		})
	}
}

// pkiServer returns the options of a serverAuth-only leaf.
func pkiServer() pki.LeafOptions {
	return pki.LeafOptions{CommonName: "api.shop.example", DNSNames: []string{"api.shop.example"}, Server: true, NotBefore: notBefore, NotAfter: notAfter}
}

// TestBundleCertificatesAreAnchors checks that every certificate of
// config.caCertificate is a trust anchor: a bundle holding only the
// issuing intermediate accepts its leaves sent alone.
func TestBundleCertificatesAreAnchors(t *testing.T) {
	root := newCA(t, "Root")
	inter := intermediate(t, root)
	fx := newFixture(t, map[secret.Ref][]byte{caRef: inter.CertPEM})
	f := fx.build(config(false))
	allow(t, f, request(conn(client(t, inter, "client-a", time.Time{}, time.Time{})), t0))
}

// TestSubjectRules checks config.subjects: no rule admits any verified
// leaf; otherwise a leaf is admitted when its RFC 4514 subject equals a
// subject rule or one of its URI SANs equals a uriSan rule, exactly, and
// a leaf matching no rule is 401 RZ-AUTH-002.
func TestSubjectRules(t *testing.T) {
	root := newCA(t, "Root")
	leaf := client(t, root, "client-a", time.Time{}, time.Time{}, spiffe, "https://workload.example/a")
	tests := []struct {
		name  string
		rules []v1alpha1.CertificateSubject
		admit bool
	}{
		{"no rules", nil, true},
		{"subject", []v1alpha1.CertificateSubject{{Subject: subjectOf("client-a")}}, true},
		{"uriSan", []v1alpha1.CertificateSubject{{URISAN: spiffe}}, true},
		{"second uriSan", []v1alpha1.CertificateSubject{{URISAN: "https://workload.example/a"}}, true},
		{"one of several", []v1alpha1.CertificateSubject{{Subject: "CN=other"}, {URISAN: spiffe}}, true},
		{"no match", []v1alpha1.CertificateSubject{{Subject: subjectOf("client-b")}, {URISAN: "spiffe://example.org/ns/prod/sa/api"}}, false},
		{"subject in other case", []v1alpha1.CertificateSubject{{Subject: "cn=client-a,o=Ruralz tests"}}, false},
		{"subject in other order", []v1alpha1.CertificateSubject{{Subject: "O=Ruralz tests,CN=client-a"}}, false},
		{"uriSan prefix", []v1alpha1.CertificateSubject{{URISAN: "spiffe://example.org/ns/prod"}}, false},
		{"subject given as uriSan", []v1alpha1.CertificateSubject{{URISAN: subjectOf("client-a")}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
			f := fx.build(config(false, tt.rules...))
			x := request(conn(leaf), t0)
			if tt.admit {
				allow(t, f, x)
				return
			}
			deny(t, f, x, auth.CodeInvalid, 401)
		})
	}
}

// TestConsumerBinding checks the optional binding through
// credentials.certificates: a Consumer whose entry equals the leaf's
// subject or one of its URI SANs binds, and the principal is its name; a
// leaf matching no Consumer authenticates unbound with the principal
// mtls:<subject>; a leaf matching two Consumers is 401 RZ-AUTH-002.
func TestConsumerBinding(t *testing.T) {
	root := newCA(t, "Root")
	leaf := client(t, root, "client-a", time.Time{}, time.Time{}, spiffe)
	subject := subjectOf("client-a")
	tests := []struct {
		name      string
		consumers []*v1alpha1.Consumer
		want      string // bound Consumer, "" for unbound, "-" for a rejection
	}{
		{"by subject", []*v1alpha1.Consumer{consumer("acme", v1alpha1.CertificateCredential{Name: "a", Subject: subject})}, "acme"},
		{"by uriSan", []*v1alpha1.Consumer{consumer("acme", v1alpha1.CertificateCredential{Name: "a", URISAN: spiffe})}, "acme"},
		{"unbound", []*v1alpha1.Consumer{consumer("acme", v1alpha1.CertificateCredential{Name: "a", Subject: subjectOf("client-b")})}, ""},
		{"two Consumers", []*v1alpha1.Consumer{
			consumer("acme", v1alpha1.CertificateCredential{Name: "a", Subject: subject}),
			consumer("beta", v1alpha1.CertificateCredential{Name: "b", URISAN: spiffe}),
		}, "-"},
		{"one Consumer by both", []*v1alpha1.Consumer{consumer("acme",
			v1alpha1.CertificateCredential{Name: "a", Subject: subject},
			v1alpha1.CertificateCredential{Name: "b", URISAN: spiffe},
		)}, "acme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
			f := fx.build(config(false), tt.consumers...)
			x := request(conn(leaf), t0)
			switch tt.want {
			case "-":
				deny(t, f, x, auth.CodeInvalid, 401)
			case "":
				if id := allow(t, f, x); id.Consumer != nil || id.Principal != "mtls:"+subject {
					t.Errorf("identity = %+v, want unbound with principal mtls:%s", id, subject)
				}
			default:
				if id := allow(t, f, x); id.Consumer == nil || id.Consumer.Name != tt.want || id.Principal != tt.want {
					t.Errorf("identity = %+v, want bound to %s", id, tt.want)
				}
			}
		})
	}
}

// TestCRLRevocation checks config.crl: a serial a CA's CRL lists revokes
// the certificate that CA issued, a leaf or an intermediate, with 401
// RZ-AUTH-004; CRLs of several CAs combine; an unlisted certificate
// authenticates.
func TestCRLRevocation(t *testing.T) {
	root := newCA(t, "Root")
	inter := intermediate(t, root)
	good := client(t, inter, "good", time.Time{}, time.Time{})
	bad := client(t, inter, "bad", time.Time{}, time.Time{})
	next := t0.Add(24 * time.Hour)
	tests := []struct {
		name    string
		ca, crl []byte
		leaf    *x509.Certificate
		revoked bool
	}{
		{"leaf listed by its issuer", bundle(root, inter), crl(t, inter, next, bad), bad, true},
		{"other leaf", bundle(root, inter), crl(t, inter, next, bad), good, false},
		{"intermediate listed by the root", bundle(root), crl(t, root, next, inter.Cert), good, true},
		{"empty CRL", bundle(root, inter), crl(t, inter, next), bad, false},
		{"CRLs of two CAs", bundle(root, inter), append(crl(t, root, next), crl(t, inter, next, bad)...), bad, true},
		{"leaf serial listed by another CA", bundle(root, inter), crl(t, root, next, bad), bad, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, map[secret.Ref][]byte{caRef: tt.ca, crlRef: tt.crl})
			f := fx.build(config(true))
			x := request(conn(tt.leaf, inter.Cert), t0)
			if tt.revoked {
				deny(t, f, x, auth.CodeRevoked, 401)
				return
			}
			allow(t, f, x)
		})
	}
}

// TestRevokerConsultedOnEveryRequest checks that identity.Revoker is
// consulted for the leaf on every request, a verdict the connection
// cached included: a revocation reaches an open connection at once
// (RZ-AUTH-004).
func TestRevokerConsultedOnEveryRequest(t *testing.T) {
	root := newCA(t, "Root")
	leaf := client(t, root, "client-a", time.Time{}, time.Time{})
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
	f := fx.build(config(false))
	c := conn(leaf)
	allow(t, f, request(c, t0))
	cached, _ := c.Cache.Get(f)
	fx.revoker.revoke(leaf)
	deny(t, f, request(c, t0.Add(time.Second)), auth.CodeRevoked, 401)
	if again, _ := c.Cache.Get(f); again != cached {
		t.Error("the revocation replaced the cached verdict; it must be a per-request check")
	}
}

// TestSecondAuthenticationIs401 checks Authentication rule 2: when an
// earlier auth-class Policy already authenticated the request, a verified
// certificate fails with 401 RZ-AUTH-002 and the first identity stays.
func TestSecondAuthenticationIs401(t *testing.T) {
	root := newCA(t, "Root")
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
	f := fx.build(config(false))
	first := &filter.Identity{Method: filter.MethodJWT, Policy: "jwt-default"}
	x := request(conn(client(t, root, "client-a", time.Time{}, time.Time{})), t0)
	x.Ident = first
	r := f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if r.Response == nil || r.Response.Status != 401 || r.Response.Code != auth.CodeInvalid || x.Ident != first {
		t.Errorf("second authentication = %+v, identity %+v; want 401 %s and the first identity", r, x.Ident, auth.CodeInvalid)
	}
}

// TestOtherPhasesContinue checks that auth.mtls acts only in
// onRequestHeaders, its one registered Phase.
func TestOtherPhasesContinue(t *testing.T) {
	root := newCA(t, "Root")
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
	f := fx.build(config(false))
	for p := phase.OnRequestBody; p < phase.Count; p++ {
		if r := f.Handle(t.Context(), p, request(nil, t0)); r.Outcome != filter.Continue {
			t.Errorf("Handle(%s) = %+v, want Continue", p, r)
		}
	}
	if fx.rec.allow != 0 || len(fx.rec.deny) != 0 {
		t.Errorf("recorded allow %d, deny %v outside onRequestHeaders", fx.rec.allow, fx.rec.deny)
	}
}

// TestVerdictCachedPerConnection checks that the verification is cached
// per connection: a later request on the same connection reuses the
// verdict, another connection presenting the same chain verifies on its
// own, and a connection without a cache verifies every time.
func TestVerdictCachedPerConnection(t *testing.T) {
	root := newCA(t, "Root")
	inter := intermediate(t, root)
	chain := []*x509.Certificate{client(t, inter, "client-a", time.Time{}, time.Time{}), inter.Cert}
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
	f := fx.build(config(false))
	c := conn(chain...)
	allow(t, f, request(c, t0))
	first, ok := c.Cache.Get(f)
	if !ok {
		t.Fatal("no verdict cached under the Filter")
	}
	allow(t, f, request(c, t0.Add(time.Minute)))
	if again, _ := c.Cache.Get(f); again != first {
		t.Error("a second request on the connection verified again")
	}
	other := conn(chain...)
	allow(t, f, request(other, t0))
	if v, _ := other.Cache.Get(f); v == first {
		t.Error("two connections share a verdict")
	}
	bare := conn(chain...)
	bare.Cache = nil
	allow(t, f, request(bare, t0))
}

// TestCacheIsPerSnapshot checks that the per-connection cache is per
// Policy and snapshot: the Filter of the next snapshot, carried over from
// this one, does not reuse this one's verdict, so a Consumer change bound
// by the new snapshot reaches open connections; the old Filter keeps its
// own.
func TestCacheIsPerSnapshot(t *testing.T) {
	root := newCA(t, "Root")
	leaf := client(t, root, "client-a", time.Time{}, time.Time{})
	subject := subjectOf("client-a")
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
	old := fx.build(config(false), consumer("acme", v1alpha1.CertificateCredential{Name: "a", Subject: subject}))
	c := conn(leaf)
	if id := allow(t, old, request(c, t0)); id.Consumer == nil || id.Consumer.Name != "acme" {
		t.Fatalf("first snapshot bound %+v, want acme", id.Consumer)
	}
	env := fx.env(config(false), consumer("beta", v1alpha1.CertificateCredential{Name: "b", Subject: subject}))
	env.Previous = old
	next := fx.buildEnv(env)
	if id := allow(t, next, request(c, t0)); id.Consumer == nil || id.Consumer.Name != "beta" {
		t.Errorf("next snapshot bound %+v on the open connection, want beta", id.Consumer)
	}
	if id := allow(t, old, request(c, t0)); id.Consumer == nil || id.Consumer.Name != "acme" {
		t.Errorf("first snapshot now binds %+v, want acme", id.Consumer)
	}
}

// TestRotationReachesOpenConnections checks that a rotated
// config.caCertificate takes effect without a Rollout, on connections
// already open too: a CA the rotation removed stops matching, and the new
// one matches.
func TestRotationReachesOpenConnections(t *testing.T) {
	rootA, rootB := newCA(t, "Root A"), newCA(t, "Root B")
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(rootA)})
	f := fx.build(config(false))
	a := conn(client(t, rootA, "client-a", time.Time{}, time.Time{}))
	allow(t, f, request(a, t0))
	if errs := fx.store.publish(t, []secret.Ref{caRef}, bundle(rootB)); len(errs) != 0 {
		t.Fatalf("rotation failed: %v", errs)
	}
	deny(t, f, request(a, t0.Add(time.Second)), auth.CodeInvalid, 401)
	allow(t, f, request(conn(client(t, rootB, "client-b", time.Time{}, time.Time{})), t0))
}

// TestExpiryDuringConnection checks that an accepted verdict holds only
// while the chain is valid: a certificate that expires during a
// long-lived connection is refused from then on (RZ-AUTH-002).
func TestExpiryDuringConnection(t *testing.T) {
	root := newCA(t, "Root")
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
	f := fx.build(config(false))
	c := conn(client(t, root, "short", notBefore, t0.Add(time.Hour)))
	allow(t, f, request(c, t0))
	allow(t, f, request(c, t0.Add(time.Hour)))
	deny(t, f, request(c, t0.Add(time.Hour+time.Second)), auth.CodeInvalid, 401)
}

// TestNotYetValidVerifiedAgain checks that a chain refused because a
// certificate was not valid yet is verified again once it is, on the same
// connection, while an earlier request reuses the refusal.
func TestNotYetValidVerifiedAgain(t *testing.T) {
	root := newCA(t, "Root")
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
	f := fx.build(config(false))
	c := conn(client(t, root, "early", t0.Add(time.Hour), notAfter))
	deny(t, f, request(c, t0), auth.CodeInvalid, 401)
	refusal, _ := c.Cache.Get(f)
	deny(t, f, request(c, t0.Add(30*time.Minute)), auth.CodeInvalid, 401)
	if again, _ := c.Cache.Get(f); again != refusal {
		t.Error("the refusal was verified again before the certificate became valid")
	}
	allow(t, f, request(c, t0.Add(time.Hour)))
}
