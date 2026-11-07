// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// CodeSignature is a Revision or Plugin artifact whose signature does not
// verify: no valid signature within the bounds, an empty trust policy, or
// a Revision bound to another Environment or an older sequence.
const CodeSignature = "RZ-CFG-033"

// Signature bounds (target values of docs/adr/0017-artifact-signing.md
// "Bounds"): a verifier lists only Sigstore bundle referrers and tries at
// most MaxSignatures of at most MaxSignatureBytes each within
// SignatureTimeout.
const (
	MaxSignatures     = 16
	MaxSignatureBytes = 1 << 20
	SignatureTimeout  = 10 * time.Second
	// minRSABits is the smallest RSA key a trust policy takes.
	minRSABits = 2048
)

// Errors of trust policies and verification.
var (
	// ErrPolicy reports a trust policy that cannot be used: an identity
	// without an issuer or subject, a pattern that does not compile, keyless
	// identities without a trusted root.
	ErrPolicy = errors.New("signing: invalid trust policy")
	// ErrTrustedRoot reports a Sigstore trusted root that does not decode.
	ErrTrustedRoot = errors.New("signing: invalid Sigstore trusted root")
	// ErrPublicKey reports a public key a trust policy cannot take.
	ErrPublicKey = errors.New("signing: invalid public key")
	// ErrUnsigned reports an artifact without a valid signature: RZ-CFG-033.
	ErrUnsigned = errors.New("signing: no valid signature")
)

// Mode is what a trust policy does with a signature that does not verify.
type Mode uint8

// Modes.
const (
	// ModeEnforce refuses an artifact without a valid signature: the
	// default.
	ModeEnforce Mode = iota
	// ModeWarn accepts it with a warning.
	ModeWarn
	// ModeOff checks no signature: a degraded state.
	ModeOff
)

// String returns enforce, warn or off.
func (m Mode) String() string {
	switch m {
	case ModeEnforce:
		return "enforce"
	case ModeWarn:
		return "warn"
	case ModeOff:
		return "off"
	default:
		return fmt.Sprintf("Mode(%d)", uint8(m))
	}
}

// Identity is a trusted keyless signer: the OIDC issuer and the subject
// (the certificate's subject alternative name) that a Sigstore certificate
// authority certified. Each is exact, or, opted into, an RE2 pattern that
// must match the whole value; an identity names both.
type Identity struct {
	Issuer, Subject               string
	IssuerPattern, SubjectPattern string
}

// String names the identity.
func (i Identity) String() string {
	issuer, subject := i.Issuer, i.Subject
	if i.IssuerPattern != "" {
		issuer = "~" + i.IssuerPattern
	}
	if i.SubjectPattern != "" {
		subject = "~" + i.SubjectPattern
	}
	return issuer + " " + subject
}

// TrustedRoot is a Sigstore trusted root: the certificate authorities,
// transparency logs and timestamp authorities keyless signatures verify
// against, read locally and never fetched.
type TrustedRoot struct {
	tr *root.TrustedRoot
}

// ParseTrustedRoot decodes a Sigstore trusted root in its JSON form
// (media type application/vnd.dev.sigstore.trustedroot+json;version=0.1).
// Every error matches ErrTrustedRoot.
func ParseTrustedRoot(data []byte) (*TrustedRoot, error) {
	tr, err := root.NewTrustedRootFromJSON(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTrustedRoot, err)
	}
	return &TrustedRoot{tr: tr}, nil
}

// PublicKey is a trusted signing key.
type PublicKey struct {
	id       string
	verifier signature.Verifier
}

// ParsePublicKey decodes a PEM PUBLIC KEY block (PKIX): ECDSA on P-256,
// P-384 or P-521, verified with SHA-256, SHA-384 or SHA-512; Ed25519; or
// RSA of at least 2048 bits with PKCS #1 v1.5 and SHA-256. Every error
// matches ErrPublicKey.
func ParsePublicKey(data []byte) (*PublicKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) > 0 {
		return nil, fmt.Errorf("%w: want one PEM PUBLIC KEY block", ErrPublicKey)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPublicKey, err)
	}
	var v signature.Verifier
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		var h crypto.Hash
		switch k.Curve {
		case elliptic.P256():
			h = crypto.SHA256
		case elliptic.P384():
			h = crypto.SHA384
		case elliptic.P521():
			h = crypto.SHA512
		default:
			return nil, fmt.Errorf("%w: ECDSA curve %s", ErrPublicKey, k.Curve.Params().Name)
		}
		v, err = signature.LoadECDSAVerifier(k, h)
	case ed25519.PublicKey:
		v, err = signature.LoadED25519Verifier(k)
	case *rsa.PublicKey:
		if k.N.BitLen() < minRSABits {
			return nil, fmt.Errorf("%w: RSA key of %d bits, fewer than %d", ErrPublicKey, k.N.BitLen(), minRSABits)
		}
		v, err = signature.LoadRSAPKCS1v15Verifier(k, crypto.SHA256)
	default:
		return nil, fmt.Errorf("%w: key type %T", ErrPublicKey, pub)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPublicKey, err)
	}
	sum := sha256.Sum256(block.Bytes)
	return &PublicKey{id: "sha256:" + hex.EncodeToString(sum[:]), verifier: v}, nil
}

// ID returns sha256:<64 hex> of the key's DER encoding.
func (k *PublicKey) ID() string { return k.id }

// Policy is the trust policy of one kind of artifact.
type Policy struct {
	// Mode is enforce, warn or off.
	Mode Mode
	// Identities are the trusted keyless signers.
	Identities []Identity
	// Keys are the trusted signing keys.
	Keys []*PublicKey
	// Root is the Sigstore trusted root keyless signatures verify against;
	// required with Identities.
	Root *TrustedRoot
}

// Empty reports whether the policy trusts nothing.
func (p Policy) Empty() bool { return len(p.Identities) == 0 && len(p.Keys) == 0 }

// SigstoreVerifier verifies Sigstore signatures of OCI artifacts under one
// Policy, offline: trust roots come only from the Policy. A keyless
// signature verifies when its bundle carries a transparency-log inclusion
// proof, which the log's key in the trusted root verifies, and at least
// one trusted timestamp (the log's or a timestamp authority's), at which
// the certificate chains to a trusted certificate authority and names a
// trusted identity. A key-based signature verifies against a trusted key.
// Either signs the artifact's manifest digest: a signature statement
// (SignatureStatement) whose subject is that digest, or a message
// signature over it. SigstoreVerifier is safe for concurrent use.
type SigstoreVerifier struct {
	mode    Mode
	empty   bool
	keyless *verify.Verifier
	ids     []verify.PolicyOption
	keys    []keyVerifier
}

// keyVerifier verifies with one trusted key.
type keyVerifier struct {
	id string
	v  *verify.Verifier
}

// NewSigstoreVerifier compiles p. Every error matches ErrPolicy.
func NewSigstoreVerifier(p Policy) (*SigstoreVerifier, error) {
	v := &SigstoreVerifier{mode: p.Mode, empty: p.Empty()}
	if p.Mode > ModeOff {
		return nil, fmt.Errorf("%w: mode %v", ErrPolicy, p.Mode)
	}
	for i, id := range p.Identities {
		ci, err := certificateIdentity(id)
		if err != nil {
			return nil, fmt.Errorf("%w: identity %d: %w", ErrPolicy, i+1, err)
		}
		v.ids = append(v.ids, verify.WithCertificateIdentity(ci))
	}
	if len(p.Identities) > 0 {
		if p.Root == nil {
			return nil, fmt.Errorf("%w: keyless identities need a Sigstore trusted root", ErrPolicy)
		}
		kv, err := verify.NewVerifier(p.Root.tr, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrPolicy, err)
		}
		v.keyless = kv
	}
	for _, k := range p.Keys {
		if k == nil {
			return nil, fmt.Errorf("%w: a nil key", ErrPolicy)
		}
		ek := root.NewExpiringKey(k.verifier, time.Time{}, time.Time{})
		material := root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) { return ek, nil })
		kv, err := verify.NewVerifier(material, verify.WithNoObserverTimestamps())
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrPolicy, err)
		}
		v.keys = append(v.keys, keyVerifier{id: k.id, v: kv})
	}
	return v, nil
}

// certificateIdentity converts an identity, anchoring its patterns so they
// match whole values.
func certificateIdentity(id Identity) (verify.CertificateIdentity, error) {
	switch {
	case (id.Issuer == "") == (id.IssuerPattern == ""):
		return verify.CertificateIdentity{}, errors.New("want exactly one of an issuer and an issuer pattern")
	case (id.Subject == "") == (id.SubjectPattern == ""):
		return verify.CertificateIdentity{}, errors.New("want exactly one of a subject and a subject pattern")
	}
	issuerRE, err := anchored(id.IssuerPattern)
	if err != nil {
		return verify.CertificateIdentity{}, fmt.Errorf("issuer pattern: %w", err)
	}
	subjectRE, err := anchored(id.SubjectPattern)
	if err != nil {
		return verify.CertificateIdentity{}, fmt.Errorf("subject pattern: %w", err)
	}
	return verify.NewShortCertificateIdentity(id.Issuer, issuerRE, id.Subject, subjectRE)
}

// anchored returns pattern wrapped to match a whole value, "" for none.
func anchored(pattern string) (string, error) {
	if pattern == "" {
		return "", nil
	}
	p := `^(?:` + pattern + `)$`
	if _, err := regexp.Compile(p); err != nil {
		return "", err
	}
	return p, nil
}

// Mode returns the policy's mode.
func (v *SigstoreVerifier) Mode() Mode { return v.mode }

// Signature is one signature that verified.
type Signature struct {
	// Referrer is the referrer manifest that carried the bundle; zero for
	// a bundle verified directly (VerifyBundle).
	Referrer Descriptor
	// Signer names who signed: "<issuer> <subject>" for a keyless
	// signature, "key sha256:<64 hex>" for a key.
	Signer string
	// Annotations are the signed annotations of the statement's subject;
	// nil for a message signature, which signs none.
	Annotations map[string]string
	// Timestamps are the verified times of a keyless signature, at which
	// its certificate was valid; none for a key.
	Timestamps []time.Time
}

// VerifyBundle verifies data, a Sigstore bundle in its JSON form, as a
// signature of the manifest subject. A bundle whose RFC 3161 timestamp is
// not DER fails with ErrTimestampEncoding before any timestamp is parsed.
// The error names why it does not verify and carries no RZ code:
// VerifyReferrers decides what a failure means.
func (v *SigstoreVerifier) VerifyBundle(data []byte, subject revision.Digest) (Signature, error) {
	if subject.IsZero() {
		return Signature{}, ErrNoDigest
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(data); err != nil {
		return Signature{}, fmt.Errorf("not a Sigstore bundle: %w", err)
	}
	// Each RFC 3161 timestamp is DER, checked before sigstore-go parses it
	// (checkDER).
	timestamps, err := b.Timestamps()
	if err != nil {
		return Signature{}, fmt.Errorf("bundle verification material: %w", err)
	}
	for i, ts := range timestamps {
		if err := checkDER(ts); err != nil {
			return Signature{}, fmt.Errorf("%w: timestamp %d: %w", ErrTimestampEncoding, i+1, err)
		}
	}
	vc, err := b.VerificationContent()
	if err != nil {
		return Signature{}, fmt.Errorf("bundle verification material: %w", err)
	}
	raw := subject.Bytes()
	artifact := verify.WithArtifactDigest("sha256", raw[:])
	var res *verify.VerificationResult
	var signer string
	switch {
	case vc.Certificate() != nil:
		if v.keyless == nil {
			return Signature{}, errors.New("a keyless signature, and the trust policy names no keyless identity")
		}
		res, err = v.keyless.Verify(&b, verify.NewPolicy(artifact, v.ids...))
		if err != nil {
			return Signature{}, fmt.Errorf("keyless signature: %w", err)
		}
		if c := res.Signature; c != nil && c.Certificate != nil {
			signer = c.Certificate.Issuer + " " + c.Certificate.SubjectAlternativeName
		}
	case vc.PublicKey() != nil:
		if len(v.keys) == 0 {
			return Signature{}, errors.New("a key-based signature, and the trust policy names no key")
		}
		var errs []error
		for _, k := range v.keys {
			r, kerr := k.v.Verify(&b, verify.NewPolicy(artifact, verify.WithKey()))
			if kerr == nil {
				res, signer = r, "key "+k.id
				break
			}
			errs = append(errs, kerr)
		}
		if res == nil {
			return Signature{}, fmt.Errorf("key-based signature matches no trusted key: %w", errors.Join(errs...))
		}
	default:
		return Signature{}, errors.New("the bundle holds neither a certificate nor a public key")
	}
	sig := Signature{Signer: signer}
	for _, ts := range res.VerifiedTimestamps {
		sig.Timestamps = append(sig.Timestamps, ts.Timestamp)
	}
	if st := res.Statement; st != nil {
		var views []subjectView
		for _, sd := range st.GetSubject() {
			view := subjectView{digest: sd.GetDigest(), annotations: map[string]any{}}
			for k, val := range sd.GetAnnotations().GetFields() {
				view.annotations[k] = val.AsInterface()
			}
			views = append(views, view)
		}
		ann, err := statementAnnotations(st.GetType(), st.GetPredicateType(), views, subject)
		if err != nil {
			return Signature{}, err
		}
		sig.Annotations = ann
	}
	return sig, nil
}

// subjectView is one statement subject: its digests and annotations.
type subjectView struct {
	digest      map[string]string
	annotations map[string]any
}

// statementAnnotations checks a verified statement: an in-toto v1
// statement of cosign's signing predicate type whose subjects name the
// manifest digest once, and returns that subject's string annotations. A
// statement of another predicate type, such as an attestation, is no
// signature of the artifact.
func statementAnnotations(typ, predicateType string, views []subjectView, subject revision.Digest) (map[string]string, error) {
	if typ != StatementType {
		return nil, fmt.Errorf("statement type %q, want %q", typ, StatementType)
	}
	if predicateType != PredicateCosignSign {
		return nil, fmt.Errorf("predicate type %q is not a signature's (%s)", predicateType, PredicateCosignSign)
	}
	want := subject.Hex()
	var match *subjectView
	for i := range views {
		if views[i].digest["sha256"] != want {
			continue
		}
		if match != nil {
			return nil, errors.New("the statement names the manifest digest twice")
		}
		match = &views[i]
	}
	if match == nil {
		return nil, errors.New("the statement does not name the manifest digest")
	}
	out := map[string]string{}
	for k, v := range match.annotations {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out, nil
}

// Result is the outcome of checking one manifest's signatures.
type Result struct {
	// Valid are the signatures that verified, in the order tried.
	Valid []Signature
	// Tried is how many referrers were tried.
	Tried int
	// Failures say why each other referrer tried failed: its digest and
	// the reason, in the order tried.
	Failures []string
}

// VerifyReferrers checks the signatures of the manifest subject in repo.
// It lists subject's referrers of artifact type MediaTypeSigstoreBundle in
// the registry's order and tries the first MaxSignatures: each referrer
// manifest, and the bundle it carries as a layer, at most
// MaxSignatureBytes and fetched by digest, all within SignatureTimeout on
// the repository's clock. It returns every signature that verified.
//
// A policy that trusts nothing, or no valid signature among those tried,
// none listed included, is RZ-CFG-033 matching ErrUnsigned: deterministic.
// A registry that cannot answer (ErrUnavailable, ErrDenied) or the time
// budget passing (ErrTimeout) is returned as is, never RZ-CFG-033, so the
// caller retries. Under ModeOff nothing is checked and the result is
// empty; ModeWarn checks as ModeEnforce does and leaves the decision to
// the caller.
func (v *SigstoreVerifier) VerifyReferrers(ctx context.Context, repo *Repository, subject Descriptor) (Result, error) {
	if v.mode == ModeOff {
		return Result{}, nil
	}
	if v.empty {
		return Result{}, errcode.Wrap(CodeSignature, fmt.Errorf("%w: the trust policy trusts no identity and no key", ErrUnsigned))
	}
	var res Result
	err := repo.within(ctx, SignatureTimeout, func(ctx context.Context) error {
		refs, err := repo.Referrers(ctx, subject, MediaTypeSigstoreBundle, MaxSignatures)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			res.Tried++
			sig, reason, err := v.tryReferrer(ctx, repo, subject, ref)
			if err != nil {
				return err
			}
			if reason != "" {
				res.Failures = append(res.Failures, ref.Digest.Short()+": "+reason)
				continue
			}
			res.Valid = append(res.Valid, sig)
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	if len(res.Valid) > 0 {
		return res, nil
	}
	if res.Tried == 0 {
		return res, errcode.Wrap(CodeSignature, fmt.Errorf("%w: manifest %s has no Sigstore bundle referrer", ErrUnsigned, subject.Digest.Short()))
	}
	return res, errcode.Wrap(CodeSignature, fmt.Errorf("%w: none of the %d Sigstore bundles of manifest %s verified: %s",
		ErrUnsigned, res.Tried, subject.Digest.Short(), strings.Join(res.Failures, "; ")))
}

// tryReferrer fetches one referrer and its bundle and verifies it. A
// referrer that does not verify returns a reason; a fetch that cannot
// complete returns an error, which ends the check.
func (v *SigstoreVerifier) tryReferrer(ctx context.Context, repo *Repository, subject, ref Descriptor) (Signature, string, error) {
	if ref.Size > MaxSignatureBytes {
		return Signature{}, fmt.Sprintf("the referrer manifest holds %d bytes, more than %d", ref.Size, MaxSignatureBytes), nil
	}
	data, err := repo.fetch(ctx, "manifests", ref, MediaTypeImageManifest, MaxSignatureBytes)
	if reason, ferr := fetchFailure(err); ferr != nil || reason != "" {
		return Signature{}, reason, ferr
	}
	layer, reason := bundleLayer(data, subject)
	if reason != "" {
		return Signature{}, reason, nil
	}
	if layer.Size > MaxSignatureBytes {
		return Signature{}, fmt.Sprintf("the bundle holds %d bytes, more than %d", layer.Size, MaxSignatureBytes), nil
	}
	b, err := repo.fetch(ctx, "blobs", layer, "", MaxSignatureBytes)
	if reason, ferr := fetchFailure(err); ferr != nil || reason != "" {
		return Signature{}, reason, ferr
	}
	sig, err := v.VerifyBundle(b, subject.Digest)
	if err != nil {
		return Signature{}, err.Error(), nil //nolint:nilerr // a bundle that does not verify is a reason this referrer fails, not an error of the check.
	}
	sig.Referrer = ref
	return sig, "", nil
}

// fetchFailure splits a fetch error into a reason the referrer fails for
// (content too large, a mismatch, a referrer that is gone) and an error
// that ends the check (the registry cannot answer, the time budget).
func fetchFailure(err error) (string, error) {
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, ErrTooLarge), errors.Is(err, ErrMismatch), errors.Is(err, ErrNotFound):
		return err.Error(), nil
	default:
		return "", err
	}
}

// bundleLayer reads a referrer manifest: an OCI image manifest of artifact
// type MediaTypeSigstoreBundle whose subject is subject and whose layers
// hold exactly one Sigstore bundle.
func bundleLayer(data []byte, subject Descriptor) (Descriptor, string) {
	var m referrerManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Descriptor{}, "the referrer is not an OCI image manifest"
	}
	if m.MediaType != MediaTypeImageManifest {
		return Descriptor{}, fmt.Sprintf("the referrer has media type %q, not an OCI image manifest", m.MediaType)
	}
	if m.ArtifactType != MediaTypeSigstoreBundle {
		return Descriptor{}, fmt.Sprintf("the referrer has artifact type %q, not a Sigstore bundle", m.ArtifactType)
	}
	if m.Subject == nil || m.Subject.Digest != subject.Digest.String() {
		return Descriptor{}, "the referrer's subject is another manifest"
	}
	i := slices.IndexFunc(m.Layers, func(l ociDescriptor) bool { return l.MediaType == MediaTypeSigstoreBundle })
	if i < 0 || slices.IndexFunc(m.Layers[i+1:], func(l ociDescriptor) bool { return l.MediaType == MediaTypeSigstoreBundle }) >= 0 {
		return Descriptor{}, "the referrer does not hold exactly one Sigstore bundle layer"
	}
	d, err := ParseDigest(m.Layers[i].Digest)
	if err != nil || m.Layers[i].Size < 0 {
		return Descriptor{}, "the bundle layer's descriptor has no sha256 digest"
	}
	return Descriptor{MediaType: MediaTypeSigstoreBundle, Digest: d, Size: m.Layers[i].Size}, ""
}
