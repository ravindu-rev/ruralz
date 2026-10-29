// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package mtls is the auth.mtls Policy type
// (docs/architecture/08-security-and-identity.md "Mutual TLS",
// "Authentication", "Basic and mTLS schemas"): in onRequestHeaders it
// verifies the client certificate chain of the connection against the
// Policy's own CAs, checks it against the Policy's CRLs and subject rules,
// binds a Consumer and sets the request identity. It is an auth-class
// Filter, failureMode closed only, and never calls the State Store.
//
// # Connection
//
// A listener requests an unverified client certificate when one of its
// Routes attaches auth.mtls (internal/tlsconf); the Filter does the
// verification. A Route reached over a connection whose handshake
// requested no certificate (a coalesced HTTP/2 connection, one accepted
// before a Hot Reload changed the mode, or a cleartext one) answers 421
// RZ-AUTH-008, so the client retries on a connection that asks; a
// connection that requested one and got none answers 401 RZ-AUTH-001.
//
// # Verification
//
// The leaf MUST list the clientAuth extended key usage. The chain is
// built from the certificates the client sent and verified against the
// certificates of config.caCertificate as trust anchors, at the request
// time, for clientAuth. A failure is 401 RZ-AUTH-002. config.crl holds
// PEM CRLs for those CAs: each must be signed by a certificate of
// config.caCertificate, and a serial a CA's CRLs list revokes the
// certificate that CA issued, leaf or intermediate (401 RZ-AUTH-004 when
// every verified chain holds a revoked certificate). identity.Revoker is
// consulted for the leaf on every request (M2's revocation list; M1 wires
// identity.NopRevoker). config.subjects rules match the leaf's RFC 4514
// subject (Subject) or one of its URI SANs exactly; with rules, a leaf
// matching none is 401 RZ-AUTH-002.
//
// The verdict is cached per connection, Policy and snapshot in the
// connection's filter.ConnCache, keyed by the Filter, which Build makes
// anew for every snapshot, and tied to the trust state it was reached
// under: a CA removed by a swap or by a rotation stops matching at once.
// An accepted verdict holds while the chain is valid, so a certificate
// that expires during a long-lived connection is refused from then on; a
// chain refused because a certificate was not valid yet is verified again
// once it is.
//
// # Identity
//
// An accepted leaf binds a Consumer through the snapshot's credential
// index: credentials.certificates entries by subject or URI SAN, exactly.
// Two Consumers are 401 RZ-AUTH-002; none leaves the identity unbound with
// the principal mtls:<subject>. The identity carries the subject, which
// source.clientCertSubject shows, and empty claims. A second successful
// authentication of the request is 401 RZ-AUTH-002 (Authentication rule
// 2). Rejections carry no WWW-Authenticate challenge (a documented
// deviation) and never echo certificate content.
//
// # Rotation and CRL freshness
//
// config.caCertificate and config.crl rotate by watch like listener
// certificates ("Certificate rotation"): a value that does not parse, or a
// CRL no certificate of the bundle signed, keeps the last trust state and
// counts a rotation failure. The two may change in either order. Once a
// CRL is past its nextUpdate it stays enforced and the Node reports the
// degraded state crl_stale (docs/architecture/10-observability.md
// "Degraded states") until fresh CRLs arrive or the Policy goes away. The
// Filters of one Policy share these watches across snapshots.
package mtls
