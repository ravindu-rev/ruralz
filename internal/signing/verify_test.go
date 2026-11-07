// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/signing/signingtest"
	"github.com/ravindu-rev/ruralz/internal/testkit/ociregistry"
)

// Tests for Sigstore verification under a trust policy
// (docs/adr/0017-artifact-signing.md "Offline verification", "Bounds";
// docs/architecture/08-security-and-identity.md "Signed Revisions and
// Plugin artifacts"): keyless bundles need a transparency-log inclusion
// proof and a trusted timestamp and name an exact or, opted into, a
// pattern identity; key-based bundles verify against a configured key;
// either signs the manifest digest; referrers are bounded; failures are
// RZ-CFG-033 and unavailability is not.

const (
	testIssuer   = "https://token.actions.example.com"
	testIdentity = "https://github.com/acme/gateway/.github/workflows/push.yml@refs/heads/main"
)

// subjectDigest is the manifest digest the tests sign.
func subjectDigest() revision.Digest { return revision.Sum([]byte(`{"schemaVersion":2}`)) }

// mustStatement returns a signature statement of d with annotations.
func mustStatement(t *testing.T, d revision.Digest, ann map[string]string) []byte {
	t.Helper()
	b, err := SignatureStatement(d, ann)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// keylessVerifier returns a verifier trusting ids under ss's root.
func keylessVerifier(t *testing.T, ss *testSigstore, ids ...Identity) *SigstoreVerifier {
	t.Helper()
	tr, err := ParseTrustedRoot(ss.trustedRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewSigstoreVerifier(Policy{Identities: ids, Root: tr})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// trustKeys returns a verifier trusting keys.
func trustKeys(t *testing.T, keys ...*signingtest.Key) *SigstoreVerifier {
	t.Helper()
	var pks []*PublicKey
	for _, k := range keys {
		pk, err := ParsePublicKey(k.PublicKeyPEM(t))
		if err != nil {
			t.Fatal(err)
		}
		pks = append(pks, pk)
	}
	v, err := NewSigstoreVerifier(Policy{Keys: pks})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyKeyless(t *testing.T) {
	ss := newSigstore(t)
	d := subjectDigest()
	ann := RevisionAnnotations("prod", 7)
	b := ss.keyless(t, testIdentity, testIssuer, mustStatement(t, d, ann), keylessOptions{})
	v := keylessVerifier(t, ss, Identity{Issuer: testIssuer, Subject: testIdentity})
	sig, err := v.VerifyBundle(b, d)
	if err != nil {
		t.Fatal(err)
	}
	if sig.Signer != testIssuer+" "+testIdentity {
		t.Errorf("Signer = %q", sig.Signer)
	}
	if sig.Annotations[AnnotationEnvironment] != "prod" || sig.Annotations[AnnotationSequence] != "7" {
		t.Errorf("Annotations = %v", sig.Annotations)
	}
	if len(sig.Timestamps) == 0 {
		t.Error("no verified timestamp")
	}
	// The signature binds the manifest digest: another digest fails.
	if _, err := v.VerifyBundle(b, revision.Sum([]byte("another manifest"))); err == nil {
		t.Error("the bundle verified for another manifest")
	}
}

func TestVerifyKeylessIdentities(t *testing.T) {
	ss := newSigstore(t)
	d := subjectDigest()
	b := ss.keyless(t, testIdentity, testIssuer, mustStatement(t, d, nil), keylessOptions{})
	tests := []struct {
		name string
		ids  []Identity
		ok   bool
	}{
		{name: "exact", ids: []Identity{{Issuer: testIssuer, Subject: testIdentity}}, ok: true},
		{name: "one of several", ids: []Identity{{Issuer: testIssuer, Subject: "someone@example.com"}, {Issuer: testIssuer, Subject: testIdentity}}, ok: true},
		{name: "subject pattern", ids: []Identity{{Issuer: testIssuer, SubjectPattern: `https://github\.com/acme/[^/]+/\.github/workflows/push\.yml@refs/heads/main`}}, ok: true},
		{name: "issuer pattern", ids: []Identity{{IssuerPattern: `https://token\.actions\..*`, Subject: testIdentity}}, ok: true},
		{name: "pattern matching part of the subject only", ids: []Identity{{Issuer: testIssuer, SubjectPattern: `acme/gateway`}}},
		{name: "pattern matching a prefix only", ids: []Identity{{Issuer: testIssuer, SubjectPattern: `https://github\.com/acme/`}}},
		{name: "other subject", ids: []Identity{{Issuer: testIssuer, Subject: testIdentity + "x"}}},
		{name: "other issuer", ids: []Identity{{Issuer: "https://accounts.example.com", Subject: testIdentity}}},
		{name: "subject differs in case", ids: []Identity{{Issuer: testIssuer, Subject: strings.ToUpper(testIdentity)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := keylessVerifier(t, ss, tt.ids...).VerifyBundle(b, d)
			if (err == nil) != tt.ok {
				t.Fatalf("VerifyBundle = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

func TestVerifyKeylessNeedsLogAndTimestamp(t *testing.T) {
	ss := newSigstore(t)
	d := subjectDigest()
	st := mustStatement(t, d, nil)
	v := keylessVerifier(t, ss, Identity{Issuer: testIssuer, Subject: testIdentity})
	tests := []struct {
		name string
		o    keylessOptions
		ok   bool
	}{
		{name: "log entry and both timestamps", ok: true},
		{name: "the log's timestamp alone", o: keylessOptions{noTimestamp: true}, ok: true},
		{name: "the authority's timestamp alone", o: keylessOptions{noInclusionPromise: true}, ok: true},
		{name: "no transparency-log entry", o: keylessOptions{noTlog: true}},
		{name: "no trusted timestamp", o: keylessOptions{noTimestamp: true, noInclusionPromise: true}},
		{name: "tampered statement", o: keylessOptions{payload: mustStatement(t, d, RevisionAnnotations("prod", 99))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.VerifyBundle(ss.keyless(t, testIdentity, testIssuer, st, tt.o), d)
			if (err == nil) != tt.ok {
				t.Fatalf("VerifyBundle = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

func TestVerifyKeylessCertificateIssuer(t *testing.T) {
	// The identity is the issuer the certificate authority certified with
	// the subject: the same subject from another issuer is another signer.
	ss := newSigstore(t)
	d := subjectDigest()
	const otherIssuer = "https://accounts.example.com"
	b := ss.keyless(t, testIdentity, otherIssuer, mustStatement(t, d, nil), keylessOptions{})
	if _, err := keylessVerifier(t, ss, Identity{Issuer: testIssuer, Subject: testIdentity}).VerifyBundle(b, d); err == nil {
		t.Fatal("a certificate of another issuer verified")
	}
	sig, err := keylessVerifier(t, ss, Identity{Issuer: otherIssuer, Subject: testIdentity}).VerifyBundle(b, d)
	if err != nil || sig.Signer != otherIssuer+" "+testIdentity {
		t.Fatalf("VerifyBundle = %+v, %v", sig, err)
	}
}

// indefiniteToken re-encodes the explicit [0] content of the token in an
// RFC 3161 response in BER's indefinite-length form, which BER decoders
// such as the one sigstore-go's timestamp check links take and DER
// forbids.
func indefiniteToken(t *testing.T, resp []byte) []byte {
	t.Helper()
	next := func(off, limit int) (content, end int) {
		t.Helper()
		_, c, e, err := readTLV(resp, off, limit)
		if err != nil {
			t.Fatal(err)
		}
		return c, e
	}
	rc, re := next(0, len(resp)) // TimeStampResp
	_, se := next(rc, re)        // status
	tc, te := next(se, re)       // timeStampToken, a ContentInfo
	_, oe := next(tc, te)        // contentType
	wc, we := next(oe, te)       // [0] EXPLICIT content
	if resp[oe] != 0xa0 {
		t.Fatalf("token content identifier %#x", resp[oe])
	}
	wrapper := append([]byte{0xa0, 0x80}, resp[wc:we]...)
	wrapper = append(wrapper, 0x00, 0x00)
	token := seq(append(slices.Clone(resp[tc:oe]), wrapper...))
	return seq(append(slices.Clone(resp[rc:se]), token...))
}

func TestVerifyBundleTimestampDER(t *testing.T) {
	// The bundle format encodes each RFC 3161 timestamp in DER; a bundle
	// with one that is not fails before any timestamp is parsed, whether
	// or not another timestamp would verify the signature.
	ss := newSigstore(t)
	d := subjectDigest()
	st := mustStatement(t, d, nil)
	v := keylessVerifier(t, ss, Identity{Issuer: testIssuer, Subject: testIdentity})
	tests := []struct {
		name string
		edit func(t *testing.T, ts [][]byte) [][]byte
		want []string // nil verifies
	}{
		{name: "DER"},
		{
			name: "indefinite length inside the token",
			edit: func(t *testing.T, ts [][]byte) [][]byte { return [][]byte{indefiniteToken(t, ts[0])} },
			want: []string{"timestamp 1: ", "indefinite length"},
		},
		{
			name: "data after the response",
			edit: func(_ *testing.T, ts [][]byte) [][]byte { return [][]byte{append(slices.Clone(ts[0]), 0x00)} },
			want: []string{"timestamp 1: ", "data after the element"},
		},
		{
			name: "crafted BER that exhausts the BER decoder",
			edit: func(t *testing.T, _ [][]byte) [][]byte { return [][]byte{overrunTimestamp(t)} },
			want: []string{"timestamp 1: offset 11: indefinite length"},
		},
		{
			name: "a second timestamp not in DER",
			edit: func(t *testing.T, ts [][]byte) [][]byte { return append(ts, overrunTimestamp(t)) },
			want: []string{"timestamp 2: offset 11: indefinite length"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.VerifyBundle(ss.keyless(t, testIdentity, testIssuer, st, keylessOptions{timestamps: tt.edit}), d)
			if tt.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrTimestampEncoding) {
				t.Fatalf("VerifyBundle = %v, want ErrTimestampEncoding", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("VerifyBundle = %v, want %q", err, w)
				}
			}
		})
	}
}

func TestVerifyKeyBundleTimestampDER(t *testing.T) {
	// A key-based verification parses no timestamp, and a bundle whose
	// timestamp is not DER still fails: the format rule is the bundle's.
	k := signingtest.NewKey(t)
	d := subjectDigest()
	var b signingtest.Bundle
	if err := json.Unmarshal(k.Sign(t, mustStatement(t, d, nil), nil), &b); err != nil {
		t.Fatal(err)
	}
	if _, err := trustKeys(t, k).VerifyBundle(b.JSON(t), d); err != nil {
		t.Fatalf("the bundle without timestamps: %v", err)
	}
	b.VerificationMaterial.TimestampVerificationData = &signingtest.TimestampData{
		RFC3161Timestamps: []signingtest.Timestamp{{SignedTimestamp: overrunTimestamp(t)}},
	}
	if _, err := trustKeys(t, k).VerifyBundle(b.JSON(t), d); !errors.Is(err, ErrTimestampEncoding) {
		t.Fatalf("VerifyBundle = %v, want ErrTimestampEncoding", err)
	}
}

func TestVerifyKeylessUntrustedRoot(t *testing.T) {
	trusted, other := newSigstore(t), newSigstore(t)
	d := subjectDigest()
	b := other.keyless(t, testIdentity, testIssuer, mustStatement(t, d, nil), keylessOptions{})
	if _, err := keylessVerifier(t, trusted, Identity{Issuer: testIssuer, Subject: testIdentity}).VerifyBundle(b, d); err == nil {
		t.Fatal("a bundle of another Sigstore deployment verified")
	}
}

func TestVerifyStatementRules(t *testing.T) {
	ss := newSigstore(t)
	d := subjectDigest()
	v := keylessVerifier(t, ss, Identity{Issuer: testIssuer, Subject: testIdentity})
	other := revision.Sum([]byte("other"))
	statement := func(predicate string, digests ...revision.Digest) []byte {
		var subj []map[string]any
		for _, dg := range digests {
			subj = append(subj, map[string]any{"digest": map[string]string{"sha256": dg.Hex()}})
		}
		b, err := json.Marshal(map[string]any{"_type": StatementType, "subject": subj, "predicateType": predicate, "predicate": map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tests := []struct {
		name string
		st   []byte
		want string
	}{
		{name: "an attestation, not a signature", st: statement("https://slsa.dev/provenance/v1", d), want: "predicate type"},
		{name: "another manifest", st: statement(PredicateCosignSign, other), want: "does not name the manifest digest"},
		{name: "the manifest twice", st: statement(PredicateCosignSign, d, d), want: "twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.VerifyBundle(ss.keyless(t, testIdentity, testIssuer, tt.st, keylessOptions{}), d)
			if err == nil {
				t.Fatal("VerifyBundle succeeded")
			}
			// sigstore-go refuses a statement that misses the digest first.
			if tt.want != "does not name the manifest digest" && !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("VerifyBundle = %v, want %q", err, tt.want)
			}
		})
	}
	// Annotations that are not strings are dropped, the rest kept.
	b, err := json.Marshal(map[string]any{
		"_type": StatementType, "predicateType": PredicateCosignSign, "predicate": map[string]any{},
		"subject": []map[string]any{{"digest": map[string]string{"sha256": d.Hex()}, "annotations": map[string]any{
			AnnotationEnvironment: "prod", AnnotationSequence: 3, "flag": true,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := v.VerifyBundle(ss.keyless(t, testIdentity, testIssuer, b, keylessOptions{}), d)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig.Annotations) != 1 || sig.Annotations[AnnotationEnvironment] != "prod" {
		t.Fatalf("Annotations = %v", sig.Annotations)
	}
}

func TestVerifyKeyBased(t *testing.T) {
	k1, k2 := signingtest.NewKey(t), signingtest.NewKey(t)
	d := subjectDigest()
	ann := RevisionAnnotations("staging", 3)
	b := k1.Sign(t, mustStatement(t, d, ann), nil)
	sig, err := trustKeys(t, k2, k1).VerifyBundle(b, d)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ParsePublicKey(k1.PublicKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	if sig.Signer != "key "+pk.ID() || sig.Annotations[AnnotationEnvironment] != "staging" || len(sig.Timestamps) != 0 {
		t.Fatalf("signature = %+v", sig)
	}
	if _, err := trustKeys(t, k2).VerifyBundle(b, d); err == nil {
		t.Fatal("a bundle verified with another key")
	}
	if _, err := trustKeys(t, k1).VerifyBundle(k1.Sign(t, mustStatement(t, d, ann), mustStatement(t, d, RevisionAnnotations("prod", 3))), d); err == nil {
		t.Fatal("a tampered statement verified")
	}
	// A message signature signs the digest and no annotation.
	raw := d.Bytes()
	msig, err := trustKeys(t, k1).VerifyBundle(k1.SignDigest(t, raw[:]), d)
	if err != nil {
		t.Fatal(err)
	}
	if msig.Annotations != nil {
		t.Fatalf("a message signature has annotations %v", msig.Annotations)
	}
	if _, err := trustKeys(t, k1).VerifyBundle(k1.SignDigest(t, raw[:]), revision.Sum([]byte("other"))); err == nil {
		t.Fatal("a message signature verified for another digest")
	}
}

func TestVerifyBundleKindMismatch(t *testing.T) {
	ss := newSigstore(t)
	k := signingtest.NewKey(t)
	d := subjectDigest()
	st := mustStatement(t, d, nil)
	if _, err := trustKeys(t, k).VerifyBundle(ss.keyless(t, testIdentity, testIssuer, st, keylessOptions{}), d); err == nil || !strings.Contains(err.Error(), "no keyless identity") {
		t.Fatalf("keyless bundle under a key-only policy: %v", err)
	}
	if _, err := keylessVerifier(t, ss, Identity{Issuer: testIssuer, Subject: testIdentity}).VerifyBundle(k.Sign(t, st, nil), d); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Fatalf("key bundle under an identity-only policy: %v", err)
	}
	if _, err := trustKeys(t, k).VerifyBundle([]byte(`{"mediaType":"nonsense"}`), d); err == nil {
		t.Fatal("garbage verified")
	}
	if _, err := trustKeys(t, k).VerifyBundle(k.Sign(t, st, nil), revision.Digest{}); !errors.Is(err, ErrNoDigest) {
		t.Fatalf("zero subject: %v", err)
	}
}

func TestNewSigstoreVerifierRefuses(t *testing.T) {
	ss := newSigstore(t)
	tr, err := ParseTrustedRoot(ss.trustedRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]Policy{
		"identity without issuer":       {Root: tr, Identities: []Identity{{Subject: testIdentity}}},
		"identity without subject":      {Root: tr, Identities: []Identity{{Issuer: testIssuer}}},
		"issuer and issuer pattern":     {Root: tr, Identities: []Identity{{Issuer: testIssuer, IssuerPattern: ".*", Subject: testIdentity}}},
		"subject and subject pattern":   {Root: tr, Identities: []Identity{{Issuer: testIssuer, Subject: testIdentity, SubjectPattern: ".*"}}},
		"pattern that does not compile": {Root: tr, Identities: []Identity{{Issuer: testIssuer, SubjectPattern: "("}}},
		"identities without a root":     {Identities: []Identity{{Issuer: testIssuer, Subject: testIdentity}}},
		"unknown mode":                  {Mode: ModeOff + 1},
		"nil key":                       {Keys: []*PublicKey{nil}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSigstoreVerifier(p); !errors.Is(err, ErrPolicy) {
				t.Fatalf("NewSigstoreVerifier = %v, want ErrPolicy", err)
			}
		})
	}
}

func TestParseTrustedRoot(t *testing.T) {
	ss := newSigstore(t)
	if _, err := ParseTrustedRoot(ss.trustedRoot(t)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "{", `{"mediaType":"application/json"}`} {
		if _, err := ParseTrustedRoot([]byte(bad)); !errors.Is(err, ErrTrustedRoot) {
			t.Errorf("ParseTrustedRoot(%q) = %v", bad, err)
		}
	}
}

// pemPublicKey encodes pub as a PEM PUBLIC KEY block.
func pemPublicKey(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestParsePublicKey(t *testing.T) {
	ec := func(c elliptic.Curve) any {
		k, err := ecdsa.GenerateKey(c, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return &k.PublicKey
	}
	rsaKey := func(bits int) any {
		k, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			t.Fatal(err)
		}
		return &k.PublicKey
	}
	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, pub := range map[string]any{
		"P-256": ec(elliptic.P256()), "P-384": ec(elliptic.P384()), "P-521": ec(elliptic.P521()),
		"Ed25519": edPub, "RSA 2048": rsaKey(2048),
	} {
		k, err := ParsePublicKey(pemPublicKey(t, pub))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.HasPrefix(k.ID(), "sha256:") || len(k.ID()) != len("sha256:")+64 {
			t.Errorf("%s: ID %q", name, k.ID())
		}
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"RSA 1024":      pemPublicKey(t, rsaKey(1024)),
		"private key":   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}),
		"not PEM":       []byte("ssh-ed25519 AAAA"),
		"trailing data": append(pemPublicKey(t, ec(elliptic.P256())), []byte("extra")...),
		"two keys":      append(pemPublicKey(t, ec(elliptic.P256())), pemPublicKey(t, ec(elliptic.P256()))...),
		"bad DER":       pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("nope")}),
	} {
		if _, err := ParsePublicKey(data); !errors.Is(err, ErrPublicKey) {
			t.Errorf("%s: %v, want ErrPublicKey", name, err)
		}
	}
}

func TestModeString(t *testing.T) {
	for m, want := range map[Mode]string{ModeEnforce: "enforce", ModeWarn: "warn", ModeOff: "off", ModeOff + 1: "Mode(3)"} {
		if got := m.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", m, got, want)
		}
	}
	if got := (Identity{IssuerPattern: "a", Subject: "b"}).String(); got != "~a b" {
		t.Errorf("Identity.String() = %q", got)
	}
}

// signedArtifact pushes an artifact to r and returns its manifest
// descriptor.
func signedArtifact(ctx context.Context, t *testing.T, r *Repository) Descriptor {
	t.Helper()
	md, _ := artifact(ctx, t, r, "prod", []byte("canonical revision"))
	return md
}

// created is the time the test signatures say they were pushed at.
var created = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func TestVerifyReferrers(t *testing.T) {
	ss := newSigstore(t)
	s := ociregistry.Start(t, ociregistry.Options{})
	r := newRepo(t, s, nil)
	ctx := t.Context()
	md := signedArtifact(ctx, t, r)
	v := keylessVerifier(t, ss, Identity{Issuer: testIssuer, Subject: testIdentity})

	// No signature at all.
	_, err := v.VerifyReferrers(ctx, r, md)
	if !errors.Is(err, ErrUnsigned) || !strings.Contains(err.Error(), "no Sigstore bundle referrer") {
		t.Fatalf("unsigned: %v", err)
	}
	if code, _ := errcode.CodeOf(err); code != "RZ-CFG-033" {
		t.Fatalf("unsigned code %q", code)
	}
	// An invalid signature (another identity) and then a valid one.
	st := mustStatement(t, md.Digest, RevisionAnnotations("prod", 2))
	bad := ss.keyless(t, "intruder@example.com", testIssuer, st, keylessOptions{})
	if _, err := PushSignature(ctx, r, md, bad, created); err != nil {
		t.Fatal(err)
	}
	_, err = v.VerifyReferrers(ctx, r, md)
	if !errors.Is(err, ErrUnsigned) || !strings.Contains(err.Error(), "none of the 1 Sigstore bundles") {
		t.Fatalf("only an invalid signature: %v", err)
	}
	good := ss.keyless(t, testIdentity, testIssuer, st, keylessOptions{})
	ref, err := PushSignature(ctx, r, md, good, created)
	if err != nil {
		t.Fatal(err)
	}
	res, err := v.VerifyReferrers(ctx, r, md)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tried != 2 || len(res.Valid) != 1 || len(res.Failures) != 1 || res.Valid[0].Referrer.Digest != ref.Digest {
		t.Fatalf("Result = %+v", res)
	}
	b, err := BindRevision(res.Valid, "prod", 2)
	if err != nil || b.Sequence != 2 {
		t.Fatalf("BindRevision = %+v, %v", b, err)
	}
	if _, err := BindRevision(res.Valid, "staging", 0); !errors.Is(err, ErrEnvironment) {
		t.Fatalf("another Environment: %v", err)
	}
	if _, err := BindRevision(res.Valid, "prod", 3); !errors.Is(err, ErrSequence) {
		t.Fatalf("older sequence: %v", err)
	}
}

func TestVerifyReferrersBounds(t *testing.T) {
	k := signingtest.NewKey(t)
	s := ociregistry.Start(t, ociregistry.Options{})
	r := newRepo(t, s, nil)
	ctx := t.Context()
	md := signedArtifact(ctx, t, r)
	v := trustKeys(t, k)
	other := signingtest.NewKey(t)
	st := mustStatement(t, md.Digest, nil)
	// 16 signatures by an untrusted key, then a valid one: only the first
	// 16 are tried.
	for i := range MaxSignatures {
		if _, err := PushSignature(ctx, r, md, other.Sign(t, mustStatement(t, md.Digest, map[string]string{"n": fmt.Sprint(i)}), nil), created); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := PushSignature(ctx, r, md, k.Sign(t, st, nil), created); err != nil {
		t.Fatal(err)
	}
	res, err := v.VerifyReferrers(ctx, r, md)
	if !errors.Is(err, ErrUnsigned) || res.Tried != MaxSignatures {
		t.Fatalf("17 referrers: %+v, %v", res, err)
	}
}

func TestVerifyReferrersOversizedAndTampered(t *testing.T) {
	k := signingtest.NewKey(t)
	s := ociregistry.Start(t, ociregistry.Options{})
	r := newRepo(t, s, nil)
	ctx := t.Context()
	md := signedArtifact(ctx, t, r)
	v := trustKeys(t, k)
	good := k.Sign(t, mustStatement(t, md.Digest, nil), nil)

	// A bundle over the size bound fails as a referrer, never fetched.
	cfg, err := r.PushBlob(ctx, MediaTypeEmpty, []byte(emptyConfig))
	if err != nil {
		t.Fatal(err)
	}
	huge := Descriptor{MediaType: MediaTypeSigstoreBundle, Digest: revision.Sum([]byte("huge")), Size: MaxSignatureBytes + 1}
	sub := jsonDescriptor(md)
	cfgDesc := jsonDescriptor(cfg)
	content, err := json.Marshal(referrerManifest{
		SchemaVersion: 2, MediaType: MediaTypeImageManifest, ArtifactType: MediaTypeSigstoreBundle,
		Config: cfgDesc, Layers: []ociDescriptor{jsonDescriptor(huge)}, Subject: &sub,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.PushManifest(ctx, MediaTypeImageManifest, content, ""); err != nil {
		t.Fatal(err)
	}
	res, err := v.VerifyReferrers(ctx, r, md)
	if !errors.Is(err, ErrUnsigned) || len(res.Failures) != 1 || !strings.Contains(res.Failures[0], "more than") {
		t.Fatalf("oversized bundle: %+v, %v", res, err)
	}
	// A bundle whose bytes do not match its digest fails, and the next
	// referrer still verifies.
	ref, err := PushSignature(ctx, r, md, good, created)
	if err != nil {
		t.Fatal(err)
	}
	layerDigest := revision.Sum(good)
	s.Tamper(layerDigest.String(), append([]byte(nil), good[:len(good)-1]...))
	res, err = v.VerifyReferrers(ctx, r, md)
	if !errors.Is(err, ErrUnsigned) || len(res.Failures) != 2 {
		t.Fatalf("tampered bundle: %+v, %v", res, err)
	}
	s.Tamper(layerDigest.String(), nil)
	res, err = v.VerifyReferrers(ctx, r, md)
	if err != nil || len(res.Valid) != 1 || res.Valid[0].Referrer.Digest != ref.Digest {
		t.Fatalf("after the tampering: %+v, %v", res, err)
	}
}

func TestVerifyReferrersTransient(t *testing.T) {
	k := signingtest.NewKey(t)
	s := ociregistry.Start(t, ociregistry.Options{})
	clk := clocktest.New(created)
	r, err := NewRepository(Reference{Registry: s.Host(), Repository: "acme/config"}, RegistryOptions{RootCAs: s.RootCAs(), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	md := signedArtifact(ctx, t, r)
	if _, err := PushSignature(ctx, r, md, k.Sign(t, mustStatement(t, md.Digest, nil), nil), created); err != nil {
		t.Fatal(err)
	}
	v := trustKeys(t, k)

	// A registry that answers 500 is unavailable, not unsigned.
	s.Intercept(func(w http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/referrers/") {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})
	_, err = v.VerifyReferrers(ctx, r, md)
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnsigned) {
		t.Fatalf("500 while listing: %v", err)
	}
	if code, ok := errcode.CodeOf(err); ok {
		t.Fatalf("an unavailable registry carries code %q", code)
	}
	// A check that runs past its 10 s budget times out, transient.
	s.Intercept(func(_ http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/referrers/") {
			<-req.Context().Done()
			return true
		}
		return false
	})
	done := make(chan error, 1)
	go func() {
		_, err := v.VerifyReferrers(ctx, r, md)
		done <- err
	}()
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(SignatureTimeout)
	err = <-done
	if !errors.Is(err, ErrTimeout) || errors.Is(err, ErrUnsigned) {
		t.Fatalf("past the budget: %v", err)
	}
	s.Intercept(nil)
	if _, err := v.VerifyReferrers(ctx, r, md); err != nil {
		t.Fatalf("once the registry answers again: %v", err)
	}
}

func TestVerifyReferrersEmptyAndOff(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{})
	r := newRepo(t, s, nil)
	ctx := t.Context()
	md := signedArtifact(ctx, t, r)
	before := len(s.Requests())
	empty, err := NewSigstoreVerifier(Policy{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = empty.VerifyReferrers(ctx, r, md)
	if !errors.Is(err, ErrUnsigned) || !strings.Contains(err.Error(), "trusts no identity") {
		t.Fatalf("empty policy: %v", err)
	}
	if code, _ := errcode.CodeOf(err); code != "RZ-CFG-033" {
		t.Fatalf("empty policy code %q", code)
	}
	off, err := NewSigstoreVerifier(Policy{Mode: ModeOff})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := off.VerifyReferrers(ctx, r, md); err != nil || res.Tried != 0 || off.Mode() != ModeOff {
		t.Fatalf("off: %+v, %v", res, err)
	}
	if n := len(s.Requests()); n != before {
		t.Fatalf("an empty or off policy asked the registry %d times", n-before)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	k := signingtest.NewKey(t)
	if _, err := trustKeys(t, k).VerifyReferrers(cctx, r, md); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestBundleLayer(t *testing.T) {
	d := subjectDigest()
	subject := Descriptor{MediaType: MediaTypeImageManifest, Digest: d, Size: 20}
	layer := ociDescriptor{MediaType: MediaTypeSigstoreBundle, Digest: revision.Sum([]byte("b")).String(), Size: 1}
	sub := jsonDescriptor(subject)
	otherSub := jsonDescriptor(Descriptor{Digest: revision.Sum([]byte("x"))})
	good := referrerManifest{SchemaVersion: 2, MediaType: MediaTypeImageManifest, ArtifactType: MediaTypeSigstoreBundle, Layers: []ociDescriptor{layer}, Subject: &sub}
	tests := map[string]func(m *referrerManifest){
		"another media type":    func(m *referrerManifest) { m.MediaType = "application/vnd.oci.image.index.v1+json" },
		"another artifact type": func(m *referrerManifest) { m.ArtifactType = "application/vnd.example" },
		"another subject":       func(m *referrerManifest) { m.Subject = &otherSub },
		"no subject":            func(m *referrerManifest) { m.Subject = nil },
		"no bundle layer":       func(m *referrerManifest) { m.Layers = nil },
		"two bundle layers":     func(m *referrerManifest) { m.Layers = []ociDescriptor{layer, layer} },
		"layer without sha256": func(m *referrerManifest) {
			m.Layers = []ociDescriptor{{MediaType: MediaTypeSigstoreBundle, Digest: "sha512:00"}}
		},
	}
	enc := func(m referrerManifest) []byte {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if got, reason := bundleLayer(enc(good), subject); reason != "" || got.Digest.String() != layer.Digest {
		t.Fatalf("bundleLayer(good) = %+v, %q", got, reason)
	}
	for name, mutate := range tests {
		m := good
		mutate(&m)
		if _, reason := bundleLayer(enc(m), subject); reason == "" {
			t.Errorf("%s: no reason", name)
		}
	}
	if _, reason := bundleLayer([]byte("not json"), subject); reason == "" {
		t.Error("not JSON: no reason")
	}
}
