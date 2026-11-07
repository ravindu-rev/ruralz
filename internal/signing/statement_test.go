// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/testkit/ociregistry"
)

// Tests for the signature format, cosign's: a signing statement whose one
// subject is the manifest digest with the signed annotations, carried as
// an OCI referrer of the manifest.

func TestSignatureStatement(t *testing.T) {
	d := revision.Sum([]byte("manifest"))
	b, err := SignatureStatement(d, RevisionAnnotations("prod", 12))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type    string `json:"_type"`
		Subject []struct {
			Digest      map[string]string `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"subject"`
		PredicateType string         `json:"predicateType"`
		Predicate     map[string]any `json:"predicate"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "https://in-toto.io/Statement/v1" || got.PredicateType != "https://sigstore.dev/cosign/sign/v1" || got.Predicate == nil || len(got.Predicate) != 0 {
		t.Fatalf("statement = %s", b)
	}
	if len(got.Subject) != 1 || got.Subject[0].Digest["sha256"] != d.Hex() || len(got.Subject[0].Digest) != 1 {
		t.Fatalf("subject = %+v", got.Subject)
	}
	if a := got.Subject[0].Annotations; a["ruralz.io/environment"] != "prod" || a["ruralz.io/sequence"] != "12" {
		t.Fatalf("annotations = %v", a)
	}
	if _, err := SignatureStatement(revision.Digest{}, nil); !errors.Is(err, ErrNoDigest) {
		t.Fatalf("zero digest: %v", err)
	}
	noAnn, err := SignatureStatement(d, nil)
	if err != nil || strings.Contains(string(noAnn), "annotations") {
		t.Fatalf("statement without annotations = %s, %v", noAnn, err)
	}
}

func TestPushSignatureLayout(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{})
	r := newRepo(t, s, nil)
	ctx := t.Context()
	md, _ := artifact(ctx, t, r, "prod", []byte("revision"))
	bundle := []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)
	ref, err := PushSignature(ctx, r, md, bundle, created)
	if err != nil {
		t.Fatal(err)
	}
	refs := s.Referrers("acme/config", md.Digest.String())
	if len(refs) != 1 || refs[0].Digest != ref.Digest.String() || refs[0].ArtifactType != MediaTypeSigstoreBundle {
		t.Fatalf("referrers = %+v", refs)
	}
	raw, ok := s.Manifest("acme/config", ref.Digest.String())
	if !ok {
		t.Fatal("the referrer manifest is not stored")
	}
	var m referrerManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != 2 || m.MediaType != MediaTypeImageManifest || m.ArtifactType != MediaTypeSigstoreBundle {
		t.Fatalf("manifest = %s", raw)
	}
	if m.Config.MediaType != MediaTypeEmpty || m.Config.Digest != revision.Sum([]byte("{}")).String() || m.Config.Size != 2 {
		t.Fatalf("config = %+v", m.Config)
	}
	if len(m.Layers) != 1 || m.Layers[0].MediaType != MediaTypeSigstoreBundle || m.Layers[0].Digest != revision.Sum(bundle).String() {
		t.Fatalf("layers = %+v", m.Layers)
	}
	if m.Subject == nil || m.Subject.Digest != md.Digest.String() || m.Subject.Size != md.Size || m.Subject.MediaType != MediaTypeImageManifest {
		t.Fatalf("subject = %+v", m.Subject)
	}
	want := map[string]string{
		"dev.sigstore.bundle.content":       "dsse-envelope",
		"dev.sigstore.bundle.predicateType": "https://sigstore.dev/cosign/sign/v1",
		"org.opencontainers.image.created":  "2026-11-07T12:00:00Z",
	}
	for k, v := range want {
		if m.Annotations[k] != v {
			t.Errorf("annotation %s = %q, want %q", k, m.Annotations[k], v)
		}
	}
	if _, err := PushSignature(ctx, r, md, make([]byte, MaxSignatureBytes+1), created); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized bundle: %v", err)
	}
}
