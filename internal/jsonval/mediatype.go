// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import "strings"

// IsJSONMediaType reports whether a Content-Type value names JSON: the
// media type application/json, or any type/subtype whose subtype ends in
// "+json" after at least one character (such as application/problem+json),
// compared case-insensitively with surrounding whitespace and parameters
// ignored (07 req 44; 03 req 25). Type and subtype must be RFC 9110 tokens.
// It is the one JSON gate every body reader shares, so another decoder can
// plug in by media type later (07 section 8).
func IsJSONMediaType(contentType string) bool {
	mt := contentType
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	mt = strings.Trim(mt, " \t")
	slash := strings.IndexByte(mt, '/')
	if slash < 0 {
		return false
	}
	typ, sub := mt[:slash], mt[slash+1:]
	if !isToken(typ) || !isToken(sub) {
		return false
	}
	if strings.EqualFold(typ, "application") && strings.EqualFold(sub, "json") {
		return true
	}
	const suffix = "+json"
	return len(sub) > len(suffix) && strings.EqualFold(sub[len(sub)-len(suffix):], suffix)
}

// isToken reports a non-empty RFC 9110 token (1*tchar).
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}
