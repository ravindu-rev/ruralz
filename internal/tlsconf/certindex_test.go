// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tlsconf

import (
	"bytes"
	"crypto/tls"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

// Tests for spec 06 requirements 75 (certificate choice by SNI, accepted
// key types) and 76 (rotation by watch: a valid pair reaches the next
// handshake, an invalid one keeps the last value and counts a failure).

// indexFixture builds a CertIndex over named leaves put into a fake store.
type indexFixture struct {
	ca     *testCA
	store  *fakeStore
	leaves map[string]leaf
}

func newIndexFixture(t *testing.T) *indexFixture {
	t.Helper()
	return &indexFixture{ca: newCA(t, "listener-ca"), store: newFakeStore(), leaves: map[string]leaf{}}
}

func (f *indexFixture) add(t *testing.T, name string, o leafOpts) CertSpec {
	t.Helper()
	if o.cn == "" {
		o.cn = name
	}
	l := f.ca.issue(t, o)
	f.leaves[name] = l
	f.store.put(ref(name+".crt"), l.certPEM)
	f.store.put(ref(name+".key"), l.keyPEM)
	return CertSpec{Name: name, Certificate: ref(name + ".crt"), PrivateKey: ref(name + ".key")}
}

func served(t *testing.T, ix *CertIndex, sni string) string {
	t.Helper()
	c, err := ix.GetCertificate(&tls.ClientHelloInfo{ServerName: sni})
	if err != nil {
		t.Fatalf("GetCertificate(%q): %v", sni, err)
	}
	return c.Leaf.Subject.CommonName
}

func TestCertIndexSNIReq75(t *testing.T) {
	f := newIndexFixture(t)
	// Authored out of order: the default is the first by name.
	specs := []CertSpec{
		f.add(t, "d-wild-api", leafOpts{dns: []string{"*.api.example.com", "api.example.com"}}),
		f.add(t, "b-exact", leafOpts{dns: []string{"api.example.com", "WWW.Example.com"}}),
		f.add(t, "c-wild", leafOpts{dns: []string{"*.example.com", "*.*.bad.example", "f*.odd.example", "*."}}),
		f.add(t, "a-default", leafOpts{dns: []string{"default.example.com"}}),
	}
	ix, err := BuildCertIndex(specs, f.store)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tests := []struct{ sni, want string }{
		{"api.example.com", "b-exact"}, // exact wins; first name wins on a shared SAN
		{"API.EXAMPLE.COM.", "b-exact"},
		{"www.example.com", "b-exact"}, // SANs are lowercased
		{"default.example.com", "a-default"},
		{"foo.example.com", "c-wild"},
		{"v1.api.example.com", "d-wild-api"},
		{"x.foo.example.com", "a-default"}, // a wildcard covers one label only
		{"example.com", "a-default"},       // a wildcard never matches its suffix
		{"", "a-default"},                  // no SNI
		{"unknown.test", "a-default"},
		{".example.com", "a-default"},
		{"x.bad.example", "a-default"},
		{"fx.odd.example", "a-default"},
	}
	for _, tt := range tests {
		if got := served(t, ix, tt.sni); got != tt.want {
			t.Errorf("SNI %q served %q, want %q", tt.sni, got, tt.want)
		}
	}
}

func TestCertIndexKeyTypesReq75(t *testing.T) {
	tests := []struct {
		key string
		ok  bool
	}{
		{"p256", true},
		{"p384", true},
		{"rsa2048", true},
		{"ed25519", true},
		{"rsa1024", false},
		{"p224", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			f := newIndexFixture(t)
			ix, err := BuildCertIndex([]CertSpec{f.add(t, "c", leafOpts{dns: []string{"a.test"}, key: tt.key})}, f.store)
			if tt.ok {
				if err != nil {
					t.Fatal(err)
				}
				ix.Close()
				return
			}
			if !errors.Is(err, ErrKeyType) {
				t.Fatalf("err = %v, want ErrKeyType", err)
			}
			if code, _ := errcode.CodeOf(err); code != "RZ-CFG-026" {
				t.Fatalf("code = %q, want RZ-CFG-026", code)
			}
		})
	}
}

func TestBuildCertIndexErrorsReq75(t *testing.T) {
	f := newIndexFixture(t)
	good := f.add(t, "good", leafOpts{dns: []string{"a.test"}})
	other := f.add(t, "other", leafOpts{dns: []string{"b.test"}})
	f.store.put(ref("garbage"), []byte("not a certificate"))
	f.store.put(ref("empty"), []byte{})
	keyBody := string(bytes.Split(f.leaves["good"].keyPEM, []byte("\n"))[1])

	tests := []struct {
		name  string
		specs []CertSpec
		store secret.Store
		code  string
		is    error
	}{
		{"none", nil, f.store, "RZ-CFG-005", nil},
		{"duplicate", []CertSpec{good, good}, f.store, "RZ-CFG-005", nil},
		{"nil store", []CertSpec{good}, nil, "RZ-CFG-026", nil},
		{"unresolved certificate", []CertSpec{{Name: "x", Certificate: ref("missing"), PrivateKey: good.PrivateKey}}, f.store, "RZ-CFG-026", nil},
		{"unresolved key", []CertSpec{{Name: "x", Certificate: good.Certificate, PrivateKey: ref("missing")}}, f.store, "RZ-CFG-026", nil},
		{"garbage certificate", []CertSpec{{Name: "x", Certificate: ref("garbage"), PrivateKey: good.PrivateKey}}, f.store, "RZ-CFG-026", ErrNoCertificate},
		{"empty certificate", []CertSpec{{Name: "x", Certificate: ref("empty"), PrivateKey: good.PrivateKey}}, f.store, "RZ-CFG-026", ErrNoCertificate},
		{"garbage key", []CertSpec{{Name: "x", Certificate: good.Certificate, PrivateKey: ref("garbage")}}, f.store, "RZ-CFG-026", ErrKeyPair},
		{"mismatched key", []CertSpec{{Name: "x", Certificate: good.Certificate, PrivateKey: other.PrivateKey}}, f.store, "RZ-CFG-026", ErrKeyPair},
		{"key as certificate", []CertSpec{{Name: "x", Certificate: good.PrivateKey, PrivateKey: good.PrivateKey}}, f.store, "RZ-CFG-026", ErrNoCertificate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := f.store.watching()
			ix, err := BuildCertIndex(tt.specs, tt.store)
			if err == nil {
				ix.Close()
				t.Fatal("BuildCertIndex succeeded")
			}
			if code, _ := errcode.CodeOf(err); code != tt.code {
				t.Fatalf("code %q, want %q (%v)", code, tt.code, err)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Fatalf("err %v, want %v", err, tt.is)
			}
			if strings.Contains(err.Error(), keyBody) || strings.Contains(err.Error(), "BEGIN") {
				t.Fatal("the error repeats secret content")
			}
			if f.store.watching() != before {
				t.Fatal("a failed build left watches registered")
			}
		})
	}
	// Every failing certificate is reported, and both unresolved
	// references of one entry.
	_, err := BuildCertIndex([]CertSpec{
		{Name: "x", Certificate: ref("garbage"), PrivateKey: good.PrivateKey},
		{Name: "y", Certificate: ref("missing"), PrivateKey: good.PrivateKey},
		{Name: "z", Certificate: ref("missing-z.crt"), PrivateKey: ref("missing-z.key")},
	}, f.store)
	if err == nil {
		t.Fatal("BuildCertIndex succeeded")
	}
	for _, want := range []string{`"x"`, `"y"`, "certificate " + ref("missing-z.crt").String(), "privateKey " + ref("missing-z.key").String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("all failures should be reported, %q missing: %v", want, err)
		}
	}
}

func TestCertIndexRotationReq76(t *testing.T) {
	f := newIndexFixture(t)
	spec := f.add(t, "main", leafOpts{cn: "v1", dns: []string{"api.example.com"}})
	ix, err := BuildCertIndex([]CertSpec{spec}, f.store)
	if err != nil {
		t.Fatal(err)
	}
	if got := served(t, ix, "api.example.com"); got != "v1" {
		t.Fatalf("served %q", got)
	}

	// Certificate and key renewed in two separate polls: the new
	// certificate does not match the current key, a failure under
	// requirement 76 (counted, v1 kept); the key's poll completes the pair.
	// A renewal in one poll counts nothing (TestCertIndexRotationOnePollReq76).
	v2 := f.ca.issue(t, leafOpts{cn: "v2", dns: []string{"new.example.com"}})
	if n := f.store.rotate(spec.Certificate, v2.certPEM); n != 1 {
		t.Fatalf("mismatched certificate: %d failures, want 1", n)
	}
	if got := served(t, ix, "api.example.com"); got != "v1" {
		t.Fatalf("after a failed rotation served %q, want v1", got)
	}
	if n := f.store.rotate(spec.PrivateKey, v2.keyPEM); n != 0 {
		t.Fatalf("matching key: %d failures", n)
	}
	if got := served(t, ix, "new.example.com"); got != "v2" {
		t.Fatalf("after rotation served %q, want v2", got)
	}
	if got := served(t, ix, "api.example.com"); got != "v2" {
		t.Fatalf("the old SAN should fall back to the default (v2), got %q", got)
	}

	// Garbage keeps v2 and counts.
	if n := f.store.rotate(spec.Certificate, []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")); n != 1 {
		t.Fatalf("garbage: %d failures", n)
	}
	if got := served(t, ix, "new.example.com"); got != "v2" {
		t.Fatalf("after garbage served %q", got)
	}
	// A refused key type keeps v2 and counts.
	weak := f.ca.issue(t, leafOpts{cn: "weak", dns: []string{"new.example.com"}, key: "p224"})
	f.store.rotate(spec.Certificate, weak.certPEM)
	if n := f.store.rotate(spec.PrivateKey, weak.keyPEM); n != 1 {
		t.Fatalf("weak key: %d failures", n)
	}
	if got := served(t, ix, "new.example.com"); got != "v2" {
		t.Fatalf("after a weak key served %q", got)
	}
	if f.store.failures.Load() != 4 {
		t.Fatalf("rotation failures = %d, want 4", f.store.failures.Load())
	}

	// After Close no watch remains and the last value keeps serving.
	ix.Close()
	ix.Close()
	if f.store.watching() != 0 {
		t.Fatalf("%d watches after Close", f.store.watching())
	}
	v3 := f.ca.issue(t, leafOpts{cn: "v3", dns: []string{"new.example.com"}})
	f.store.rotate(spec.Certificate, v3.certPEM)
	f.store.rotate(spec.PrivateKey, v3.keyPEM)
	if got := served(t, ix, "new.example.com"); got != "v2" {
		t.Fatalf("a closed index followed a rotation: %q", got)
	}
	// A late callback after Close is ignored.
	if err := ix.rotate(ix.entries[0], true, secret.NewValue(v3.certPEM)); err != nil {
		t.Fatal(err)
	}
}

// TestCertIndexRotationOnePollReq76 renews certificates in one resolver
// poll: the Store holds every new value before any watcher runs, so the
// first watcher pairs the new certificate with the new key and no rotation
// failure is counted, whichever watcher runs first and also for one PEM
// file used as both certificate and privateKey (requirement 76 counts a
// failure only for a value that does not parse or whose key does not
// match).
func TestCertIndexRotationOnePollReq76(t *testing.T) {
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
			f := newIndexFixture(t)
			spec := f.add(t, "main", leafOpts{cn: "v1", dns: []string{"a.test"}})
			if tt.combined {
				f.store.put(ref("main.pem"), f.leaves["main"].combinedPEM())
				spec.Certificate, spec.PrivateKey = ref("main.pem"), ref("main.pem")
			}
			ix, err := BuildCertIndex([]CertSpec{spec}, f.store)
			if err != nil {
				t.Fatal(err)
			}
			defer ix.Close()
			for _, cn := range []string{"v2", "v3"} {
				next := f.ca.issue(t, leafOpts{cn: cn, dns: []string{"a.test"}})
				var n int
				switch {
				case tt.combined:
					n = f.store.rotate(spec.Certificate, next.combinedPEM())
				case tt.keyFirst:
					n = f.store.rotateTogether(update{spec.PrivateKey, next.keyPEM}, update{spec.Certificate, next.certPEM})
				default:
					n = f.store.rotateTogether(update{spec.Certificate, next.certPEM}, update{spec.PrivateKey, next.keyPEM})
				}
				if n != 0 {
					t.Fatalf("renewal %s in one poll: %d rotation failures, want 0", cn, n)
				}
				if got := served(t, ix, "a.test"); got != cn {
					t.Fatalf("served %q, want %q", got, cn)
				}
			}
		})
	}
}

// TestCertIndexSeparatePollsClearReasonReq76 renews a certificate in one
// poll and its key in the next: the certificate's callback fails once
// (counted, the resolver raises secret_rotation_failed for its watch), and
// the key's poll, which completes the pair, registers the certificate's
// watch again, so no degraded reason is left (secret.Store.Watch). A key
// that never arrives keeps the reason raised, and so does a valid pair of
// another entry.
func TestCertIndexSeparatePollsClearReasonReq76(t *testing.T) {
	f := newIndexFixture(t)
	spec := f.add(t, "main", leafOpts{cn: "v1", dns: []string{"a.test"}})
	other := f.add(t, "other", leafOpts{cn: "o1", dns: []string{"b.test"}})
	ix, err := BuildCertIndex([]CertSpec{spec, other}, f.store)
	if err != nil {
		t.Fatal(err)
	}
	v2 := f.ca.issue(t, leafOpts{cn: "v2", dns: []string{"a.test"}})
	if n := f.store.rotate(spec.Certificate, v2.certPEM); n != 1 || f.store.raised() != 1 {
		t.Fatalf("certificate in its own poll: %d failures, %d raised; want 1 and 1", n, f.store.raised())
	}
	if n := f.store.rotate(spec.PrivateKey, v2.keyPEM); n != 0 {
		t.Fatalf("key in the next poll: %d failures", n)
	}
	if got := served(t, ix, "a.test"); got != "v2" {
		t.Fatalf("served %q, want v2", got)
	}
	if r := f.store.raised(); r != 0 {
		t.Fatalf("after the pair completed: %d reasons raised, want 0", r)
	}
	if n := f.store.failures.Load(); n != 1 || f.store.watching() != 4 {
		t.Fatalf("failures %d (want 1 kept), watches %d (want 4)", n, f.store.watching())
	}
	// The registered-again watch follows the next renewal.
	v3 := f.ca.issue(t, leafOpts{cn: "v3", dns: []string{"a.test"}})
	if n := f.store.rotateTogether(update{spec.Certificate, v3.certPEM}, update{spec.PrivateKey, v3.keyPEM}); n != 0 {
		t.Fatalf("renewal in one poll: %d failures", n)
	}
	if got := served(t, ix, "a.test"); got != "v3" {
		t.Fatalf("served %q, want v3", got)
	}

	// The key never arrives: the reason stays raised, also when another
	// entry rotates and when a key that does not match arrives.
	v4 := f.ca.issue(t, leafOpts{cn: "v4", dns: []string{"a.test"}})
	f.store.rotate(spec.Certificate, v4.certPEM)
	o2 := f.ca.issue(t, leafOpts{cn: "o2", dns: []string{"b.test"}})
	if n := f.store.rotateTogether(update{other.Certificate, o2.certPEM}, update{other.PrivateKey, o2.keyPEM}); n != 0 {
		t.Fatalf("other entry: %d failures", n)
	}
	if r := f.store.raised(); r != 1 {
		t.Fatalf("certificate without its key: %d reasons raised, want 1", r)
	}
	stray := f.ca.issue(t, leafOpts{cn: "stray", dns: []string{"a.test"}})
	if n := f.store.rotate(spec.PrivateKey, stray.keyPEM); n != 1 || f.store.raised() != 2 {
		t.Fatalf("mismatched key: %d failures, %d raised; want 1 and 2", n, f.store.raised())
	}
	if got := served(t, ix, "a.test"); got != "v3" {
		t.Fatalf("served %q, want v3", got)
	}

	// Close stops the watches registered again too.
	ix.Close()
	if f.store.watching() != 0 || f.store.raised() != 0 {
		t.Fatalf("after Close: %d watches, %d raised", f.store.watching(), f.store.raised())
	}
}

// TestCertIndexNewerSiblingValueReq76 renews one half alone (its callback
// fails), then, in one poll, the other half's matching value together with
// a newer value of the first half that matches nothing. The pair is formed
// with the first half's last delivered value, so the first half's watch is
// not registered again: a new registration would start at the newer
// value's version and skip it. Its own callback examines that value, which
// counts a second failure and keeps the reason raised. The next value of
// the other half, which pairs with the first half's current value,
// registers it again and clears the reason. Both reference orders are
// covered (the resolver delivers in reference order).
func TestCertIndexNewerSiblingValueReq76(t *testing.T) {
	for _, certFirst := range []bool{false, true} {
		name := "key alone, then certificate with a newer key"
		if certFirst {
			name = "certificate alone, then key with a newer certificate"
		}
		t.Run(name, func(t *testing.T) {
			f := newIndexFixture(t)
			spec := f.add(t, "main", leafOpts{cn: "v1", dns: []string{"a.test"}})
			ix, err := BuildCertIndex([]CertSpec{spec}, f.store)
			if err != nil {
				t.Fatal(err)
			}
			defer ix.Close()
			v2 := f.ca.issue(t, leafOpts{cn: "v2", dns: []string{"a.test"}})
			v3 := f.ca.issue(t, leafOpts{cn: "v3", dns: []string{"a.test"}})
			alone, other := spec.PrivateKey, spec.Certificate
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
			if got := served(t, ix, "a.test"); got != "v2" {
				t.Fatalf("served %q, want v2", got)
			}
			if n, r := f.store.failures.Load(), f.store.raised(); n != 2 || r != 1 {
				t.Fatalf("after the newer value: %d failures, %d raised; want 2 and 1", n, r)
			}
			if n := f.store.rotate(other, other3); n != 0 {
				t.Fatalf("other half matching the current value: %d failures", n)
			}
			if got := served(t, ix, "a.test"); got != "v3" {
				t.Fatalf("served %q, want v3", got)
			}
			if n, r, w := f.store.failures.Load(), f.store.raised(), f.store.watching(); n != 2 || r != 0 || w != 2 {
				t.Fatalf("after the pair completed: %d failures, %d raised, %d watches; want 2, 0 and 2", n, r, w)
			}
		})
	}
}

// TestCertIndexRewatchAfterClose registers a failed half again while the
// index closes: the registration is stopped, so Close leaves no watch.
func TestCertIndexRewatchAfterClose(t *testing.T) {
	f := newIndexFixture(t)
	spec := f.add(t, "main", leafOpts{cn: "v1", dns: []string{"a.test"}})
	ix, err := BuildCertIndex([]CertSpec{spec}, f.store)
	if err != nil {
		t.Fatal(err)
	}
	h := &ix.entries[0].cert
	f.store.onWatch = func(secret.Ref, func(secret.Value) error) { ix.Close() }
	rewatch(&ix.mu, &ix.closed, &ix.stops, f.store, h)
	if f.store.watching() != 0 {
		t.Fatalf("%d watches after Close", f.store.watching())
	}
	// Once closed, nothing is registered.
	f.store.onWatch = nil
	rewatch(&ix.mu, &ix.closed, &ix.stops, f.store, h)
	if f.store.watching() != 0 {
		t.Fatalf("%d watches after a rewatch of a closed index", f.store.watching())
	}
}

// TestCertIndexRotationFallbackReq76 pairs a rotated value with the other
// half's latest value delivered to the index when the Store reports the
// other reference unresolved or still holds an older value than the
// watches follow; a value that pairs with neither fails with the first
// pairing's error.
func TestCertIndexRotationFallbackReq76(t *testing.T) {
	tests := []struct {
		name   string
		before func(f *indexFixture, spec CertSpec)
	}{
		{"store holds older values", func(*indexFixture, CertSpec) {}},
		{"store no longer holds the key", func(f *indexFixture, spec CertSpec) { f.store.drop(spec.PrivateKey) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newIndexFixture(t)
			spec := f.add(t, "main", leafOpts{cn: "v1", dns: []string{"a.test"}})
			ix, err := BuildCertIndex([]CertSpec{spec}, f.store)
			if err != nil {
				t.Fatal(err)
			}
			defer ix.Close()
			tt.before(f, spec)
			v2 := f.ca.issue(t, leafOpts{cn: "v2", dns: []string{"a.test"}})
			// The key alone: the current and the latest certificate are v1.
			if n := f.store.deliver(spec.PrivateKey, v2.keyPEM); n != 1 {
				t.Fatalf("key before certificate: %d failures, want 1", n)
			}
			// The certificate: the Store's key is v1 or missing, the latest
			// key delivered is v2.
			if n := f.store.deliver(spec.Certificate, v2.certPEM); n != 0 {
				t.Fatalf("certificate after key: %d failures, want 0", n)
			}
			if got := served(t, ix, "a.test"); got != "v2" {
				t.Fatalf("served %q, want v2", got)
			}
			// Garbage pairs with neither key; the error is the first
			// pairing's and repeats no secret content.
			garbage := []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
			err = ix.rotate(ix.entries[0], true, secret.NewValue(garbage))
			if !errors.Is(err, ErrNoCertificate) || strings.Contains(err.Error(), "BEGIN") {
				t.Fatalf("garbage certificate: %v", err)
			}
			if got := served(t, ix, "a.test"); got != "v2" {
				t.Fatalf("after garbage served %q, want v2", got)
			}
		})
	}
	// Without a Store only the latest value is tried.
	f := newIndexFixture(t)
	l := f.ca.issue(t, leafOpts{cn: "solo", dns: []string{"a.test"}})
	pair, used, current, err := rotatePair(nil, secret.NewValue(l.certPEM), true, ref("unused"), secret.NewValue(l.keyPEM))
	if err != nil || pair.Leaf.Subject.CommonName != "solo" || used.Len() != len(l.keyPEM) || current {
		t.Fatalf("rotatePair without a Store: %v", err)
	}
}

// TestCertIndexRotationDuringBuildReq76 delivers a rotation while the index
// registers its watches: the delivered value wins over the one read by Get.
func TestCertIndexRotationDuringBuildReq76(t *testing.T) {
	f := newIndexFixture(t)
	spec := f.add(t, "main", leafOpts{cn: "old", dns: []string{"a.test"}})
	fresh := f.ca.issue(t, leafOpts{cn: "fresh", dns: []string{"a.test"}})
	var once sync.Once
	f.store.onWatch = func(r secret.Ref, fn func(secret.Value) error) {
		switch r {
		case spec.Certificate:
			if err := fn(secret.NewValue(fresh.certPEM)); err != nil {
				t.Errorf("setup delivery: %v", err)
			}
		case spec.PrivateKey:
			once.Do(func() {
				if err := fn(secret.NewValue(fresh.keyPEM)); err != nil {
					t.Errorf("setup delivery: %v", err)
				}
			})
		}
	}
	ix, err := BuildCertIndex([]CertSpec{spec}, f.store)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if got := served(t, ix, "a.test"); got != "fresh" {
		t.Fatalf("served %q, want the value delivered during the build", got)
	}
}

func TestCertIndexEmpty(t *testing.T) {
	var ix CertIndex
	if ix.Lookup("a") != nil {
		t.Fatal("empty index served a certificate")
	}
	if _, err := ix.GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("empty index must fail the handshake")
	}
}

func TestNormalizeSNI(t *testing.T) {
	for in, want := range map[string]string{
		"a.example.com":  "a.example.com",
		"A.Example.COM":  "a.example.com",
		"a.example.com.": "a.example.com",
		"":               "",
	} {
		if got := normalizeSNI(in); got != want {
			t.Errorf("normalizeSNI(%q) = %q", in, got)
		}
	}
	if n := testing.AllocsPerRun(100, func() { _ = normalizeSNI("a.example.com") }); n != 0 {
		t.Fatalf("normalizeSNI allocates %v times on a lowercase name", n)
	}
}

// TestCertIndexConcurrentReq76 reads while rotating (run with -race).
func TestCertIndexConcurrentReq76(t *testing.T) {
	f := newIndexFixture(t)
	spec := f.add(t, "main", leafOpts{cn: "v0", dns: []string{"a.test"}})
	ix, err := BuildCertIndex([]CertSpec{spec}, f.store)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	pairs := []leaf{
		f.ca.issue(t, leafOpts{cn: "v1", dns: []string{"a.test"}}),
		f.ca.issue(t, leafOpts{cn: "v2", dns: []string{"a.test"}}),
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; i < 20; i++ {
			p := pairs[i%2]
			f.store.rotate(spec.Certificate, p.certPEM)
			f.store.rotate(spec.PrivateKey, p.keyPEM)
		}
		close(done)
	})
	for {
		select {
		case <-done:
			wg.Wait()
			return
		default:
			if c := ix.Lookup("a.test"); c == nil || c.Leaf == nil {
				t.Fatal("no certificate during rotation")
			}
		}
	}
}

func BenchmarkLookup(b *testing.B) {
	ca := newCA(b, "bench")
	st := newFakeStore()
	var specs []CertSpec
	for _, n := range []string{"a", "b", "c", "d"} {
		l := ca.issue(b, leafOpts{cn: n, dns: []string{n + ".example.com", "*." + n + ".example.com"}})
		st.put(ref(n+".crt"), l.certPEM)
		st.put(ref(n+".key"), l.keyPEM)
		specs = append(specs, CertSpec{Name: n, Certificate: ref(n + ".crt"), PrivateKey: ref(n + ".key")})
	}
	ix, err := BuildCertIndex(specs, st)
	if err != nil {
		b.Fatal(err)
	}
	defer ix.Close()
	hello := &tls.ClientHelloInfo{ServerName: "x.c.example.com"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ix.GetCertificate(hello); err != nil {
			b.Fatal(err)
		}
	}
}
