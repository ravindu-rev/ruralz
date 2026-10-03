// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package auth holds what the auth- and authz-class Filters share (M1 spec
// 06 section 2.1; docs/architecture/08-security-and-identity.md
// "Authentication"): the WWW-Authenticate challenges and Retry-After of
// their rejections, decision recording into ruralz_auth_decisions_total,
// Security rule 2 (a second successful authentication is 401 RZ-AUTH-002),
// the request identity they set, single-field-line credential reads and
// credential stripping, and the per-snapshot credential index they bind
// Consumers with. The type packages live below it (jwt, apikey, basic,
// mtls, upstreamoauth2).
//
// Nothing here logs, traces or labels a credential, token, key, username,
// subject or claim (spec 06 requirement 11): rejections carry only an RZ
// code, and problem documents never echo credential material.
package auth

import (
	"strings"
)

// RZ-AUTH codes the auth and authz Filters return (Security and identity,
// "RZ-AUTH decision codes").
const (
	// CodeMissing: credential missing, or every auth-class Policy skipped.
	CodeMissing = "RZ-AUTH-001"
	// CodeInvalid: credential invalid, no matching Consumer, or a second
	// binding.
	CodeInvalid = "RZ-AUTH-002"
	// CodeClaims: token expired, not yet valid, without or over-long exp,
	// or wrong issuer or audience.
	CodeClaims = "RZ-AUTH-003"
	// CodeRevoked: credential revoked.
	CodeRevoked = "RZ-AUTH-004"
	// CodeUnknownKID: unknown kid.
	CodeUnknownKID = "RZ-AUTH-005"
	// CodeNoKeys: no usable keys for the issuer.
	CodeNoKeys = "RZ-AUTH-006"
	// CodeThrottled: authentication throttled (429 with Retry-After).
	CodeThrottled = "RZ-AUTH-007"
	// CodeNoCertRequest: an auth.mtls Route reached over a connection that
	// requested no client certificate (421).
	CodeNoCertRequest = "RZ-AUTH-008"
	// CodeAuthzUndecided: authorization could not decide (the authz class
	// default under closed).
	CodeAuthzUndecided = "RZ-AUTH-015"
)

// Challenges of the auth types (spec 06 requirement 8; RFC 9110 section
// 11.6.1). auth.mtls answers without one (documented deviation).
const (
	// ChallengeJWT is auth.jwt's challenge.
	ChallengeJWT = `Bearer realm="ruralz"`
	// ChallengeBasic is auth.basic's challenge.
	ChallengeBasic = `Basic realm="ruralz", charset="UTF-8"`
)

// DefaultAPIKeyHeader is the header auth.api-key reads when config.header
// is absent (spec 06 requirement 15; the schema default).
const DefaultAPIKeyHeader = "x-api-key"

// Response field names this package sets.
const (
	headerWWWAuthenticate = "Www-Authenticate"
	headerRetryAfter      = "Retry-After"
)

// Challenger is implemented by auth-class Filters: Challenge returns the
// WWW-Authenticate value their 401s carry, "" for none. The Filter Chain
// executor asserts a structurally identical interface to give the 401
// RZ-AUTH-001 of Security rule 1 the challenge of every auth-class Policy
// in the chain (spec 06 requirements 2 and 8). A Filter that embeds a
// *Decider implements it.
type Challenger interface {
	Challenge() string
}

// APIKeyChallenge returns auth.api-key's challenge for the configured
// header name: APIKey header="<name>", the name as an RFC 9110
// quoted-string.
func APIKeyChallenge(header string) string {
	return `APIKey header=` + quote(header)
}

// APIKeyHeader returns the header name of an auth.api-key config.header:
// the value when set and non-empty, else DefaultAPIKeyHeader.
func APIKeyHeader(configured *string) string {
	if configured == nil || *configured == "" {
		return DefaultAPIKeyHeader
	}
	return *configured
}

// quote returns s as an RFC 9110 quoted-string: DQUOTE and backslash are
// escaped with a backslash.
func quote(s string) string {
	if !strings.ContainsAny(s, `"\`) {
		return `"` + s + `"`
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	b.WriteByte('"')
	for i := range len(s) {
		if c := s[i]; c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}
