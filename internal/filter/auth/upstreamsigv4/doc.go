// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package upstreamsigv4 is the auth.upstream-sigv4 Policy type
// (docs/architecture/08-security-and-identity.md "Upstream
// authentication"): it signs the requests of an Upstream's legs with AWS
// Signature Version 4, through the aws/signer/v4 package of
// github.com/aws/aws-sdk-go-v2, which depguard confines to this package
// (docs/engineering/01-tech-stack-and-libraries.md "Library catalog"). It
// is Filter class upstream-auth, Upstream scope only, Phase
// onUpstreamRequest, failureMode closed only.
//
// # Signing at the transport-time hook
//
// The type records its signing parameters in onUpstreamRequest and signs
// at the transport-time signing hook (foundation pack section 8.12 rule 6;
// filter.Signer): Handle obtains the credentials of the attempt and keeps
// them in the Policy's per-leg state, and Sign, which the Node calls after
// the attempt's last onUpstreamRequest Filter, as the last step before the
// request is sent, signs the request as it will be sent. Every header and
// body transform of the attempt is therefore covered, and every attempt of
// a leg, retries included, is signed anew at the clock's time of the hook,
// so a retry never resends an older signature.
//
// Signing replaces the fields the signature owns, Authorization,
// X-Amz-Date, X-Amz-Security-Token and X-Amz-Content-Sha256, whatever
// value the request carried for them. It first merges the lines of a
// header field the request holds under names that differ only in letter
// case under the canonical name, so the field the transport writes is the
// field signed. X-Amz-Content-Sha256 carries the payload hash on every
// request: with config.payload signed (the default) the hex SHA-256 of the
// body the attempt sends, read through the request's GetBody, which a
// gated body and a rewritten one provide (the snapshot compiler gates the
// request body of every Route whose legs sign their payload, within
// limits.maxRequestBodyBytes); with unsigned, UNSIGNED-PAYLOAD, which only
// services that accept it take, and the body may stream. Amazon S3 needs
// the field on every request and an unsigned payload is declared by it.
// Then the request is signed with the attempt's credentials, a session
// token among the signed fields, for config.region and config.service:
// every header field but Authorization, User-Agent, X-Amzn-Trace-Id,
// Expect and Transfer-Encoding is signed, the host without a default port
// (which the request then carries), the query in its canonical form (the
// request then carries that form: names and values encoded and sorted),
// and the path as the service checks it. The path is first given the form
// AWS SDKs send and AWS URI-encodes: each segment decoded, then every byte
// but the unreserved characters percent-encoded, so the sub-delims, ":"
// and "@" that the normalized path keeps unescaped are escaped and an
// escaped slash stays one; a service then computes the same canonical path
// whether it decodes the path first or not. For every service but Amazon
// S3 (config.service s3) the canonical path is that path with its dot
// segments resolved and its empty segments dropped, encoded once more; S3
// checks the path exactly as sent. The request goes out with that path,
// dot and empty segments included.
//
// # Credentials
//
// The Policy signs with the credentials of the Node's credential source,
// Factory.Credentials. Which source a Node has, static keys through
// secretRef or the workload's own keyless identity, is
// OQ-security-and-identity-45 (docs/architecture/08-security-and-identity.md
// "Open questions"), so the Policy type registry does not serve the type
// yet (RZ-CFG-040) and internal/filter/builtin registers no Factory for it.
//
// # Failures
//
// Credentials that cannot be obtained, an attempt without an outgoing
// request, a body that cannot be read for its hash (a streamed body under
// a signed payload) and a Sign without the attempt's credentials are
// CannotDecide with RZ-AUTH-020, which the closed-only class answers with
// 401 before any byte reaches the Upstream. Credentials never reach a log,
// an error, a metric or a span: Credentials holds its secrets as
// secret.Value, and errors carry fixed text.
package upstreamsigv4
