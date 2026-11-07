// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package signing holds the integrity and origin checks of Revisions and
// Plugin artifacts (docs/adr/0017-artifact-signing.md;
// docs/architecture/08-security-and-identity.md "Signed Revisions and
// Plugin artifacts"): digest checks, OCI registry access with registry
// credentials, and Sigstore signatures, verified offline under a trust
// policy.
//
// # Digests
//
// revision.Digest is the only digest type: an expected digest is always
// the full sha256:<64 lowercase hex>, never the display form rev-<12 hex>.
// Content that does not hash to its digest is RZ-CFG-027 for Revisions
// and RZ-CFG-028 for Plugin artifacts (VerifyRevision, VerifyArtifact,
// ReadArtifact). The checks are always on and not configurable. Verifier
// is the content check: DigestOnly, or the digest check followed by a
// check of a signature over the content (WithSignature).
//
// # Registries
//
// Repository is one OCI repository, reached over TLS 1.2 or newer with the
// server certificate verified against the system roots or RootCAs; there
// is no cleartext registry connection. It resolves tags (Resolve), fetches
// manifests and blobs by digest within a size bound, hashing them as they
// stream (FetchManifest, FetchBlob: ErrMismatch, ErrTooLarge), lists
// referrers through the referrers API or, without one, the referrers tag
// schema, and tags, both with bounded pages (Referrers, Tags), and pushes
// (PushBlob, PushManifest, PushSignature). Errors are ErrNotFound,
// ErrDenied (missing or refused credentials) or ErrUnavailable (network,
// TLS, 5xx or 429 answers): the first two say what is wrong, the third
// that a later attempt may pass.
//
// Credentials resolve registry credentials from one file in the container
// credential file format: RURALZ_REGISTRY_AUTH_FILE on a Node, the
// standard file (config.json under DOCKER_CONFIG, else ~/.docker/) or the
// --registry-auth-file of the CLI. auths entries, credHelpers and
// credsStore are read as the container tools read them, a helper being
// the docker-credential-<name> executable on PATH. A credential is sent
// only to the registry host it is keyed by: lookups are by exact host and
// port, and the client drops Authorization once a redirect leaves the
// registry's origin, as to a blob store, follows at most 10 redirects and
// none from https to http.
//
// # Signatures
//
// A signature of an OCI artifact is in cosign's format, so cosign and
// Ruralz read each other's: a Sigstore bundle whose DSSE envelope holds an
// in-toto statement of predicate type https://sigstore.dev/cosign/sign/v1
// (SignatureStatement), its subject the artifact's manifest digest with
// the signed annotations, stored as a referrer of that manifest of
// artifact type application/vnd.dev.sigstore.bundle.v0.3+json, the bundle
// its one layer. A message signature over the manifest digest also
// verifies; it signs no annotation.
//
// SigstoreVerifier verifies under a Policy: mode enforce, warn or off;
// trusted keyless identities, each an exact issuer and subject or,
// opted into, RE2 patterns matching the whole value; trusted keys; the
// Sigstore trusted root, read locally and never fetched over TUF. A
// keyless bundle verifies with a transparency-log inclusion proof that the
// log's key verifies and at least one trusted timestamp, the log's signed
// entry timestamp or a timestamp authority's, at which its certificate
// chains to a trusted certificate authority and names a trusted identity;
// a key-based bundle verifies against a trusted key, with no timestamp.
// VerifyReferrers lists only Sigstore bundle referrers and tries at most
// 16, each referrer manifest and bundle at most 1 MiB, within 10 s
// (target). No valid signature, or a policy that trusts nothing, is
// RZ-CFG-033 (ErrUnsigned); a registry that cannot answer, or the time
// budget passing (ErrTimeout), is transient and carries no RZ code.
//
// An OCI Revision's signature binds its Environment and a monotonic
// sequence in the signed annotations ruralz.io/environment and
// ruralz.io/sequence (RevisionAnnotations). BindRevision takes, among the
// valid signatures, the one naming the Node's Environment with the
// highest sequence, and refuses with RZ-CFG-033 another Environment's
// Revision (ErrEnvironment) and a sequence below the Node's high-water
// mark (ErrSequence).
package signing
