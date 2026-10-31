// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"errors"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/routematch"
)

// Errors of a computed path (package documentation, "Path target"). Their
// text never holds the path.
var (
	// errPathLength is a path over MaxPathBytes, computed or forwarded.
	errPathLength = errors.New("path longer than " + strconv.Itoa(MaxPathBytes) + " bytes")
	// errPathQuery is a path holding "?" or "#".
	errPathQuery = errors.New("path holds ? or #")
)

// checkPath returns the forwarded form of p, the string result of a set
// entry with target path: p holds no "?" or "#" and passes the rule of a
// path Ruralz sends in place of the client's, as a composition step's
// path does (routematch.CheckForwardPath: it starts with "/", has no "."
// or ".." segment in any spelling, and passes the hardening of the client
// path). The result is p normalized as the Router normalizes a client
// path (routematch.NormalizePath), and both p and the result are at most
// MaxPathBytes long.
func checkPath(p string) (string, error) {
	if len(p) > MaxPathBytes {
		return "", errPathLength
	}
	if strings.ContainsAny(p, "?#") {
		return "", errPathQuery
	}
	if err := routematch.CheckForwardPath(p); err != nil {
		return "", err
	}
	norm, err := routematch.NormalizePath(p)
	if err != nil {
		return "", err
	}
	if len(norm) > MaxPathBytes {
		return "", errPathLength
	}
	return norm, nil
}

// errMediaType is a contentType that is not an RFC 9110 media type.
var errMediaType = errors.New("not an RFC 9110 media type")

// checkMediaType reports whether s, a contentType, is an RFC 9110 media
// type (section 8.3.1) that a Policy may write as a field value
// (httpfield.CheckValue): type "/" subtype, both tokens, then parameters,
// each OWS ";" OWS and optionally a token name "=" and a token or
// quoted-string value.
func checkMediaType(s string) error {
	if err := httpfield.CheckValue(s); err != nil {
		return errMediaType
	}
	typ, rest, ok := strings.Cut(s, "/")
	if !ok || !httpfield.IsToken(typ) {
		return errMediaType
	}
	n := tokenLen(rest)
	if n == 0 {
		return errMediaType
	}
	rest = rest[n:]
	for rest != "" {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" || rest[0] != ';' {
			return errMediaType
		}
		rest = strings.TrimLeft(rest[1:], " \t")
		if rest == "" || rest[0] == ';' {
			continue
		}
		n = tokenLen(rest)
		if n == 0 || n == len(rest) || rest[n] != '=' {
			return errMediaType
		}
		rest = rest[n+1:]
		if rest != "" && rest[0] == '"' {
			n = quotedLen(rest)
		} else {
			n = tokenLen(rest)
		}
		if n == 0 {
			return errMediaType
		}
		rest = rest[n:]
	}
	return nil
}

// tokenLen returns the length of the RFC 9110 token s starts with.
func tokenLen(s string) int {
	n := 0
	for n < len(s) && httpfield.IsTokenChar(s[n]) {
		n++
	}
	return n
}

// quotedLen returns the length of the RFC 9110 quoted-string s starts
// with, 0 when it is not one: DQUOTE, then qdtext (HTAB, SP, %x21,
// %x23-5B, %x5D-7E, obs-text) or quoted-pair ("\" and HTAB, SP, VCHAR or
// obs-text), then DQUOTE.
func quotedLen(s string) int {
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			return i + 1
		case c == '\\':
			if i+1 >= len(s) || !quotedPairChar(s[i+1]) {
				return 0
			}
			i++
		case c == '\t' || c == ' ' || c == 0x21 || (c >= 0x23 && c <= 0x5B) || (c >= 0x5D && c <= 0x7E) || c >= 0x80:
		default:
			return 0
		}
	}
	return 0
}

// quotedPairChar reports a byte a quoted-pair may escape: HTAB, SP, VCHAR
// or obs-text.
func quotedPairChar(c byte) bool {
	return c == '\t' || c == ' ' || (c >= 0x21 && c <= 0x7E) || c >= 0x80
}
