// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// The signature format: cosign's, so a signature cosign writes verifies on
// a Node and one Ruralz writes verifies with cosign
// (docs/adr/0017-artifact-signing.md "Confirmation", interoperability).
// A signature of an OCI artifact is a Sigstore bundle whose DSSE envelope
// holds an in-toto statement with cosign's signing predicate type and an
// empty predicate; its one subject is the artifact's manifest digest,
// carrying the signed annotations. The bundle is an OCI referrer of that
// manifest: an image manifest of artifact type MediaTypeSigstoreBundle,
// the empty config, the bundle as its one layer.
const (
	// MediaTypeSigstoreBundle is the media type of a Sigstore bundle, and
	// the artifact type of the referrer that carries one.
	MediaTypeSigstoreBundle = "application/vnd.dev.sigstore.bundle.v0.3+json"
	// PredicateCosignSign is the predicate type of a signature statement.
	PredicateCosignSign = "https://sigstore.dev/cosign/sign/v1"
	// StatementType is the in-toto statement type.
	StatementType = "https://in-toto.io/Statement/v1"
	// PayloadTypeInToto is the DSSE payload type of an in-toto statement.
	PayloadTypeInToto = "application/vnd.in-toto+json"
)

// Annotations of a signature referrer, as cosign writes them.
const (
	annotationBundleContent   = "dev.sigstore.bundle.content"
	annotationBundlePredicate = "dev.sigstore.bundle.predicateType"
	annotationCreated         = "org.opencontainers.image.created"
	bundleContentDSSE         = "dsse-envelope"
)

// emptyConfig is the OCI empty descriptor's content.
const emptyConfig = "{}"

// statement is the JSON of a signature statement.
type statement struct {
	Type          string             `json:"_type"`
	Subject       []statementSubject `json:"subject"`
	PredicateType string             `json:"predicateType"`
	Predicate     struct{}           `json:"predicate"`
}

// statementSubject is the statement's subject.
type statementSubject struct {
	Digest      map[string]string `json:"digest"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// SignatureStatement returns the in-toto statement a signature of the
// manifest subject signs: cosign's signing statement whose one subject is
// subject with annotations, such as the Environment and sequence of an
// OCI Revision (RevisionAnnotations).
func SignatureStatement(subject revision.Digest, annotations map[string]string) ([]byte, error) {
	if subject.IsZero() {
		return nil, ErrNoDigest
	}
	s := statement{
		Type:          StatementType,
		Subject:       []statementSubject{{Digest: map[string]string{"sha256": subject.Hex()}, Annotations: maps.Clone(annotations)}},
		PredicateType: PredicateCosignSign,
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("signing: encode the statement: %w", err)
	}
	return b, nil
}

// referrerManifest is the JSON of the OCI image manifest that carries a
// Sigstore bundle as a referrer of its subject.
type referrerManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        ociDescriptor     `json:"config"`
	Layers        []ociDescriptor   `json:"layers"`
	Subject       *ociDescriptor    `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// ociDescriptor is the JSON of a descriptor.
type ociDescriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// jsonDescriptor converts d.
func jsonDescriptor(d Descriptor) ociDescriptor {
	return ociDescriptor{MediaType: d.MediaType, Digest: d.Digest.String(), Size: d.Size, ArtifactType: d.ArtifactType, Annotations: d.Annotations}
}

// PushSignature uploads bundle, a Sigstore bundle signing subject, as a
// referrer of subject in repo, in cosign's layout: the empty config, the
// bundle as the one layer, artifact type MediaTypeSigstoreBundle, and the
// annotations naming the bundle's content, its predicate type and created,
// the push time in RFC 3339 UTC. It returns the referrer's descriptor.
func PushSignature(ctx context.Context, repo *Repository, subject Descriptor, bundle []byte, created time.Time) (Descriptor, error) {
	if int64(len(bundle)) > MaxSignatureBytes {
		return Descriptor{}, fmt.Errorf("%w: a bundle of %d bytes, more than %d", ErrTooLarge, len(bundle), MaxSignatureBytes)
	}
	cfg, err := repo.PushBlob(ctx, MediaTypeEmpty, []byte(emptyConfig))
	if err != nil {
		return Descriptor{}, err
	}
	layer, err := repo.PushBlob(ctx, MediaTypeSigstoreBundle, bundle)
	if err != nil {
		return Descriptor{}, err
	}
	cfgDesc := jsonDescriptor(cfg)
	cfgDesc.ArtifactType = MediaTypeSigstoreBundle
	sub := jsonDescriptor(Descriptor{MediaType: subject.MediaType, Digest: subject.Digest, Size: subject.Size})
	m := referrerManifest{
		SchemaVersion: 2, MediaType: MediaTypeImageManifest, ArtifactType: MediaTypeSigstoreBundle,
		Config: cfgDesc, Layers: []ociDescriptor{jsonDescriptor(layer)}, Subject: &sub,
		Annotations: map[string]string{
			annotationBundleContent:   bundleContentDSSE,
			annotationBundlePredicate: PredicateCosignSign,
			annotationCreated:         created.UTC().Format(time.RFC3339),
		},
	}
	content, err := json.Marshal(m)
	if err != nil {
		return Descriptor{}, fmt.Errorf("signing: encode the referrer manifest: %w", err)
	}
	return repo.PushManifest(ctx, MediaTypeImageManifest, content, "")
}
