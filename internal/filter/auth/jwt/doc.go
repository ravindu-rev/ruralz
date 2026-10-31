// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package jwt is the auth.jwt Policy type
// (docs/architecture/08-security-and-identity.md "Authentication", "JWT
// and OIDC"): in onRequestHeaders it verifies the JWS compact token of
// Authorization: Bearer with the issuer's keys from the Node's JWKS
// manager (package jwks), checks its registered claims, binds a Consumer
// and sets the request identity. It is an auth-class Filter, failureMode
// closed only, and never calls the State Store.
//
// # Token
//
// M1 fixes these rules instead of adding fields: the token is at most
// MaxTokenBytes; its alg is RS256, PS256, ES256 or EdDSA (HS256 and every
// other alg, and a crit header, are RZ-AUTH-002); jku, x5u, jwk and x5c
// are ignored, so keys come only from the jwksUrl of the issuer the
// token's iss selects. The header and payload are strict JSON decoded by
// internal/jsonval (valid UTF-8, no duplicate member names). The
// signature is verified with the jws package of lestrrat-go/jwx against
// the key of the token's kid and algorithm family; a JWK that names an
// alg verifies only tokens of that alg. A token without a kid tries every
// key of its family in the issuer's set, in document order, and the first
// that verifies wins: at most jwks.MaxKeys verifications per token. exp is
// required and at most MaxExpiry ahead, exp and nbf get ClockSkew, aud
// must hold one of the issuer's audiences, and iat is not checked.
//
// # Decisions
//
// Filter.Handle lists the RZ-AUTH code of every outcome ("RZ-AUTH
// decision codes"). An issuer without usable keys is CannotDecide with
// RZ-AUTH-006; every other rejection is a deny. An unknown kid (for a
// token without a kid, no key of its family), and an issuer without usable
// keys, nudge the JWKS manager to refetch, at most once per issuer per
// jwks.RefreshInterval. Rejections carry the challenge Bearer
// realm="ruralz" and never echo the token or a claim.
//
// # Identity
//
// A verified token is checked against identity.Revoker (RZ-AUTH-004),
// then bound through the snapshot's credential index: Consumers by issuer
// plus subject or claims, and OAuth clients by client_id, else azp, when
// the issuer is in the Consumer's credentials.jwt ("Consumers and
// tiers"). Two Consumers are RZ-AUTH-002; none leaves the identity
// unbound with the principal jwt:<iss>#<sub>. The identity carries the
// verified claims as auth.claims. The Authorization field is forwarded
// upstream unchanged ("Request hardening"); an Upstream headers Policy
// forwards claims.
//
// # Configuration
//
// Build rejects a config with no issuers, or an issuer that is empty,
// named twice or without audiences, with RZ-CFG-005, and a jwksUrl that is
// not https with RZ-CFG-037: the first finding of package check, whose
// Check, the offline hub.PolicyCheck every binary runs, reports every such
// finding from the same rule set. The check lives in its own package so
// that the binaries that link it never link jwx. Build
// prefetches the issuers that have no usable keys, waiting at most until
// the compile's prefetch round ends, and activates whatever the fetches
// returned. ruralz_auth_jwks_age_seconds reports the age of the Policy's
// oldest key set.
package jwt
