// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// claims is a token payload decoded by jsonval: a JSON object whose
// members are map[string]any, []any, string, json.Number, bool or nil.
// Once verified it is the identity's auth.claims (expr.Value) and the
// input of Consumer binding (identity.Claims). It is never changed after
// parse, so any number of readers may share it.
type claims map[string]any

var (
	_ expr.Tree       = claims(nil)
	_ identity.Claims = claims(nil)
)

// StringClaim returns the top-level claim name when it is a JSON string.
func (c claims) StringClaim(name string) (string, bool) {
	s, ok := c[name].(string)
	return s, ok
}

// HasClaim reports whether the top-level claim name is present, null
// included.
func (c claims) HasClaim(name string) bool {
	_, ok := c[name]
	return ok
}

// IsNull reports false: the claims are an object.
func (c claims) IsNull() bool { return false }

// AppendBody appends the claims as JSON with members in ascending byte
// order of their names.
func (c claims) AppendBody(dst []byte) ([]byte, error) {
	return jsonval.Append(dst, map[string]any(c))
}

// Native returns a deep copy of the claims, so a caller that edits it
// never changes the identity.
func (c claims) Native() (any, error) { return jsonval.Clone(map[string]any(c)), nil }

// Tree returns the claims for reading in place (expr.Tree): CEL views them
// per evaluation without the copy Native makes, since they never change
// after parse.
func (c claims) Tree() any { return map[string]any(c) }

// numericDate reads a NumericDate claim (RFC 7519 section 2): a JSON
// number of seconds since the epoch, fractions allowed; ok is false for an
// absent claim, another type or a value outside the float64 range.
func numericDate(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// unixSeconds returns t in seconds since the epoch, with its fraction.
func unixSeconds(t time.Time) float64 {
	return float64(t.Unix()) + float64(t.Nanosecond())/1e9
}

// issuedAt returns the iat claim as a time for the revocation check; the
// zero time when it is absent, not a number or out of range ("iat is not
// checked").
func (c claims) issuedAt() time.Time {
	f, ok := numericDate(c["iat"])
	if !ok || f < -1e15 || f > 1e15 {
		return time.Time{}
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC()
}

// valid checks the registered claims of a verified token at now for an
// issuer accepting audiences: exp is required, not passed by more than
// ClockSkew and at most MaxExpiry ahead; nbf, when present, is a number
// not more than ClockSkew ahead; aud, a string or an array, holds one of
// audiences. iat is not checked. A failure is RZ-AUTH-003.
func (c claims) valid(audiences []string, now time.Time) bool {
	t := unixSeconds(now)
	skew := ClockSkew.Seconds()
	exp, ok := numericDate(c["exp"])
	if !ok || t >= exp+skew || exp-t > MaxExpiry.Seconds() {
		return false
	}
	if v, present := c["nbf"]; present {
		nbf, ok := numericDate(v)
		if !ok || t+skew < nbf {
			return false
		}
	}
	return audienceMatch(c["aud"], audiences)
}

// audienceMatch reports whether aud, a string or an array of strings,
// holds one of want (case-sensitive); array members that are not strings
// never match.
func audienceMatch(aud any, want []string) bool {
	switch a := aud.(type) {
	case string:
		return slices.Contains(want, a)
	case []any:
		for _, v := range a {
			if s, ok := v.(string); ok && slices.Contains(want, s) {
				return true
			}
		}
	}
	return false
}
