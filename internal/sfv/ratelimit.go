// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sfv

import "fmt"

// Canonical http.Header keys of the draft-ietf-httpapi-ratelimit-headers-11
// fields (textproto.CanonicalMIMEHeaderKey form, so assigning them to an
// http.Header map allocates no key). Field names are case-insensitive; the
// draft spells them RateLimit-Policy and RateLimit.
const (
	HeaderRateLimitPolicy = "Ratelimit-Policy"
	HeaderRateLimit       = "Ratelimit"
)

// AppendRateLimitPolicy appends one RateLimit-Policy List member to the
// field value in dst, preceded by ", " when dst is not empty (05 req 67):
// the Policy name as a String (".1", ".2" suffixed by the caller for a
// multi-limit Policy) with the parameters q = quota (requests, or the quota
// limit) and w = window in seconds (rounded up by the caller). quota and
// window must be non-negative Integers. For example
//
//	"ratelimit-gold.1";q=100;w=1
//
// On failure dst is returned unchanged with an error wrapping
// ErrSerialize. It allocates nothing when dst has room.
func AppendRateLimitPolicy(dst []byte, name string, quota, window int64) ([]byte, error) {
	return appendLimit(dst, name, 'q', quota, 'w', window)
}

// AppendRateLimit appends one RateLimit List member to the field value in
// dst, preceded by ", " when dst is not empty (05 req 67 and 68): the
// limit's name as a String with the parameters r = remaining and t =
// seconds until the limit resets or admits again. remaining and reset must
// be non-negative Integers. For example
//
//	"ratelimit-gold.1";r=0;t=1
//
// On failure dst is returned unchanged with an error wrapping
// ErrSerialize. It allocates nothing when dst has room.
func AppendRateLimit(dst []byte, name string, remaining, reset int64) ([]byte, error) {
	return appendLimit(dst, name, 'r', remaining, 't', reset)
}

// appendLimit appends `, "name";k1=v1;k2=v2` (without the separator for an
// empty dst).
func appendLimit(dst []byte, name string, k1 byte, v1 int64, k2 byte, v2 int64) ([]byte, error) {
	if v1 < 0 || v1 > MaxInteger {
		return dst, fmt.Errorf("%w: parameter %c=%d is not a non-negative integer", ErrSerialize, k1, v1)
	}
	if v2 < 0 || v2 > MaxInteger {
		return dst, fmt.Errorf("%w: parameter %c=%d is not a non-negative integer", ErrSerialize, k2, v2)
	}
	start := len(dst)
	if start > 0 {
		dst = append(dst, ", "...)
	}
	dst, err := appendString(dst, name)
	if err != nil {
		return dst[:start], err
	}
	dst, _ = appendInteger(append(dst, ';', k1, '='), v1)
	dst, _ = appendInteger(append(dst, ';', k2, '='), v2)
	return dst, nil
}
