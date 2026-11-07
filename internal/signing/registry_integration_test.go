// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package signing

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/signing/signingtest"
	"github.com/ravindu-rev/ruralz/internal/testkit/ociregistry"
)

// The OCI registry integration tests (docs/engineering/03-testing-and-quality-strategy.md
// "Integration tests"): the registry image through testcontainers-go,
// over TLS with htpasswd credentials, and with no referrers API, so every
// referrer goes through the referrers tag schema. They cover pushes, tag
// polling, pulls by digest, referrers, signature verification failures,
// a Revision signed for another Environment or at an older sequence, and
// missing or wrong credentials.

// containerRepo returns a Repository of repo on c with creds.
func containerRepo(t *testing.T, c *ociregistry.Container, repo string, creds *Credentials) *Repository {
	t.Helper()
	r, err := NewRepository(Reference{Registry: c.Host(), Repository: repo}, RegistryOptions{Credentials: creds, RootCAs: c.RootCAs()})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRegistryContainer(t *testing.T) {
	c := ociregistry.StartContainer(t)
	ctx := t.Context()
	creds := credsFor(t, c.Host(), Credential{Username: ociregistry.ContainerUser, Password: ociregistry.ContainerPassword})
	r := containerRepo(t, c, "acme/gateway-config", creds)

	t.Run("credentials", func(t *testing.T) {
		if _, err := containerRepo(t, c, "acme/gateway-config", nil).Resolve(ctx, "prod"); !errors.Is(err, ErrDenied) {
			t.Fatalf("missing credentials: %v, want ErrDenied", err)
		}
		wrong := credsFor(t, c.Host(), Credential{Username: ociregistry.ContainerUser, Password: "wrong"})
		if _, err := containerRepo(t, c, "acme/gateway-config", wrong).Resolve(ctx, "prod"); !errors.Is(err, ErrDenied) {
			t.Fatalf("wrong credentials: %v, want ErrDenied", err)
		}
		other := credsFor(t, "registry.example.com", Credential{Username: ociregistry.ContainerUser, Password: ociregistry.ContainerPassword})
		if _, err := containerRepo(t, c, "acme/gateway-config", other).Resolve(ctx, "prod"); !errors.Is(err, ErrDenied) {
			t.Fatalf("another host's credentials: %v, want ErrDenied", err)
		}
	})

	// Push the first artifact and poll its tag.
	first, firstLayer := artifact(ctx, t, r, "prod", []byte(`{"revision":1}`))
	got, err := r.Resolve(ctx, "prod")
	if err != nil || got.Digest != first.Digest {
		t.Fatalf("Resolve after the first push = %+v, %v", got, err)
	}
	t.Run("pull by digest", func(t *testing.T) {
		m, err := r.FetchManifest(ctx, got)
		if err != nil || len(m) != int(first.Size) {
			t.Fatalf("FetchManifest = %d bytes, %v", len(m), err)
		}
		b, err := r.FetchBlob(ctx, firstLayer, MaxArtifactBytes)
		if err != nil || string(b) != `{"revision":1}` {
			t.Fatalf("FetchBlob = %q, %v", b, err)
		}
		if _, err := r.FetchBlob(ctx, firstLayer, firstLayer.Size-1); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("over the limit: %v", err)
		}
		if _, err := r.Resolve(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a missing tag: %v", err)
		}
	})

	// A second push moves the tag; the next poll sees the new digest.
	second, _ := artifact(ctx, t, r, "prod", []byte(`{"revision":2}`))
	if got, err := r.Resolve(ctx, "prod"); err != nil || got.Digest != second.Digest || got.Digest == first.Digest {
		t.Fatalf("Resolve after the second push = %+v, %v", got, err)
	}
	tags, err := r.Tags(ctx, 100)
	if err != nil || !containsTag(tags, "prod") {
		t.Fatalf("Tags = %v, %v", tags, err)
	}

	ss := newSigstore(t)
	v := keylessVerifier(t, ss, Identity{Issuer: testIssuer, Subject: testIdentity})
	t.Run("unsigned", func(t *testing.T) {
		_, err := v.VerifyReferrers(ctx, r, first)
		if !errors.Is(err, ErrUnsigned) {
			t.Fatalf("an unsigned manifest: %v", err)
		}
		if code, _ := errcode.CodeOf(err); code != "RZ-CFG-033" {
			t.Fatalf("code %q", code)
		}
	})

	sign := func(t *testing.T, d Descriptor, identity, env string, seq uint64) {
		t.Helper()
		b := ss.keyless(t, identity, testIssuer, mustStatement(t, d.Digest, RevisionAnnotations(env, seq)), keylessOptions{})
		if _, err := PushSignature(ctx, r, d, b, created); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("untrusted identity", func(t *testing.T) {
		sign(t, first, "intruder@example.com", "prod", 1)
		res, err := v.VerifyReferrers(ctx, r, first)
		if !errors.Is(err, ErrUnsigned) || res.Tried != 1 {
			t.Fatalf("a signature by an untrusted identity: %+v, %v", res, err)
		}
	})
	sign(t, first, testIdentity, "prod", 1)
	sign(t, second, testIdentity, "prod", 2)

	t.Run("referrers and verification", func(t *testing.T) {
		refs, err := r.Referrers(ctx, first, MediaTypeSigstoreBundle, MaxSignatures)
		if err != nil || len(refs) != 2 {
			t.Fatalf("Referrers of the first manifest = %d, %v", len(refs), err)
		}
		res, err := v.VerifyReferrers(ctx, r, second)
		if err != nil || len(res.Valid) != 1 {
			t.Fatalf("VerifyReferrers = %+v, %v", res, err)
		}
		b, err := BindRevision(res.Valid, "prod", 1)
		if err != nil || b.Sequence != 2 || b.Signer != testIssuer+" "+testIdentity {
			t.Fatalf("BindRevision = %+v, %v", b, err)
		}
	})
	t.Run("another Environment", func(t *testing.T) {
		staging, _ := artifact(ctx, t, r, "staging", []byte(`{"revision":"staging"}`))
		sign(t, staging, testIdentity, "staging", 5)
		res, err := v.VerifyReferrers(ctx, r, staging)
		if err != nil {
			t.Fatal(err)
		}
		_, err = BindRevision(res.Valid, "prod", 2)
		if !errors.Is(err, ErrEnvironment) || !strings.Contains(err.Error(), `"staging"`) {
			t.Fatalf("a staging Revision on a prod Node: %v", err)
		}
		if code, _ := errcode.CodeOf(err); code != "RZ-CFG-033" {
			t.Fatalf("code %q", code)
		}
	})
	t.Run("older sequence", func(t *testing.T) {
		// A retag back to the first Revision replays sequence 1 to a Node
		// whose high-water mark is 2.
		res, err := v.VerifyReferrers(ctx, r, first)
		if err != nil {
			t.Fatal(err)
		}
		_, err = BindRevision(res.Valid, "prod", 2)
		if !errors.Is(err, ErrSequence) {
			t.Fatalf("an older sequence: %v", err)
		}
		if code, _ := errcode.CodeOf(err); code != "RZ-CFG-033" {
			t.Fatalf("code %q", code)
		}
		// Signed again at a new sequence, as a rollback push does, it
		// passes the mark.
		sign(t, first, testIdentity, "prod", 3)
		res, err = v.VerifyReferrers(ctx, r, first)
		if err != nil {
			t.Fatal(err)
		}
		if b, err := BindRevision(res.Valid, "prod", 2); err != nil || b.Sequence != 3 {
			t.Fatalf("the rollback = %+v, %v", b, err)
		}
	})
	t.Run("key-based signature", func(t *testing.T) {
		k := signingtest.NewKey(t)
		third, _ := artifact(ctx, t, r, "keyed", []byte(`{"revision":3}`))
		if _, err := PushSignature(ctx, r, third, k.Sign(t, mustStatement(t, third.Digest, RevisionAnnotations("prod", 4)), nil), created); err != nil {
			t.Fatal(err)
		}
		res, err := trustKeys(t, k).VerifyReferrers(ctx, r, third)
		if err != nil || len(res.Valid) != 1 {
			t.Fatalf("a key-based signature: %+v, %v", res, err)
		}
		if _, err := trustKeys(t, signingtest.NewKey(t)).VerifyReferrers(ctx, r, third); !errors.Is(err, ErrUnsigned) {
			t.Fatalf("another key: %v", err)
		}
	})
}

// containsTag reports whether tags holds tag.
func containsTag(tags []string, tag string) bool {
	for _, x := range tags {
		if x == tag {
			return true
		}
	}
	return false
}

func TestRegistryContainerReferrerBounds(t *testing.T) {
	c := ociregistry.StartContainer(t)
	ctx := t.Context()
	creds := credsFor(t, c.Host(), Credential{Username: ociregistry.ContainerUser, Password: ociregistry.ContainerPassword})
	r := containerRepo(t, c, "acme/bounds", creds)
	md, _ := artifact(ctx, t, r, "prod", []byte("bounded"))
	k, other := signingtest.NewKey(t), signingtest.NewKey(t)
	for i := range MaxSignatures {
		b := other.Sign(t, mustStatement(t, md.Digest, map[string]string{"n": fmt.Sprint(i)}), nil)
		if _, err := PushSignature(ctx, r, md, b, created); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := PushSignature(ctx, r, md, k.Sign(t, mustStatement(t, md.Digest, nil), nil), created); err != nil {
		t.Fatal(err)
	}
	refs, err := r.Referrers(ctx, md, MediaTypeSigstoreBundle, 64)
	if err != nil || len(refs) != MaxSignatures+1 {
		t.Fatalf("Referrers = %d, %v", len(refs), err)
	}
	// The referrers tag schema lists referrers in push order, so the valid
	// signature, pushed 17th, is past the 16 a verifier tries.
	res, err := trustKeys(t, k).VerifyReferrers(ctx, r, md)
	if res.Tried != MaxSignatures || !errors.Is(err, ErrUnsigned) {
		t.Fatalf("VerifyReferrers = %+v, %v, want %d tried and RZ-CFG-033", res, err, MaxSignatures)
	}
}
