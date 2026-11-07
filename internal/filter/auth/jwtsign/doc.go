// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package jwtsign is the auth.jwt-sign Policy type
// (docs/architecture/08-security-and-identity.md "JWT signing"): in
// onResponse, at Route scope, it signs JWTs into the buffered Upstream
// response with a key the Node holds. It is an auth-class Filter, slot its
// name, failureMode closed only; it authenticates no client, so Security
// rule 1 takes no account of it, and it never calls the State Store.
//
// # Modes
//
// A config holds exactly one mode. With config.paths, each JSON object a
// body dot path matches is replaced by a compact JWS whose claims are that
// object, as the body holds it in the transform form (members in their
// order, numbers verbatim); the paths apply in order, each over the
// document the one before it wrote. The dot paths are those of the
// transform Policies (package dotpath): over a missing path, a member name
// on an array, an index past an array's end and a missing or empty array
// for "*" nothing is signed, while a path through a scalar, "*" on an
// object and a match that is not an object are path errors. With
// config.claims, a CEL expression over response (with its body), consumer
// and auth (expr.PlaceJWTSignClaims) builds the claims of one token, a
// map written as JSON in the transform body form of the CEL module, and
// the token is written as a JSON string at config.target, creating
// missing objects as a transform set entry with target body does.
//
// # Tokens
//
// A token is the JWS compact serialization (RFC 7515): the JOSE header
// {"alg":<config.algorithm>} with "kid":<config.keyId> when keyId is set,
// the claims, and the signature, each base64url without padding. RS256 is
// RSASSA-PKCS1-v1_5 and PS256 RSASSA-PSS (MGF1, salt as long as the hash),
// both with SHA-256 and an RSA key; ES256 is ECDSA on P-256 with SHA-256,
// R and S as 32 bytes each; EdDSA is Ed25519 (RFC 8037). The claims must
// hold an exp NumericDate at most MaxExpiry ahead of the request's start;
// an exp in the past is signed as it is, and no claim is added.
//
// # Key
//
// config.signingKey resolves through secretRef only and binds as local: it
// never leaves the Node. Its value is a private JWK (kty RSA, EC on P-256
// or OKP on Ed25519, with d), whose use, when present, is "sig", whose
// key_ops, when present, hold "sign" and whose alg, when present, is
// config.algorithm; or an unencrypted PEM private key (PKCS #8, PKCS #1 or
// SEC 1), text outside its blocks ignored and at most one private key per
// value. An RSA key has at least MinRSABits bits and must sign
// consistently; a JWK's public members must be those of its private key;
// a public key is refused. The key must fit config.algorithm: RSA for
// RS256 and PS256, P-256 for ES256, Ed25519 for EdDSA. A value Build
// refuses, or a reference missing from the resolved table, is RZ-CFG-026.
// A Filter watches the reference: a rotated value it refuses keeps the
// last key, and the resolver counts the rotation failure and raises
// secret_rotation_failed. Filters of later snapshots with the same
// reference and algorithm share the holder (BuildEnv.Previous).
//
// # Body
//
// The Policy gates the response body (the snapshot compiler gates it for
// every auth.jwt-sign attachment) and never runs on a generated response
// (the executor skips it there). On a response that cannot have content,
// to a HEAD request or with status 1xx, 204 or 304, and on a 206 or a
// response with Content-Range, it signs nothing. A body is JSON when the
// response has exactly one Content-Type field line naming JSON and the
// bytes are strict JSON (dotpath.Editor.Normalize). With config.paths an
// empty body passes unchanged; with config.claims an empty body starts
// from an empty object and takes Content-Type application/json. A
// rewritten body is the response's own copy (filter.Message.SetBody
// recomputes Content-Length and drops Content-Encoding and the body
// digests); each strong ETag turns weak and Accept-Ranges is removed
// (transform.MarkRewritten). A body no path matched keeps its bytes.
//
// # Configuration
//
// Each finding below is RZ-CFG-005, in Build and in Check, the offline
// check every binary runs: a config.paths entry or config.target that is
// not a dot path (dotpath.Parse), config.target beside config.paths, and
// config.claims without config.target. Build also refuses what the schema
// states, which Check leaves to it: both or neither of paths and claims,
// and an algorithm outside RS256, PS256, ES256 and EdDSA. Build compiles
// config.claims at its site through BuildEnv.CEL: RZ-CFG-014 or
// RZ-CFG-015, as stage J reports them.
//
// # Failures
//
// A CEL runtime error (cel_error, or result_type for a result that is not
// a map or has no JSON form), claims without exp or with one past
// MaxExpiry (invalid_value), a path error (path_error), a body that is not
// JSON but not empty (null_body), a body over limits.maxResponseBodyBytes
// (output_cap) and a signature that cannot be computed (internal) cannot
// decide: the Policy is closed only, so the executor replaces the response
// with 502 RZ-RT-012 and no unsigned claims reach the client. A spent
// buffer budget stays 503 RZ-RT-004 (filter.ErrBudget), and a body past
// its decoded limit filter.ErrTooLarge.
package jwtsign
