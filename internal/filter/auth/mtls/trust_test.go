// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mtls

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// garbled is a PEM block of type typ whose content parses as nothing.
func garbled(typ string) []byte {
	return []byte("-----BEGIN " + typ + "-----\nAAAA\n-----END " + typ + "-----\n")
}

// testKey is a secretRef-held API key the credential index watches.
const testKey = "ZXhhbXBsZS1rZXktYWFhYWFhYWFhYWFhYWFhYWFhYWE"

// keyConsumer is a Consumer with a secretRef-held API key, so the
// credential index holds a watch while it is acquired.
func keyConsumer() *v1alpha1.Consumer {
	return &v1alpha1.Consumer{
		Metadata: v1alpha1.ObjectMeta{Name: "keyed"},
		Spec: v1alpha1.ConsumerSpec{Credentials: v1alpha1.Credentials{APIKeys: []v1alpha1.APIKey{
			{Name: "k", SecretRef: &v1alpha1.SecretRef{Provider: keyRef.Provider, Name: keyRef.Name}},
		}}},
	}
}

// TestBuildRejectsInvalidConfig checks what Build refuses, each without a
// watch left behind: a config that is not an auth.mtls config or a
// config.subjects rule with both or neither of subject and uriSan
// (RZ-CFG-005); no secret table, an unresolved reference, a CA bundle
// with no certificate or one that does not parse, a CRL value with no
// CRL or one that does not parse, and a CRL no certificate of the bundle
// signed (RZ-CFG-026); and a credential index that cannot be built
// (its own code).
func TestBuildRejectsInvalidConfig(t *testing.T) {
	root := newCA(t, "Root")
	stranger := newCA(t, "Stranger")
	next := t0.Add(24 * time.Hour)
	tests := []struct {
		name      string
		cfg       any
		values    map[secret.Ref][]byte
		consumers []*v1alpha1.Consumer
		noSecrets bool
		code      string
	}{
		{name: "not an auth.mtls config", cfg: &v1alpha1.CORSConfig{}, code: CodeSchema},
		{name: "nil config", cfg: (*v1alpha1.AuthMTLSConfig)(nil), code: CodeSchema},
		{name: "rule with both", cfg: config(false, v1alpha1.CertificateSubject{Subject: "CN=a", URISAN: spiffe}), code: CodeSchema},
		{name: "rule with neither", cfg: config(false, v1alpha1.CertificateSubject{}), code: CodeSchema},
		{name: "no secret table", cfg: config(false), noSecrets: true, code: CodeResolution},
		{name: "CA unresolved", cfg: config(false), values: map[secret.Ref][]byte{}, code: CodeResolution},
		{name: "CA without certificate", cfg: config(false), values: map[secret.Ref][]byte{caRef: []byte("not PEM")}, code: CodeResolution},
		{name: "CA with a bad certificate", cfg: config(false), values: map[secret.Ref][]byte{caRef: append(bundle(root), garbled("CERTIFICATE")...)}, code: CodeResolution},
		{name: "CRL unresolved", cfg: config(true), values: map[secret.Ref][]byte{caRef: bundle(root)}, code: CodeResolution},
		{name: "CRL without CRL", cfg: config(true), values: map[secret.Ref][]byte{caRef: bundle(root), crlRef: bundle(root)}, code: CodeResolution},
		{name: "CRL that does not parse", cfg: config(true), values: map[secret.Ref][]byte{caRef: bundle(root), crlRef: append(crl(t, root, next), garbled("X509 CRL")...)}, code: CodeResolution},
		{name: "CRL of another CA", cfg: config(true), values: map[secret.Ref][]byte{caRef: bundle(root), crlRef: crl(t, stranger, next)}, code: CodeResolution},
		{
			name: "credential index", cfg: config(true), values: map[secret.Ref][]byte{caRef: bundle(root), crlRef: crl(t, root, next)},
			consumers: []*v1alpha1.Consumer{keyConsumer()}, code: "RZ-CFG-026",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := tt.values
			if values == nil {
				values = map[secret.Ref][]byte{caRef: bundle(root)}
			}
			fx := newFixture(t, values)
			env := fx.env(tt.cfg, tt.consumers...)
			if tt.noSecrets {
				env.Secrets = nil
			}
			f, err := NewFactory(nil).Build(t.Context(), env)
			if f != nil || err == nil {
				t.Fatalf("Build = %v, %v; want an error", f, err)
			}
			if code, ok := errcode.CodeOf(err); !ok || code != tt.code {
				t.Errorf("Build error %v has code %q, want %s", err, code, tt.code)
			}
			if n := fx.store.live(caRef) + fx.store.live(crlRef) + fx.store.live(keyRef); n != 0 {
				t.Errorf("%d watches left after a failed Build", n)
			}
			if fx.status.raised() != 0 {
				t.Error("crl_stale raised by a failed Build")
			}
		})
	}
}

// TestBuildStopsOnEndedContext checks that a canceled snapshot compile
// stops Build with the context's error, which carries no RZ code.
func TestBuildStopsOnEndedContext(t *testing.T) {
	root := newCA(t, "Root")
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root)})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := NewFactory(nil).Build(ctx, fx.env(config(false)))
	if _, coded := errcode.CodeOf(err); !errors.Is(err, context.Canceled) || coded {
		t.Errorf("Build = %v, want context.Canceled without a code", err)
	}
	if fx.store.live(caRef) != 0 {
		t.Error("a canceled Build left a watch")
	}
}

// TestCloseReleasesWatchesAndIndex checks that Close stops the Filter's
// secret watches and releases its credential index, whose watches stop
// with it, and that a second Close does nothing.
func TestCloseReleasesWatchesAndIndex(t *testing.T) {
	root := newCA(t, "Root")
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root), crlRef: crl(t, root, t0.Add(time.Hour)), keyRef: []byte(testKey)})
	built, err := NewFactory(nil).Build(t.Context(), fx.env(config(true), keyConsumer()))
	if err != nil {
		t.Fatal(err)
	}
	f := built.(*Filter)
	if fx.store.live(caRef) != 1 || fx.store.live(crlRef) != 1 || fx.store.live(keyRef) != 1 {
		t.Fatalf("live watches ca %d, crl %d, key %d; want one each", fx.store.live(caRef), fx.store.live(crlRef), fx.store.live(keyRef))
	}
	if f.CredentialIndex() == nil {
		t.Fatal("no credential index")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if n := fx.store.live(caRef) + fx.store.live(crlRef) + fx.store.live(keyRef); n != 0 {
		t.Errorf("%d watches left after Close", n)
	}
}

// TestCarryOverSharesTrust checks that the Filter of the next snapshot
// carries over the trust holder of the previous one when it follows the
// same references (no new watches), that the watches stop only with the
// last Filter sharing them, and that other references get a holder of
// their own.
func TestCarryOverSharesTrust(t *testing.T) {
	root := newCA(t, "Root")
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root), crlRef: crl(t, root, t0.Add(time.Hour))})
	factory := NewFactory(nil)
	build := func(cfg *v1alpha1.AuthMTLSConfig, prev filter.Filter) *Filter {
		t.Helper()
		env := fx.env(cfg)
		env.Previous = prev
		f, err := factory.Build(t.Context(), env)
		if err != nil {
			t.Fatal(err)
		}
		return f.(*Filter)
	}
	old := build(config(true), nil)
	next := build(config(true), old)
	if next.trust != old.trust || fx.store.live(caRef) != 1 {
		t.Fatalf("carried over: shared %v, ca watches %d; want shared and one watch", next.trust == old.trust, fx.store.live(caRef))
	}
	_ = old.Close()
	if fx.store.live(caRef) != 1 {
		t.Fatal("closing the previous snapshot's Filter stopped the shared watches")
	}
	allow(t, next, request(conn(client(t, root, "client-a", time.Time{}, time.Time{})), t0))
	without := build(config(false), next)
	if without.trust == next.trust || fx.store.live(caRef) != 2 {
		t.Errorf("a config without crl shares the holder (%v) or reuses its watch (ca watches %d)", without.trust == next.trust, fx.store.live(caRef))
	}
	_ = next.Close()
	_ = without.Close()
	if n := fx.store.live(caRef) + fx.store.live(crlRef); n != 0 {
		t.Errorf("%d watches left after the last Close", n)
	}
	// A holder closed with its last Filter is not carried over.
	again := build(config(true), next)
	if again.trust == next.trust {
		t.Error("a closed holder was carried over")
	}
	_ = again.Close()
}

// TestRotationFailureKeepsLastValue checks that a rotated value the
// Filter refuses (a bundle without a certificate, a CRL of another CA)
// fails the watch callback, which the resolver counts, and keeps the last
// trust state in force.
func TestRotationFailureKeepsLastValue(t *testing.T) {
	root := newCA(t, "Root")
	stranger := newCA(t, "Stranger")
	bad := client(t, root, "bad", time.Time{}, time.Time{})
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root), crlRef: crl(t, root, t0.Add(48*time.Hour), bad)})
	f := fx.build(config(true))
	if errs := fx.store.publish(t, []secret.Ref{caRef}, []byte("not PEM")); len(errs) != 1 || !errors.Is(errs[0], errNoCA) {
		t.Fatalf("CA rotation errors = %v, want errNoCA", errs)
	}
	if errs := fx.store.publish(t, []secret.Ref{crlRef}, crl(t, stranger, t0.Add(48*time.Hour))); len(errs) != 1 {
		t.Fatalf("CRL rotation errors = %v, want one", errs)
	}
	if fx.store.failing() != 2 {
		t.Errorf("%d failing watches, want 2", fx.store.failing())
	}
	deny(t, f, request(conn(bad), t0), auth.CodeRevoked, 401)
	allow(t, f, request(conn(client(t, root, "good", time.Time{}, time.Time{})), t0))
}

// TestPairedRotation checks that config.caCertificate and config.crl may
// change in either order or in one poll: a CRL the current bundle did not
// sign fails until its CA arrives, a bundle that drops the CA of the
// current CRL fails until its CRL arrives, and once the pair is valid it
// is in force and no watch is left failing.
func TestPairedRotation(t *testing.T) {
	rootA, rootB := newCA(t, "Root A"), newCA(t, "Root B")
	revokedB := client(t, rootB, "revoked-b", time.Time{}, time.Time{})
	goodB := client(t, rootB, "good-b", time.Time{}, time.Time{})
	newCRL := crl(t, rootB, t0.Add(48*time.Hour), revokedB)
	steps := map[string][][]secret.Ref{
		"CRL first":                     {{crlRef}, {caRef}},
		"CA first":                      {{caRef}, {crlRef}},
		"one poll":                      {{caRef, crlRef}},
		"one poll, CRL delivered first": {{crlRef, caRef}},
	}
	for name, polls := range steps {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(rootA), crlRef: crl(t, rootA, t0.Add(48*time.Hour))})
			f := fx.build(config(true))
			for _, refs := range polls {
				values := make([][]byte, len(refs))
				for i, r := range refs {
					values[i] = newCRL
					if r == caRef {
						values[i] = bundle(rootB)
					}
				}
				fx.store.publish(t, refs, values...)
			}
			if n := fx.store.failing(); n != 0 {
				t.Errorf("%d watches still failing after the pair became valid", n)
			}
			if fx.store.live(caRef) != 1 || fx.store.live(crlRef) != 1 {
				t.Errorf("live watches ca %d, crl %d; want one each", fx.store.live(caRef), fx.store.live(crlRef))
			}
			deny(t, f, request(conn(revokedB), t0), auth.CodeRevoked, 401)
			allow(t, f, request(conn(goodB), t0))
			deny(t, f, request(conn(client(t, rootA, "a", time.Time{}, time.Time{})), t0), auth.CodeInvalid, 401)
		})
	}
}

// TestPairedRotationKeepsFailureWhenValueMoved checks the re-registration
// race: when the other reference's value moved after the pair was formed,
// its failed watch is kept, so it still examines the newer value.
func TestPairedRotationKeepsFailureWhenValueMoved(t *testing.T) {
	rootA, rootB := newCA(t, "Root A"), newCA(t, "Root B")
	newCRL := crl(t, rootB, t0.Add(48*time.Hour))
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(rootA), crlRef: crl(t, rootA, t0.Add(48*time.Hour))})
	fx.build(config(true))
	if errs := fx.store.publish(t, []secret.Ref{crlRef}, newCRL); len(errs) != 1 {
		t.Fatalf("CRL before its CA: errors %v, want one", errs)
	}
	fx.store.onWatch = func(r secret.Ref) {
		if r == crlRef {
			fx.store.move(crlRef, []byte("a newer value, not delivered yet"))
		}
	}
	if errs := fx.store.publish(t, []secret.Ref{caRef}, bundle(rootB)); len(errs) != 0 {
		t.Fatalf("CA rotation errors = %v, want none", errs)
	}
	if fx.store.failing() != 1 || fx.store.live(crlRef) != 1 {
		t.Errorf("failing %d, live CRL watches %d; want the old failed watch kept alone", fx.store.failing(), fx.store.live(crlRef))
	}
}

// TestCRLStale checks crl_stale: a CRL past its nextUpdate stays enforced
// and the Node reports the degraded state, raised at Build when already
// past and by a timer when the time comes, cleared by a rotation to fresh
// CRLs and when the Policy goes away.
func TestCRLStale(t *testing.T) {
	root := newCA(t, "Root")
	revoked := client(t, root, "revoked", time.Time{}, time.Time{})
	fx := newFixture(t, map[secret.Ref][]byte{caRef: bundle(root), crlRef: crl(t, root, t0.Add(time.Hour), revoked)})
	f := fx.build(config(true))
	if fx.status.raised() != 0 {
		t.Fatal("crl_stale raised before nextUpdate")
	}
	fx.clock.Advance(time.Hour - time.Second)
	if fx.status.raised() != 0 {
		t.Fatal("crl_stale raised before nextUpdate")
	}
	fx.clock.Advance(time.Second)
	if fx.status.raised() != 1 {
		t.Fatal("crl_stale not raised at nextUpdate")
	}
	deny(t, f, request(conn(revoked), t0.Add(2*time.Hour)), auth.CodeRevoked, 401)
	fresh := crl(t, root, t0.Add(48*time.Hour), revoked)
	if errs := fx.store.publish(t, []secret.Ref{crlRef}, fresh); len(errs) != 0 {
		t.Fatalf("rotation errors: %v", errs)
	}
	if fx.status.raised() != 0 {
		t.Fatal("crl_stale kept after fresh CRLs")
	}
	if errs := fx.store.publish(t, []secret.Ref{crlRef}, crl(t, root, t0.Add(30*time.Minute), revoked)); len(errs) != 0 {
		t.Fatalf("rotation errors: %v", errs)
	}
	if fx.status.raised() != 1 {
		t.Fatal("crl_stale not raised for a CRL already past nextUpdate")
	}
	_ = f.Close()
	if fx.status.raised() != 0 {
		t.Error("crl_stale kept after the Policy's last Filter closed")
	}
	fx.clock.Advance(72 * time.Hour)
	if fx.status.raised() != 0 {
		t.Error("crl_stale raised after Close")
	}
}

// TestCRLStaleIsPerCA checks the nextUpdate that counts: per CA the
// latest of its CRLs, and the earliest over the CAs.
func TestCRLStaleIsPerCA(t *testing.T) {
	root := newCA(t, "Root")
	inter := intermediate(t, root)
	crls := append(append(crl(t, root, t0.Add(time.Hour)), crl(t, root, t0.Add(10*time.Hour))...), crl(t, inter, t0.Add(5*time.Hour))...)
	ts, err := newTrustState(bundle(root, inter), crls, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(5 * time.Hour); !ts.staleAt.Equal(want) {
		t.Errorf("staleAt = %v, want %v", ts.staleAt, want)
	}
	noCRL, err := newTrustState(bundle(root), nil, false)
	if err != nil || !noCRL.staleAt.IsZero() || noCRL.crls != nil {
		t.Errorf("without config.crl: %+v, %v; want no CRL and no staleAt", noCRL, err)
	}
}

// TestBundleSkipsOtherBlocks checks that PEM blocks other than
// CERTIFICATE in config.caCertificate, and other than X509 CRL in
// config.crl, are skipped.
func TestBundleSkipsOtherBlocks(t *testing.T) {
	root := newCA(t, "Root")
	ca := append(append([]byte("comment\n"), root.KeyPEM...), bundle(root)...)
	list := append(bundle(root), crl(t, root, t0.Add(time.Hour))...)
	if _, err := newTrustState(ca, list, true); err != nil {
		t.Errorf("newTrustState = %v, want other blocks skipped", err)
	}
}
